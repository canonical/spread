package spread

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/niemeyer/pretty"
	"gopkg.in/yaml.v2"
)

func Linode(p *Project, b *Backend, o *Options) Provider {
	return &linodeProvider{
		project: p,
		backend: b,
		options: o,

		reserved: make(map[int]bool),
	}
}

type linodeProvider struct {
	project *Project
	backend *Backend
	options *Options

	mu sync.Mutex

	keyChecked bool
	keyErr     error
	reserved   map[int]bool

	templatesDone  bool
	templatesCache []*linodeTemplate
	kernelsCache   []*linodeKernel
}

var client = &http.Client{}

type linodeServer struct {
	p *linodeProvider
	d linodeServerData
}

type linodeServerData struct {
	ID      int    `json:"LINODEID"`
	Label   string `json:"LABEL"`
	Status  int    `json:"STATUS" yaml:"-"`
	Backend string `json:"-"`
	System  string `json:"-"`
	Address string `json:"-"`
	Config  int    `json:"-"`
	Root    int    `json:"-"`
	Swap    int    `json:"-"`
}

func (s *linodeServer) String() string {
	return fmt.Sprintf("%s:%s (%s)", s.p.backend.Name, s.d.System, s.d.Label)
}

func (s *linodeServer) Provider() Provider {
	return s.p
}

func (s *linodeServer) Address() string {
	return s.d.Address
}

func (s *linodeServer) System() *System {
	system := s.p.backend.Systems[s.d.System]
	if system == nil {
		return removedSystem(s.p.backend, s.d.System)
	}
	return system
}

func (s *linodeServer) ReuseData() []byte {
	data, err := yaml.Marshal(s.d)
	if err != nil {
		panic(err)
	}
	return data
}

const (
	linodeBeingCreated = -1
	linodeBrandNew     = 0
	linodeRunning      = 1
	linodePoweredOff   = 2
)

type linodeResult struct {
	Errors []linodeError `json:"ERRORARRAY"`
}

type linodeError struct {
	Code    int    `json:"ERRORCODE"`
	Message string `json:"ERRORMESSAGE"`
}

func (r *linodeResult) err() error {
	for _, e := range r.Errors {
		return fmt.Errorf("%s", strings.ToLower(string(e.Message[0]))+e.Message[1:])
	}
	return nil
}

func (p *linodeProvider) Backend() *Backend {
	return p.backend
}

func (p *linodeProvider) Reuse(data []byte) (Server, error) {
	if err := p.checkKey(); err != nil {
		return nil, err
	}

	s := &linodeServer{}
	err := yaml.Unmarshal(data, &s.d)
	if err != nil {
		return nil, fmt.Errorf("cannot unmarshal Linode reuse data: %v", err)
	}
	s.p = p
	return s, nil
}

func (p *linodeProvider) reserve(s *linodeServer) bool {
	p.mu.Lock()
	ok := false
	if !p.reserved[s.d.ID] {
		p.reserved[s.d.ID] = true
		ok = true
	}
	p.mu.Unlock()
	return ok
}

func (p *linodeProvider) unreserve(s *linodeServer) {
	p.mu.Lock()
	delete(p.reserved, s.d.ID)
	p.mu.Unlock()
}

func (p *linodeProvider) Allocate(system *System) (Server, error) {
	if err := p.checkKey(); err != nil {
		return nil, err
	}

	servers, err := p.list()
	if err != nil {
		return nil, err
	}
	if len(servers) == 0 {
		return nil, FatalError{fmt.Errorf("no servers in Linode account")}
	}
	// Iterate out of order to reduce conflicts.
	for _, i := range rnd.Perm(len(servers)) {
		s := servers[i]
		if (s.d.Status != linodeBrandNew && s.d.Status != linodePoweredOff) || !p.reserve(s) {
			continue
		}
		err := p.setup(s, system)
		if err != nil {
			p.unreserve(s)
			return nil, err
		}
		printf("Allocated %s.", s)
		return s, nil
	}
	return nil, fmt.Errorf("no powered off servers in Linode account")
}

