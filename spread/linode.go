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

	"github.com/kr/pretty"
	"gopkg.in/yaml.v2"
)

func Linode(b *Backend) Provider {
	return &linode{
		backend:  b,
		reserved: make(map[int]bool),
	}
}

type linode struct {
	backend *Backend

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
	l *linode
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
	return fmt.Sprintf("%s:%s (%s)", s.l.backend.Name, s.d.System, s.d.Label)
}

func (s *linodeServer) Provider() Provider {
	return s.l
}

func (s *linodeServer) Address() string {
	return s.d.Address
}

func (s *linodeServer) System() *System {
	system := s.l.backend.Systems[s.d.System]
	if system == nil {
		return removedSystem(s.l.backend, s.d.System)
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

func (l *linode) Backend() *Backend {
	return l.backend
}

func (l *linode) Reuse(data []byte, password string) (Server, error) {
	if err := l.checkKey(); err != nil {
		return nil, err
	}

	server := &linodeServer{}
	err := yaml.Unmarshal(data, &server.d)
	if err != nil {
		return nil, fmt.Errorf("cannot unmarshal Linode reuse data: %v", err)
	}
	server.l = l
	return server, nil
}

func (l *linode) reserve(server *linodeServer) bool {
	l.mu.Lock()
	ok := false
	if !l.reserved[server.d.ID] {
		l.reserved[server.d.ID] = true
		ok = true
	}
	l.mu.Unlock()
	return ok
}

func (l *linode) unreserve(server *linodeServer) {
	l.mu.Lock()
	delete(l.reserved, server.d.ID)
	l.mu.Unlock()
}

func (l *linode) Allocate(system *System, password string, keep bool) (Server, error) {
	if err := l.checkKey(); err != nil {
		return nil, err
	}

	servers, err := l.list()
	if err != nil {
		return nil, err
	}
	if len(servers) == 0 {
		return nil, FatalError{fmt.Errorf("no servers in Linode account")}
	}
	// Iterate out of order to reduce conflicts.
	for _, i := range rnd.Perm(len(servers)) {
		server := servers[i]
		if (server.d.Status != linodeBrandNew && server.d.Status != linodePoweredOff) || !l.reserve(server) {
			continue
		}
		err := l.setup(server, system, password, keep)
		if err != nil {
			l.unreserve(server)
			return nil, err
		}
		printf("Allocated %s.", server)
		return server, nil
	}
	return nil, fmt.Errorf("no powered off servers in Linode account")
}

func (s *linodeServer) Discard() error {
	_, err1 := s.l.shutdown(s)
	err2 := s.l.removeConfig(s, s.d.Config)
	err3 := s.l.removeDisks(s, s.d.Root, s.d.Swap)
	s.l.unreserve(s)
	return firstErr(err1, err2, err3)
}

type linodeListResult struct {
	linodeResult
	Data []linodeServerData `json:"DATA"`
}

func (l *linode) list() ([]*linodeServer, error) {
	debug("Listing available Linode servers...")
	params := linodeParams{
		"api_action": "linode.list",
	}
	var result linodeListResult
	err := l.do(params, &result)
	if err == nil {
		err = result.err()
	}
	if err != nil {
		return nil, err
	}
	servers := make([]*linodeServer, len(result.Data))
	for i, d := range result.Data {
		servers[i] = &linodeServer{l: l, d: d}
	}
	return servers, nil
}

func (l *linode) status(server *linodeServer) (int, error) {
	debugf("Checking power status of %s...", server)
	params := linodeParams{
		"api_action": "linode.list",
		"LinodeID":   server.d.ID,
	}
	var result linodeListResult
	err := l.do(params, &result)
	if err == nil {
		err = result.err()
	}
	if err != nil {
		return 0, err
	}
	if len(result.Data) == 0 {
		return 0, fmt.Errorf("cannot find %s data", server)
	}
	return result.Data[0].Status, nil
}

func (l *linode) setup(server *linodeServer, system *System, password string, keep bool) error {
	server.l = l
	server.d.System = system.Name
	server.d.Backend = l.backend.Name

	rootJob, swapJob, err := l.createDisk(server, system, password)
	if err != nil {
		return err
	}
	server.d.Root = rootJob.DiskID
	server.d.Swap = swapJob.DiskID

	if _, err := l.waitJob(server, "allocate disk", rootJob.JobID); err != nil {
		l.removeDisks(server, server.d.Root, server.d.Swap)
		return err
	}

	if status, err := l.status(server); err != nil {
		l.removeDisks(server, server.d.Root, server.d.Swap)
		return err
	} else if status != linodeBrandNew && status != linodePoweredOff {
		l.removeDisks(server, server.d.Root, server.d.Swap)
		return fmt.Errorf("server %s concurrently allocated, giving up on it.", server)
	}
	if conflict, err := l.hasRecentDisk(server, server.d.Root); err != nil {
		l.removeDisks(server, server.d.Root, server.d.Swap)
		return err
	} else if conflict {
		l.removeDisks(server, server.d.Root, server.d.Swap)
		return fmt.Errorf("server %s concurrently allocated, giving up on it.", server)
	}

	ip, err := l.ip(server)
	if err != nil {
		return err
	}
	server.d.Address = ip.IPAddress

	configID, err := l.createConfig(server, system, password, keep, server.d.Root, server.d.Swap)
	if err != nil {
		l.removeDisks(server, server.d.Root, server.d.Swap)
		return err
	}
	server.d.Config = configID

	bootJob, err := l.boot(server, configID)
	if err == nil {
		_, err = l.waitJob(server, "boot", bootJob.JobID)
	}
	if err != nil {
		l.removeConfig(server, server.d.Config)
		l.removeDisks(server, server.d.Root, server.d.Swap)
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

func (l *linode) boot(server *linodeServer, configID int) (*linodeSimpleJob, error) {
	return l.simpleJob(server, "boot", linodeParams{
		"api_action": "linode.boot",
		"LinodeID":   server.d.ID,
		"ConfigID":   configID,
	})
}

func (l *linode) reboot(server *linodeServer, configID int) (*linodeSimpleJob, error) {
	return l.simpleJob(server, "reboot", linodeParams{
		"api_action": "linode.reboot",
		"LinodeID":   server.d.ID,
		"ConfigID":   configID,
	})
}

func (l *linode) shutdown(server *linodeServer) (*linodeSimpleJob, error) {
	return l.simpleJob(server, "shutdown", linodeParams{
		"api_action": "linode.shutdown",
		"LinodeID":   server.d.ID,
	})
}

func (l *linode) simpleJob(server *linodeServer, verb string, params linodeParams) (*linodeSimpleJob, error) {
	var result linodeSimpleJobResult
	err := l.do(params, &result)
	if err == nil {
		err = result.err()
	}
	if err != nil {
		return nil, fmt.Errorf("cannot %s %s: %v", verb, server, err)
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

func (l *linode) createDisk(server *linodeServer, system *System, password string) (root, swap *linodeDiskJob, err error) {
	template, _, err := l.template(system)
	if err != nil {
		return nil, nil, err
	}

	createRoot := linodeParams{
		"LinodeID": server.d.ID,
		"Label":    SystemLabel(system, "root"),
		"Size":     4096,
		"rootPass": password,
	}
	createSwap := linodeParams{
		"api_action": "linode.disk.create",
		"LinodeID":   server.d.ID,
		"Label":      SystemLabel(system, "swap"),
		"Size":       256,
		"Type":       "swap",
	}

	logf("Creating disk on %s with %s...", server, system.Name)
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
	err = l.do(params, &results)
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
		l.removeDisks(server, root.DiskID)
	}
	if swap != nil {
		l.removeDisks(server, swap.DiskID)
	}
	return nil, nil, fmt.Errorf("cannot create Linode disk with %s: %v", system.Name, err)
}

func (l *linode) removeDisks(server *linodeServer, diskIDs ...int) error {
	logf("Removing disks from %s...", server)
	var batch []linodeParams
	for _, diskID := range diskIDs {
		batch = append(batch, linodeParams{
			"api_action": "linode.disk.delete",
			"LinodeID":   server.d.ID,
			"DiskID":     diskID,
		})
	}
	params := linodeParams{
		"api_action":       "batch",
		"api_requestArray": batch,
	}
	var results []linodeResult
	err := l.do(params, &results)
	if err != nil {
		return fmt.Errorf("cannot remove disk on %s: %v", server, err)
	}
	for _, result := range results {
		if err := result.err(); err != nil {
			return fmt.Errorf("cannot remove disk on %s: %v", server, err)
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

func (l *linode) createConfig(server *linodeServer, system *System, password string, keep bool, rootID, swapID int) (configID int, err error) {
	logf("Creating configuration on %s with %s...", server, system.Name)

	_, kernel, err := l.template(system)
	if err != nil {
		return 0, err
	}

	comments := fmt.Sprintf("USER=%q spread\n-pass=%q -reuse=%s -keep=%v", username(), password, server.Address(), keep)

	params := linodeParams{
		"api_action":             "linode.config.create",
		"LinodeID":               server.d.ID,
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
	err = l.do(params, &result)
	if err == nil {
		err = result.err()
	}
	if err != nil {
		return 0, fmt.Errorf("cannot create config on %s with %s: %v", server, system.Name, err)
	}
	return result.Data.ConfigID, nil
}

func (l *linode) removeConfig(server *linodeServer, configID int) error {
	logf("Removing configuration from %s...", server)

	params := linodeParams{
		"api_action": "linode.config.delete",
		"LinodeID":   server.d.ID,
		"ConfigID":   configID,
	}
	var result linodeResult
	err := l.do(params, &result)
	if err == nil {
		err = result.err()
	}
	if err != nil {
		return fmt.Errorf("cannot remove config from %s: %v", server, err)
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

func (l *linode) job(server *linodeServer, jobID int) (*linodeJob, error) {
	params := linodeParams{
		"api_action": "linode.job.list",
		"LinodeID":   server.d.ID,
		"JobID":      jobID,
	}
	var result linodeJobResult
	err := l.do(params, &result)
	if err == nil {
		err = result.err()
	}
	if err == nil && len(result.Data) == 0 {
		err = fmt.Errorf("empty result")
	}
	if err != nil {
		return nil, fmt.Errorf("cannot get job details for %s: %v", server, err)
	}
	return result.Data[0], nil
}

func (l *linode) waitJob(server *linodeServer, verb string, jobID int) (*linodeJob, error) {
	logf("Waiting for %s to %s...", server, verb)

	timeout := time.After(1 * time.Minute)
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
			l.removeConfig(server, server.d.Config)
			l.removeDisks(server, server.d.Root, server.d.Swap)
			return nil, fmt.Errorf("timeout waiting for %s to %s", server, verb)

		case <-retry.C:
			job, err := l.job(server, jobID)
			if err != nil {
				infoErr = fmt.Errorf("cannot %s %s: %s", verb, server, err)
				break
			}
			if job.HostFinishDT != "" {
				err := job.err()
				if err != nil {
					err = fmt.Errorf("cannot %s %s: %s", verb, server, err)
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

func (l *linode) disks(server *linodeServer) ([]*linodeDisk, error) {
	params := linodeParams{
		"api_action": "linode.disk.list",
		"LinodeID":   server.d.ID,
	}
	var result linodeDiskResult
	err := l.do(params, &result)
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
func (l *linode) hasRecentDisk(server *linodeServer, diskID int) (bool, error) {
	logf("Checking %s for allocation conflict...", server)
	disks, err := l.disks(server)
	if err != nil {
		return false, fmt.Errorf("cannot check %s for allocation conflict: %v", server, err)
	}

	var limit time.Time
	for _, d := range disks {
		if d.DiskID == diskID {
			limit = d.Created()
		}
	}
	if limit.IsZero() {
		return false, fmt.Errorf("cannot check %s for allocation conflict: disk %d not found", server, diskID)
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

func (l *linode) ip(server *linodeServer) (*linodeIP, error) {
	logf("Obtaining address of %s...", server)

	params := linodeParams{
		"api_action": "linode.ip.list",
		"LinodeID":   server.d.ID,
	}
	var result linodeIPResult
	err := l.do(params, &result)
	if err != nil {
		return nil, err
	}
	if err := result.err(); err != nil {
		return nil, fmt.Errorf("cannot list IPs for %s: %v", server, err)
	}
	for _, ip := range result.Data {
		if ip.IsPublic == 1 {
			logf("Got address of %s: %s", server, ip.IPAddress)
			return ip, nil
		}
	}
	return nil, fmt.Errorf("cannot find public IP for %s", server)
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

func (l *linode) template(system *System) (*linodeTemplate, *linodeKernel, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.templatesDone {
		if err := l.cacheTemplates(); err != nil {
			return nil, nil, err
		}
	}
	l.templatesDone = true

	wantImage := strings.ToLower(system.Image)
	wantPrefix := wantImage + " "

	var best *linodeTemplate
	for _, template := range l.templatesCache {
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
		for _, kernel := range l.kernelsCache {
			label := strings.ToLower(kernel.Label)
			if label == wantKernel || strings.HasPrefix(label, wantPrefix) {
				return best, kernel, nil
			}
		}
		return nil, nil, &FatalError{fmt.Errorf("no %q Linode kernel", system.Kernel)}
	}
	return best, best.Kernel, nil
}

func (l *linode) cacheTemplates() error {
	var err error
	for retry := 0; retry < 3; retry++ {
		params := linodeParams{
			"api_action": "avail.distributions",
		}
		var result linodeTemplateResult
		err = l.do(params, &result)
		if err == nil {
			err = result.err()
		}
		if err == nil {
			l.templatesCache = result.Data
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
		err = l.do(params, &result)
		if err == nil {
			err = result.err()
		}
		if err == nil {
			l.templatesCache = append(l.templatesCache, result.Data...)
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
		err = l.do(params, &result)
		if err == nil {
			err = result.err()
		}
		if err == nil {
			l.kernelsCache = result.Data
			break
		}
	}
	if err != nil {
		return fmt.Errorf("cannot list Linode kernels: %v", err)
	}

	l.kernelsCache = append(l.kernelsCache, linodeKernels...)

	var latest32, latest64 *linodeKernel
	for _, kernel := range l.kernelsCache {
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
	for _, template := range l.templatesCache {
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

	debugf("Linode distributions available: %# v", l.templatesCache)
	return nil
}

func (l *linode) checkKey() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.keyChecked {
		return l.keyErr
	}

	var result linodeResult
	err := l.do(linodeParams{"api_action": "test.echo"}, &result)
	if err == nil {
		err = result.err()
	}
	if err != nil {
		err = &FatalError{err}
	}

	l.keyChecked = true
	l.keyErr = err
	return err
}

type linodeParams map[string]interface{}

func (l *linode) do(params linodeParams, result interface{}) error {
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
	values["api_key"] = []string{l.backend.Key}

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