func (s *linodeServer) Discard() error {
	_, err1 := s.p.shutdown(s)
	err2 := s.p.removeConfig(s, s.d.Config)
	err3 := s.p.removeDisks(s, s.d.Root, s.d.Swap)
	s.p.unreserve(s)
	return firstErr(err1, err2, err3)
}

type linodeListResult struct {
	linodeResult
	Data []linodeServerData `json:"DATA"`
}

func (p *linodeProvider) list() ([]*linodeServer, error) {
	debug("Listing available Linode servers...")
	params := linodeParams{
		"api_action": "linode.list",
	}
	var result linodeListResult
	err := p.do(params, &result)
	if err == nil {
		err = result.err()
	}
	if err != nil {
		return nil, err
	}
	servers := make([]*linodeServer, len(result.Data))
	for i, d := range result.Data {
		servers[i] = &linodeServer{p: p, d: d}
	}
	return servers, nil
}

func (p *linodeProvider) status(s *linodeServer) (int, error) {
	debugf("Checking power status of %s...", s)
	params := linodeParams{
		"api_action": "linode.list",
		"LinodeID":   s.d.ID,
	}
	var result linodeListResult
	err := p.do(params, &result)
	if err == nil {
		err = result.err()
	}
	if err != nil {
		return 0, err
	}
	if len(result.Data) == 0 {
		return 0, fmt.Errorf("cannot find %s data", s)
	}
	return result.Data[0].Status, nil
}

func (p *linodeProvider) setup(s *linodeServer, system *System) error {
	s.p = p
	s.d.System = system.Name
	s.d.Backend = p.backend.Name

	rootJob, swapJob, err := p.createDisk(s, system)
	if err != nil {
		return err
	}
	s.d.Root = rootJob.DiskID
	s.d.Swap = swapJob.DiskID

	if _, err := p.waitJob(s, "allocate disk", rootJob.JobID); err != nil {
		p.removeDisks(s, s.d.Root, s.d.Swap)
		return err
	}

	if status, err := p.status(s); err != nil {
		p.removeDisks(s, s.d.Root, s.d.Swap)
		return err
	} else if status != linodeBrandNew && status != linodePoweredOff {
		p.removeDisks(s, s.d.Root, s.d.Swap)
		return fmt.Errorf("server %s concurrently allocated, giving up on it.", s)
	}
	if conflict, err := p.hasRecentDisk(s, s.d.Root); err != nil {
		p.removeDisks(s, s.d.Root, s.d.Swap)
		return err
	} else if conflict {
		p.removeDisks(s, s.d.Root, s.d.Swap)
		return fmt.Errorf("server %s concurrently allocated, giving up on it.", s)
	}

	ip, err := p.ip(s)
	if err != nil {
		return err
	}
	s.d.Address = ip.IPAddress

	configID, err := p.createConfig(s, system, s.d.Root, s.d.Swap)
	if err != nil {
		p.removeDisks(s, s.d.Root, s.d.Swap)
		return err
	}
	s.d.Config = configID

	bootJob, err := p.boot(s, configID)
	if err == nil {
		_, err = p.waitJob(s, "boot", bootJob.JobID)
	}
	if err != nil {
		p.removeConfig(s, s.d.Config)
		p.removeDisks(s, s.d.Root, s.d.Swap)
		return err
	}

	return nil
}

type linodeSimpleJob struct {
	JobID int `json:"JOBID"`
}

type linodeSimpleJobResult struct {
	linodeResult
	Data *linodeSimpleJob `json:"DATA"`
}

func (p *linodeProvider) boot(s *linodeServer, configID int) (*linodeSimpleJob, error) {
	return p.simpleJob(s, "boot", linodeParams{
		"api_action": "linode.boot",
		"LinodeID":   s.d.ID,
		"ConfigID":   configID,
	})
}

func (p *linodeProvider) reboot(s *linodeServer, configID int) (*linodeSimpleJob, error) {
	return p.simpleJob(s, "reboot", linodeParams{
		"api_action": "linode.reboot",
		"LinodeID":   s.d.ID,
		"ConfigID":   configID,
	})
}

func (p *linodeProvider) shutdown(s *linodeServer) (*linodeSimpleJob, error) {
	return p.simpleJob(s, "shutdown", linodeParams{
		"api_action": "linode.shutdown",
		"LinodeID":   s.d.ID,
	})
}

func (p *linodeProvider) simpleJob(s *linodeServer, verb string, params linodeParams) (*linodeSimpleJob, error) {
	var result linodeSimpleJobResult
	err := p.do(params, &result)
	if err == nil {
		err = result.err()
	}
	if err != nil {
		return nil, fmt.Errorf("cannot %s %s: %v", verb, s, err)
	}
	return result.Data, nil
}

type linodeDiskJob struct {
	DiskID int `json:"DISKID"`
	JobID  int `json:"JOBID"`
}

type linodeDiskJobResult struct {
	linodeResult
	Data *linodeDiskJob `json:"DATA"`
}

func (p *linodeProvider) createDisk(s *linodeServer, system *System) (root, swap *linodeDiskJob, err error) {
	template, _, err := p.template(system)
	if err != nil {
		return nil, nil, err
	}

	createRoot := linodeParams{
		"LinodeID": s.d.ID,
		"Label":    SystemLabel(system, "root"),
		"Size":     4096,
		"rootPass": p.options.Password,
	}
	createSwap := linodeParams{
		"api_action": "linode.disk.create",
		"LinodeID":   s.d.ID,
		"Label":      SystemLabel(system, "swap"),
		"Size":       256,
		"Type":       "swap",
	}

	logf("Creating disk on %s with %s...", s, system.Name)
	params := linodeParams{
		"api_action":       "batch",
		"api_requestArray": []linodeParams{createRoot, createSwap},
	}

	if template.DistroID > 0 {
		createRoot["api_action"] = "linode.disk.createFromDistribution"
		createRoot["DistributionID"] = template.DistroID
	} else {
		createRoot["api_action"] = "linode.disk.createFromImage"
		createRoot["ImageID"] = template.ImageID
	}

	var results []linodeDiskJobResult
	err = p.do(params, &results)
	for i, result := range results {
		if err == nil {
			err = result.err()
		}
		if i == 0 {
			root = result.Data
		} else {
			swap = result.Data
		}
	}
	if len(results) == 0 || err == nil && (root == nil || swap == nil) {
		err = fmt.Errorf("empty batch result")
	}
	if err == nil {
		return root, swap, nil
	}
	if root != nil {
		p.removeDisks(s, root.DiskID)
	}
	if swap != nil {
		p.removeDisks(s, swap.DiskID)
	}
	return nil, nil, fmt.Errorf("cannot create Linode disk with %s: %v", system.Name, err)
}

func (p *linodeProvider) removeDisks(s *linodeServer, diskIDs ...int) error {
	logf("Removing disks from %s...", s)
	var batch []linodeParams
	for _, diskID := range diskIDs {
		batch = append(batch, linodeParams{
			"api_action": "linode.disk.delete",
			"LinodeID":   s.d.ID,
			"DiskID":     diskID,
		})
	}
	params := linodeParams{
		"api_action":       "batch",
		"api_requestArray": batch,
	}
	var results []linodeResult
	err := p.do(params, &results)
	if err != nil {
		return fmt.Errorf("cannot remove disk on %s: %v", s, err)
	}
	for _, result := range results {
		if err := result.err(); err != nil {
			return fmt.Errorf("cannot remove disk on %s: %v", s, err)
		}
	}
	return nil
}

func username() string {
	for _, name := range []string{"USER", "LOGNAME"} {
		if user := os.Getenv(name); user != "" {
			return user
		}
	}
	if home := os.Getenv("HOME"); home != "" {
		return filepath.Base(home)
	}
	return ""
}

type linodeConfigResult struct {
	linodeResult
	Data struct {
		ConfigID int `json:"CONFIGID"`
	} `json:"DATA"`
}

func (p *linodeProvider) createConfig(s *linodeServer, system *System, rootID, swapID int) (configID int, err error) {
	logf("Creating configuration on %s with %s...", s, system.Name)

	_, kernel, err := p.template(system)
	if err != nil {
		return 0, err
	}

	comments := fmt.Sprintf("USER=%q spread\n-pass=%q -reuse=%s -keep=%v", username(), p.options.Password, s.Address(), p.options.Keep)

	params := linodeParams{
		"api_action":             "linode.config.create",
		"LinodeID":               s.d.ID,
		"KernelID":               kernel.ID,
		"Label":                  SystemLabel(system, ""),
		"Comments":               comments,
		"DiskList":               fmt.Sprintf("%d,%d", rootID, swapID),
		"RootDeviceNum":          1,
		"RootDeviceR0":           true,
		"helper_disableUpdateDB": true,
		"helper_distro":          true,
		"helper_depmod":          true,
		"helper_network":         false,
		"devtmpfs_automount":     true,
	}

	var result linodeConfigResult
	err = p.do(params, &result)
	if err == nil {
		err = result.err()
	}
	if err != nil {
		return 0, fmt.Errorf("cannot create config on %s with %s: %v", s, system.Name, err)
	}
	return result.Data.ConfigID, nil
}

func (p *linodeProvider) removeConfig(s *linodeServer, configID int) error {
	logf("Removing configuration from %s...", s)

	params := linodeParams{
		"api_action": "linode.config.delete",
		"LinodeID":   s.d.ID,
		"ConfigID":   configID,
	}
	var result linodeResult
	err := p.do(params, &result)
	if err == nil {
		err = result.err()
	}
	if err != nil {
		return fmt.Errorf("cannot remove config from %s: %v", s, err)
	}
	return nil
}

type linodeJob struct {
	JobID        int    `json:"JOBID"`
	LinodeID     int    `json:"LINODEID"`
	Action       string `json:"ACTION"`
	Label        string `json:"LABEL"`
	HostMessage  string `json:"HOST_MESSAGE"`
	HostStartDT  string `json:"HOST_START_DT"`
	HostFinishDT string `json:"HOST_FINISH_DT"`
	EnteredDT    string `json:"ENTERED_DT"`

	// Linode bug: sometimes "", generally 1/0, see https://goo.gl/fxVyaz.
	HostSuccess interface{} `json:"HOST_SUCCESS"`
}

func (job *linodeJob) err() error {
	if job.HostSuccess == 1.0 || job.HostFinishDT == "" {
		return nil
	}
	if msg := job.HostMessage; msg != "" {
		return fmt.Errorf("%s", strings.ToLower(string(msg[0]))+msg[1:])
	}
	return fmt.Errorf("job %d failed silently", job.JobID)
}

type linodeJobResult struct {
	linodeResult
	Data []*linodeJob `json:"DATA"`
}

func (p *linodeProvider) job(s *linodeServer, jobID int) (*linodeJob, error) {
	params := linodeParams{
		"api_action": "linode.job.list",
		"LinodeID":   s.d.ID,
		"JobID":      jobID,
	}
	var result linodeJobResult
	err := p.do(params, &result)
	if err == nil {
		err = result.err()
	}
	if err == nil && len(result.Data) == 0 {
		err = fmt.Errorf("empty result")
	}
	if err != nil {
		return nil, fmt.Errorf("cannot get job details for %s: %v", s, err)
	}
	return result.Data[0], nil
}

func (p *linodeProvider) waitJob(s *linodeServer, verb string, jobID int) (*linodeJob, error) {
	logf("Waiting for %s to %s...", s, verb)

	// Used to be 1 min up to Aug 2016, but disk allocation timeouts were frequently observed.
	timeout := time.After(3 * time.Minute)
	retry := time.NewTicker(5 * time.Second)
	defer retry.Stop()

	var infoErr error
	for {
		select {
		case <-timeout:
			// Don't shutdown. The machine may be running something else.
			if infoErr != nil {
				return nil, infoErr
			}
			p.removeConfig(s, s.d.Config)
			p.removeDisks(s, s.d.Root, s.d.Swap)
			return nil, fmt.Errorf("timeout waiting for %s to %s", s, verb)

		case <-retry.C:
			job, err := p.job(s, jobID)
			if err != nil {
				infoErr = fmt.Errorf("cannot %s %s: %s", verb, s, err)
				break
			}
			if job.HostFinishDT != "" {
				err := job.err()
				if err != nil {
					err = fmt.Errorf("cannot %s %s: %s", verb, s, err)
				}
				return job, err
			}
		}
	}
	panic("unreachable")
}

type linodeDisk struct {
	UpdateDT   string `json:"UPDATE_DT"`
	Type       string `json:"TYPE"`
	Status     int    `json:"STATUS"`
	DiskID     int    `json:"DISKID"`
	CreateDT   string `json:"CREATE_DT"`
	IsReadOnly int    `josn:"ISREADONLY"`
	LinodeID   int    `json:"LINODEID"`
	Label      string `json:"LABEL"`
	Size       int    `json:"SIZE"`
}

func (d *linodeDisk) Created() time.Time {
	return parseLinodeDT(d.CreateDT)
}

type linodeDiskResult struct {
	linodeResult
	Data []*linodeDisk `json:"DATA"`
}

func (p *linodeProvider) disks(s *linodeServer) ([]*linodeDisk, error) {
	params := linodeParams{
		"api_action": "linode.disk.list",
		"LinodeID":   s.d.ID,
	}
	var result linodeDiskResult
	err := p.do(params, &result)
	if err == nil {
		err = result.err()
	}
	if err != nil {
		return nil, err
	}
	return result.Data, nil
}

// hasRecentDisk returns an error if there's a disk that is ready and
// was created up to a minute before the provided disk ID. If two clients
// use this logic, the most recent one will concede the machine usage to
// the oldest one. We need this hack because operations in Linode are not
// atomic, and a server will happily boot a second time on a different
// configuration overriding a recent boot.
func (p *linodeProvider) hasRecentDisk(s *linodeServer, diskID int) (bool, error) {
	logf("Checking %s for allocation conflict...", s)
	disks, err := p.disks(s)
	if err != nil {
		return false, fmt.Errorf("cannot check %s for allocation conflict: %v", s, err)
	}

	var limit time.Time
	for _, d := range disks {
		if d.DiskID == diskID {
			limit = d.Created()
		}
	}
	if limit.IsZero() {
		return false, fmt.Errorf("cannot check %s for allocation conflict: disk %d not found", s, diskID)
	}
	for _, d := range disks {
		t := d.Created()
		if t.Before(limit) && t.After(limit.Add(-time.Minute)) {
			return true, nil
		}
	}
	return false, nil
}

type linodeIPResult struct {
	linodeResult
	Data []*linodeIP `json:"DATA"`
}

type linodeIP struct {
	ID        int    `json:"IPADDRESSID"`
	LinodeID  int    `json:"LINODEID"`
	IsPublic  int    `json:"ISPUBLIC"`
	IPAddress string `json:"IPADDRESS"`
	RDNSName  string `json:"RDNS_NAME"`
}

func (p *linodeProvider) ip(s *linodeServer) (*linodeIP, error) {
	logf("Obtaining address of %s...", s)

	params := linodeParams{
		"api_action": "linode.ip.list",
		"LinodeID":   s.d.ID,
	}
	var result linodeIPResult
	err := p.do(params, &result)
	if err != nil {
		return nil, err
	}
	if err := result.err(); err != nil {
		return nil, fmt.Errorf("cannot list IPs for %s: %v", s, err)
	}
	for _, ip := range result.Data {
		if ip.IsPublic == 1 {
			logf("Got address of %s: %s", s, ip.IPAddress)
			return ip, nil
		}
	}
	return nil, fmt.Errorf("cannot find public IP for %s", s)
}

type linodeTemplateResult struct {
	linodeResult
	Data []*linodeTemplate
}

type linodeTemplate struct {
	Name   string        `json:"-"`
	Kernel *linodeKernel `json:"-"`

	DistroID     int    `json:"DISTRIBUTIONID"`
	ImageID      int    `json:"IMAGEID"`
	Label        string `json:"LABEL"`
	MinImageSize int    `json:"MINIMAGESIZE"`
	VOPSKernel   int    `json:"REQUIRESVOPSKERNEL"`
	Is64Bit      int    `json:"IS64BIT"`
	Create       string `json:"CREATE_DT"`
}

type linodeKernelResult struct {
	linodeResult
	Data []*linodeKernel
}

type linodeKernel struct {
	ID      int    `json:"KERNELID"`
	IsPVOPS int    `json:"ISPVOPS"`
	IsXEN   int    `json:"ISXEN"`
	IsKVM   int    `json:"ISKVM"`
	Label   string `json:"LABEL"`
}

// Not listed for some reason.
var linodeKernels = []*linodeKernel{{
	ID:    210,
	Label: "GRUB 2",
	IsKVM: 1,
}, {
	ID:    213,
	Label: "Direct Disk",
	IsKVM: 1,
}}

func (p *linodeProvider) template(system *System) (*linodeTemplate, *linodeKernel, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.templatesDone {
		if err := p.cacheTemplates(); err != nil {
			return nil, nil, err
		}
	}
	p.templatesDone = true

	wantImage := strings.ToLower(system.Image)
	wantPrefix := wantImage + " "

	var best *linodeTemplate
	for _, template := range p.templatesCache {
		label := strings.ToLower(template.Label)
		if template.Name != system.Image && label != wantImage && !strings.HasPrefix(label, wantPrefix) {
			continue
		}
		best = template
		if template.ImageID > 0 || template.Is64Bit == 1 {
			break
		}
	}
	if best == nil {
		return nil, nil, &FatalError{fmt.Errorf("no Linode image or distribution for %q", system.Image)}
	}
	if system.Kernel != "" {
		wantKernel := strings.ToLower(system.Kernel)
		wantPrefix := wantKernel + " "
		for _, kernel := range p.kernelsCache {
			label := strings.ToLower(kernel.Label)
			if label == wantKernel || strings.HasPrefix(label, wantPrefix) {
				return best, kernel, nil
			}
		}
		return nil, nil, &FatalError{fmt.Errorf("no %q Linode kernel", system.Kernel)}
	}
	return best, best.Kernel, nil
}

func (p *linodeProvider) cacheTemplates() error {
	var err error
	for retry := 0; retry < 3; retry++ {
		params := linodeParams{
			"api_action": "avail.distributions",
		}
		var result linodeTemplateResult
		err = p.do(params, &result)
		if err == nil {
			err = result.err()
		}
		if err == nil {
			p.templatesCache = result.Data
			break
		}
	}
	if err != nil {
		return fmt.Errorf("cannot list Linode distributions: %v", err)
	}
	for retry := 0; retry < 3; retry++ {
		params := linodeParams{
			"api_action": "image.list",
		}
		var result linodeTemplateResult
		err = p.do(params, &result)
		if err == nil {
			err = result.err()
		}
		if err == nil {
			p.templatesCache = append(p.templatesCache, result.Data...)
			break
		}
	}
	if err != nil {
		return fmt.Errorf("cannot list Linode images: %v", err)
	}
	for retry := 0; retry < 3; retry++ {
		params := linodeParams{
			"api_action": "avail.kernels",
		}
		var result linodeKernelResult
		err = p.do(params, &result)
		if err == nil {
			err = result.err()
		}
		if err == nil {
			p.kernelsCache = result.Data
			break
		}
	}
	if err != nil {
		return fmt.Errorf("cannot list Linode kernels: %v", err)
	}

	p.kernelsCache = append(p.kernelsCache, linodeKernels...)

	var latest32, latest64 *linodeKernel
	for _, kernel := range p.kernelsCache {
		if strings.HasPrefix(kernel.Label, "Latest 64 bit") {
			latest64 = kernel
		}
		if strings.HasPrefix(kernel.Label, "Latest 32 bit") {
			latest32 = kernel
		}
	}
	if latest32 == nil || latest64 == nil {
		return fmt.Errorf("cannot find latest Linode kernel")
	}
	for _, template := range p.templatesCache {
		label := strings.Fields(strings.ToLower(template.Label))
		if len(label) > 2 && label[1] == "linux" {
			template.Name = label[0] + "-" + label[2]
		} else if len(label) > 1 {
			template.Name = label[0] + "-" + label[1]
		} else if len(label) > 0 {
			template.Name = label[0]
		}

		if strings.HasSuffix(template.Name, "-grub") {
			// Deprecated logic. Drop after a short while.
			template.Kernel = linodeKernels[0]
		} else if template.Is64Bit == 1 || template.ImageID > 0 {
			template.Kernel = latest64
		} else {
			template.Kernel = latest32
		}
	}

	debugf("Linode distributions available: %# v", p.templatesCache)
	return nil
}

func (p *linodeProvider) checkKey() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.keyChecked {
		return p.keyErr
	}

	var result linodeResult
	err := p.do(linodeParams{"api_action": "test.echo"}, &result)
	if err == nil {
		err = result.err()
	}
	if err != nil {
		err = &FatalError{err}
	}

	p.keyChecked = true
	p.keyErr = err
	return err
}

type linodeParams map[string]interface{}

func (p *linodeProvider) do(params linodeParams, result interface{}) error {
	debugf("Linode request: %# v\n", params)

	values := make(url.Values)
	for k, v := range params {
		var vs string
		switch v := v.(type) {
		case int:
			vs = strconv.Itoa(v)
		case string:
			vs = v
		default:
			data, err := json.Marshal(v)
			if err != nil {
				return fmt.Errorf("cannot marshal Linode request parameter %q: %s", k, err)
			}
			vs = string(data)
		}
		values[k] = []string{vs}
	}
	values["api_key"] = []string{p.backend.Key}

	resp, err := client.PostForm("https://api.linode.com", values)
	if err != nil {
		return fmt.Errorf("cannot perform Linode request: %v", err)
	}
	defer resp.Body.Close()

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("cannot read Linode response: %v", err)
	}

	if Debug {
		var r interface{}
		if err := json.Unmarshal(data, &r); err == nil {
			debugf("Linode response: %# v\n", r)
		}
	}

	err = json.Unmarshal(data, result)
	if err != nil {
		info := pretty.Sprintf("Request:\n-----\n%# v\n-----\nResponse:\n-----\n%s\n-----\n", params, data)
		return fmt.Errorf("cannot decode Linode response: %s\n%s", err, info)
	}
	return nil
}

func parseLinodeDT(dt string) time.Time {
	if dt != "" {
		t, err := time.Parse("2006-01-02 15:04:05.0", dt)
		if err == nil {
			return t
		}
		printf("WARNING: Cannot parse Linode date/time string: %q", dt)
	}
	return time.Time{}
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
