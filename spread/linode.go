package spread

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/context"

	"github.com/niemeyer/pretty"

	"gopkg.in/tomb.v2"
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

	listCache struct {
		mu   sync.Mutex
		next time.Time
		data []*linodeServer
	}

	planCache struct {
		mu   sync.Mutex
		data []*linodePlan
	}

	locationCache struct {
		mu   sync.Mutex
		data []*linodeLocation
	}
}

var httpClient = &http.Client{}

type linodeServer struct {
	p *linodeProvider
	d linodeServerData

	system  *System
	address string

	watchTomb tomb.Tomb
}

type linodeServerData struct {
	ID       int    `json:"LINODEID"`
	Label    string `json:"LABEL"`
	Group    string `json:"LPM_DISPLAYGROUP"`
	Status   int    `json:"STATUS" yaml:"-"`
	CreateDT string `json:"CREATE_DT" yaml:"created"`
	// Plan should be an int but may be an empty string due to bugs in Linode's end.
	Plan   interface{} `json:"PLANID"`
	Config int         `json:"-"`
	Root   int         `json:"-"`
	Swap   int         `json:"-"`
}

var (
	linodeDefaultLabel   = regexp.MustCompile("^linode[0-9]+$")
	linodeSpreadLabel    = regexp.MustCompile("^Spread-[0-9]+$")
	linodeEphemeralGroup = "# Spread Ephemeral"
)

func (s *linodeServer) brandNew() bool {
	return s.d.Status == linodeBrandNew && s.d.Group == "" && linodeDefaultLabel.MatchString(s.d.Label)
}

func (s *linodeServer) created() time.Time {
	return parseLinodeDT(s.d.CreateDT)
}

func (s *linodeServer) ephemeral() bool {
	return s.d.Group == linodeEphemeralGroup
}

func (s *linodeServer) plan() int {
	if id, ok := s.d.Plan.(int); ok {
		return id
	}
	return -1
}

func (s *linodeServer) String() string {
	if s.system == nil {
		return s.d.Label
	}
	return fmt.Sprintf("%s (%s)", s.system, s.d.Label)
}

func (s *linodeServer) Label() string {
	return s.d.Label
}

func (s *linodeServer) Provider() Provider {
	return s.p
}

func (s *linodeServer) Address() string {
	return s.address
}

func (s *linodeServer) System() *System {
	return s.system
}

func (s *linodeServer) ReuseData() interface{} {
	return &s.d
}

func (s *linodeServer) watch() {
	s.watchTomb.Go(s.watchLoop)
}

func (s *linodeServer) watchLoop() error {
	retry := time.NewTicker(5 * time.Second)
	defer retry.Stop()
	for s.watchTomb.Alive() {
		select {
		case <-s.watchTomb.Dying():
			return nil
		case <-retry.C:
			status, err := s.p.status(s)
			if err == nil && status == linodePoweredOff {
				found, _ := s.p.hasActiveJob(s, "linode.boot", noLog)
				if found {
					continue
				}
				printf("Found %s powered off. Starting it again.", s)
				_, err := s.p.boot(s, s.d.Config)
				if err != nil {
					printf("Cannot boot %s: %s", s, err)
				}
			}
		}
	}
	return nil
}

const (
	linodeBeingCreated = -1
	linodeBrandNew     = 0
	linodeRunning      = 1
	linodePoweredOff   = 2
)

func (p *linodeProvider) Backend() *Backend {
	return p.backend
}

func (p *linodeProvider) Reuse(ctx context.Context, rsystem *ReuseSystem, system *System) (Server, error) {
	s := &linodeServer{
		p:       p,
		address: rsystem.Address,
		system:  system,
	}
	err := rsystem.UnmarshalData(&s.d)
	if err != nil {
		return nil, fmt.Errorf("cannot unmarshal Linode reuse data: %v", err)
	}
	s.watch()
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

func (p *linodeProvider) Allocate(ctx context.Context, system *System) (Server, error) {
	if err := p.checkKey(); err != nil {
		return nil, err
	}

	servers, err := p.list()
	if err != nil {
		return nil, err
	}

	// HACK HACK HACK - Force snapd to use 4GB systems.
	if p.backend.HaltTimeout.Duration == 2*time.Hour {
		p.backend.Location = "fremont"
		p.backend.Plan = "2GB"
	}

	plan := 0
	if p.backend.Plan != "" {
		plan, err = p.planID(p.backend.Plan)
		if err != nil {
			return nil, err
		}
	}

	// Iterate out of order to reduce conflicts.
	perm := rnd.Perm(len(servers))
	activity := make([]time.Time, len(servers))
	adopt := time.Now().Add(-30 * time.Second)
	for _, i := range perm {
		s := servers[i]
		if plan > 0 && s.plan() != plan {
			continue
		}
		if (s.d.Status != linodeBrandNew && s.d.Status != linodePoweredOff) || !p.reserve(s) {
			continue
		}

		when, err := p.recentActivity(s)
		if err != nil {
			printf("Cannot check %s for activity: %v", s, err)
			continue
		}

		activity[i] = when
		if when.After(adopt) {
			continue
		}

		printf("Configuring spare Linode server %s with %s...", s, system.Name)

		err = p.setup(s, system)
		if err != nil {
			p.unreserve(s)
			return nil, err
		}
		printf("Allocated %s.", s)
		s.watch()
		return s, nil
	}
	if p.backend.Plan == "" && (len(servers) == 0 || p.backend.HaltTimeout.Duration == 0) {
		return nil, fmt.Errorf("no powered off servers in Linode account and no plan to allocate new servers")
	}

	// See if it's time to shutdown servers based on halt-timeout.
	halt := time.Now().Add(-p.backend.HaltTimeout.Duration)
	for _, i := range perm {
		s := servers[i]
		if plan > 0 && s.plan() != plan {
			continue
		}

		// Do not unreserve system unless it may really be used elsewhere.
		// If we cannot halt it right now, nobody else can either.
		if !p.reserve(s) {
			continue
		}

		// Take first in the permutation that timed out rather than
		// oldest, to reduce chances of conflict.
		if activity[i].IsZero() || activity[i].After(halt) {
			continue
		}

		// Ensure no recent activity again.
		when, err := p.recentActivity(s)
		if err != nil {
			printf("Cannot check %s for activity: %v", s, err)
			continue
		}
		if when.After(halt) {
			continue
		}

		printf("Server %s exceeds halt-timeout. Shutting it down...", s)
		_, err = p.shutdown(s)
		if err != nil {
			printf("Cannot shutdown %s after halt-timeout: %v", s, err)
			p.unreserve(s)
			continue
		}

		err = p.setup(s, system)
		if err != nil {
			p.unreserve(s)
			return nil, err
		}
		printf("Allocated %s.", s)
		s.watch()
		return s, nil
	}

	// Allocate a brand new server if allowed.
	if p.backend.Plan != "" && p.backend.Location != "" {
		s, err := p.createMachine(system)
		if err != nil {
			return nil, err
		}

		err = p.setup(s, system)
		if err != nil {
			if p.removeMachine(s) != nil {
				err = fmt.Errorf("cannot deallocate server after setup error: %v", err)
			}
			return nil, err
		}
		printf("Allocated %s.", s)
		s.watch()
		return s, nil
	}

	return nil, fmt.Errorf("no powered off servers in Linode account exceed halt-timeout")
}

func (s *linodeServer) Discard(ctx context.Context) error {
	s.watchTomb.Kill(nil)
	s.watchTomb.Wait()

	var err error
	if s.ephemeral() {
		err = s.p.removeMachine(s)
	} else {
		_, err1 := s.p.shutdown(s)
		err2 := s.p.removeConfig(s, "", s.d.Config)
		err3 := s.p.removeDisks(s, "", 0, s.d.Root, s.d.Swap)
		s.p.removeAbandoned(s)
		err = firstErr(err1, err2, err3)
	}
	s.p.unreserve(s)
	return err
}

type linodeListResult struct {
	Data []linodeServerData `json:"DATA"`
}

var linodeLabelWarning = true

func (p *linodeProvider) list() ([]*linodeServer, error) {
	p.listCache.mu.Lock()
	defer p.listCache.mu.Unlock()

	if p.listCache.next.After(time.Now()) {
		return p.listCache.data, nil
	}

	debug("Listing available Linode servers...")
	params := linodeParams{
		"api_action": "linode.list",
	}
	var result linodeListResult
	err := p.do(params, &result)
	if err != nil {
		return nil, err
	}
	servers := make([]*linodeServer, 0, len(result.Data))
	for _, d := range result.Data {
		if !linodeSpreadLabel.MatchString(d.Label) {
			if linodeLabelWarning && !linodeDefaultLabel.MatchString(d.Label) {
				linodeLabelWarning = false
				printf("WARNING: Some Linode servers ignored due to unsafe labels (must be \"Spread-...\").")
			}
			continue
		}
		servers = append(servers, &linodeServer{p: p, d: d})
	}

	p.listCache.data = servers
	p.listCache.next = time.Now().Add(3 * time.Second)

	return servers, nil
}

func (p *linodeProvider) status(s *linodeServer) (int, error) {
	servers, err := p.list()
	if err != nil {
		return 0, err
	}
	for _, server := range servers {
		if server.d.ID == s.d.ID {
			return server.d.Status, nil
		}
	}
	return 0, fmt.Errorf("cannot find %s for status check", s)
}

func (p *linodeProvider) setup(s *linodeServer, system *System) error {
	s.p = p
	s.system = system

	rootJob, swapJob, err := p.createDisk(s, system)
	if err != nil {
		p.removeDisks(s, "", noLog, s.d.Root, s.d.Swap)
		p.removeAbandoned(s)
		return err
	}
	s.d.Root = rootJob.DiskID
	s.d.Swap = swapJob.DiskID

	configID, err := p.createConfig(s, system, s.d.Root, s.d.Swap)
	if err != nil {
		p.removeDisks(s, "", 0, s.d.Root, s.d.Swap)
		return err
	}
	s.d.Config = configID

	_, err = p.waitJob(s, "allocate disk", rootJob.JobID)
	if err != nil {
		p.removeConfig(s, "", s.d.Config)
		p.removeDisks(s, "", noLog, s.d.Root, s.d.Swap)
		p.removeAbandoned(s)
		return err
	}

	ip, err := p.ip(s)
	if err != nil {
		return err
	}
	s.address = ip.IPAddress

	if status, e := p.status(s); e != nil {
		err = e
	} else if conflict, e := p.hasRecentDisk(s, s.d.Root); e != nil {
		err = e
	} else if conflict || status != linodeBrandNew && status != linodePoweredOff {
		err = fmt.Errorf("server %s concurrently allocated, giving up on it", s)
	}
	if err != nil {
		p.removeConfig(s, "", s.d.Config)
		p.removeDisks(s, "", 0, s.d.Root, s.d.Swap)
		return err
	}

	bootJob, err := p.boot(s, configID)
	if err == nil {
		printf("Waiting for %s to boot at %s...", s, s.address)
		_, err = p.waitJob(s, "boot", bootJob.JobID)
	}
	if err != nil {
		p.removeConfig(s, "", s.d.Config)
		p.removeDisks(s, "", 0, s.d.Root, s.d.Swap)
		return err
	}

	return nil
}

type linodeSimpleJob struct {
	JobID int `json:"JOBID"`
}

type linodeSimpleJobResult struct {
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
	if err := p.checkLabel(s); err != nil {
		return nil, fmt.Errorf("cannot %s %s: %v", verb, s, err)
	}
	var result linodeSimpleJobResult
	err := p.do(params, &result)
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
	Data *linodeDiskJob `json:"DATA"`
}

func (p *linodeProvider) createDisk(s *linodeServer, system *System) (root, swap *linodeDiskJob, err error) {
	template, _, err := p.template(system)
	if err != nil {
		return nil, nil, err
	}

	storage := 10000
	if system.Storage > 0 {
		storage = int(system.Storage / mb)
	}

	// Smallest disk is 30720MB. (10000+240)*3 == 30720,
	// so may halt two times without breaking.
	createRoot := linodeParams{
		"LinodeID": s.d.ID,
		"Label":    SystemLabel(system, "root"),
		"Size":     storage,
		"rootPass": p.options.Password,
	}
	createSwap := linodeParams{
		"api_action": "linode.disk.create",
		"LinodeID":   s.d.ID,
		"Label":      SystemLabel(system, "swap"),
		"Size":       240,
		"Type":       "swap",
	}

	debugf("Creating disk on %s with %s...", s, system.Image)
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
		p.removeDisks(s, "", noLog, root.DiskID)
	}
	if swap != nil {
		p.removeDisks(s, "", noLog, swap.DiskID)
	}
	return nil, nil, fmt.Errorf("cannot create Linode disk on %s: %v", s.d.Label, err)
}

func (p *linodeProvider) removeDisks(s *linodeServer, what string, flags doFlags, diskIDs ...int) error {
	if err := p.checkLabel(s); err != nil {
		return fmt.Errorf("cannot remove %sdisk on %s: %v", what, s, err)
	}

	if what != "" {
		what += " "
	}
	log := flags&noLog == 0
	if log {
		debugf("Removing %sdisks from %s...", what, s)
	}
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
	err := p.dofl(params, &results, flags)
	if err != nil {
		return fmt.Errorf("cannot remove %sdisk on %s: %v", what, s, err)
	}
	for _, result := range results {
		if err := result.err(); err != nil {
			return fmt.Errorf("cannot remove %sdisk on %s: %v", what, s, err)
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

type linodeConfig struct {
	ConfigID int    `json:"CONFIGID"`
	Label    string `json:"LABEL"`
}

type linodeConfigResult struct {
	Data []*linodeConfig `json:"DATA"`
}

func (p *linodeProvider) createConfig(s *linodeServer, system *System, rootID, swapID int) (configID int, err error) {
	debugf("Creating configuration on %s with %s...", s, system.Image)

	_, kernel, err := p.template(system)
	if err != nil {
		return 0, err
	}

	job := os.Getenv("TRAVIS_JOB_ID")
	if job != "" {
		job = fmt.Sprintf(` JOB="https://travis-ci.org/%s/jobs/%s"`, os.Getenv("TRAVIS_REPO_SLUG"), job)
	}

	reuse := ""
	if p.options.Reuse {
		reuse = " -reuse"
	}
	comments := fmt.Sprintf("USER=%q%s spread\n-pass=%q %s", username(), job, p.options.Password, reuse)

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

	var result struct {
		Data linodeConfig `json:"DATA"`
	}

	err = p.do(params, &result)
	if err != nil {
		return 0, fmt.Errorf("cannot create config on %s with %s: %v", s, system.Name, err)
	}
	return result.Data.ConfigID, nil
}

func (p *linodeProvider) removeConfig(s *linodeServer, what string, configID int) error {
	if err := p.checkLabel(s); err != nil {
		return fmt.Errorf("cannot remove %sconfig from %s: %v", what, s, err)
	}

	if what != "" {
		what += " "
	}
	debugf("Removing %sconfiguration from %s...", what, s)

	params := linodeParams{
		"api_action": "linode.config.delete",
		"LinodeID":   s.d.ID,
		"ConfigID":   configID,
	}
	err := p.do(params, nil)
	if err != nil {
		return fmt.Errorf("cannot remove %sconfig from %s: %v", what, s, err)
	}
	return nil
}

func (p *linodeProvider) configs(s *linodeServer) ([]*linodeConfig, error) {
	params := linodeParams{
		"api_action": "linode.config.list",
		"LinodeID":   s.d.ID,
	}
	var result linodeConfigResult
	err := p.do(params, &result)
	if err != nil {
		return nil, fmt.Errorf("cannot list configurations for %s: %v", s, err)
	}
	return result.Data, nil
}

// removeAbandoned removes abandoned configurations and disks.
// It warns on errors as this is an optional cleanup step.
func (p *linodeProvider) removeAbandoned(s *linodeServer) {
	const warn = "WARNING: Cannot clean up %s: %v"

	now := time.Now()

	configs, err := p.configs(s)
	if err != nil {
		printf(warn, s, err)
	}
	for _, config := range configs {
		t, err := ParseLabelTime(config.Label)
		if err == nil && now.Sub(t) > p.backend.HaltTimeout.Duration {
			err := p.removeConfig(s, "abandoned", config.ConfigID)
			if err != nil {
				printf(warn, s, err)
			}
		}
	}

	disks, err := p.disks(s)
	if err != nil {
		printf(warn, s, err)
	}
	var diskIds []int
	for _, disk := range disks {
		t, err := ParseLabelTime(disk.Label)
		if err == nil && now.Sub(t) > p.backend.HaltTimeout.Duration && disk.DiskID != s.d.Root && disk.DiskID != s.d.Swap {
			diskIds = append(diskIds, disk.DiskID)
		}
	}
	if len(diskIds) > 0 {
		err := p.removeDisks(s, "abandoned", 0, diskIds...)
		if err != nil {
			printf(warn, s, err)
		}
	}
}

type linodePlan struct {
	ID       int    `json:"PLANID"`
	Label    string `json:"LABEL"`
	AltLabel string `json:"-"`
}

func (p *linodeProvider) planID(name string) (int, error) {
	p.planCache.mu.Lock()
	defer p.planCache.mu.Unlock()

	plans := p.planCache.data
	if plans == nil {
		params := linodeParams{
			"api_action": "avail.linodeplans",
		}
		var result struct {
			Data []*linodePlan `json:"DATA"`
		}
		err := p.do(params, &result)
		if err != nil {
			return 0, fmt.Errorf("cannot list available Linode plans: %v", err)
		}
		plans = result.Data

		for _, plan := range plans {
			i := strings.LastIndex(plan.Label, " ")
			if i > 0 {
				n, err := strconv.Atoi(plan.Label[i+1:])
				if err == nil {
					plan.AltLabel = fmt.Sprintf("%s %dGB", plan.Label[:i], n/1024)
				}
			}
			plan.Label = strings.TrimPrefix(plan.Label, "Linode ")
			plan.AltLabel = strings.TrimPrefix(plan.AltLabel, "Linode ")
		}
		p.planCache.data = plans
	}

	name = strings.TrimPrefix(name, "Linode ")

	for _, plan := range plans {
		if name == plan.Label || name == plan.AltLabel {
			return plan.ID, nil
		}
	}

	return 0, &FatalError{fmt.Errorf("cannot find Linode plan %q", name)}
}

type linodeLocation struct {
	ID    int    `json:"DATACENTERID"`
	Label string `json:"LABEL"`
	Abbr  string `json:"ABBR"`
}

func (p *linodeProvider) locationID(name string) (int, error) {
	p.locationCache.mu.Lock()
	defer p.locationCache.mu.Unlock()

	locations := p.locationCache.data
	if locations == nil {
		params := linodeParams{
			"api_action": "avail.datacenters",
		}
		var result struct {
			Data []*linodeLocation `json:"DATA"`
		}
		err := p.do(params, &result)
		if err != nil {
			return 0, fmt.Errorf("cannot list available Linode locations: %v", err)
		}
		locations = result.Data

		for _, location := range locations {
			location.Label = strings.ToLower(location.Label)
			location.Abbr = strings.ToLower(location.Abbr)
		}
		p.locationCache.data = locations
	}

	name = strings.ToLower(name)
	for _, location := range locations {
		if name == location.Label || name == location.Abbr {
			return location.ID, nil
		}
	}

	return 0, &FatalError{fmt.Errorf("cannot find Linode location %q", name)}
}

func (p *linodeProvider) GarbageCollect() error {
	params := linodeParams{
		"api_action": "linode.list",
	}
	var result linodeListResult
	err := p.do(params, &result)
	if err != nil {
		return err
	}

	// Iterate out of order to reduce conflicts.
	perm := rnd.Perm(len(result.Data))

	now := time.Now()
	recent := now.Add(-3 * time.Minute)
	halt := now.Add(-p.backend.HaltTimeout.Duration)
	for _, i := range perm {
		d := result.Data[i]
		s := &linodeServer{p: p, d: d}

		if !(linodeSpreadLabel.MatchString(d.Label) || linodeDefaultLabel.MatchString(d.Label)) {
			// Don't even look at anything that doesn't seem like
			// a server spread should be working with.
			continue
		}

		if !p.reserve(s) {
			continue
		}

		printf("Checking %s...", s)
		when, err := p.recentActivity(s)
		if err != nil {
			printf("Cannot check %s for activity: %v", s, err)
			continue
		}
		switch {
		case when.Before(recent) && s.brandNew():
			printf("Server %s is brand new and has no activity. Removing it...", s)
		case when.Before(halt) && (s.ephemeral() || s.d.Status == linodeRunning):
			printf("Server %s exceeds halt-timeout. Shutting it down...", s)
		default:
			continue
		}

		if s.ephemeral() || s.brandNew() {
			err = p.removeMachine(s)
		} else {
			if s.d.Status == linodeRunning {
				_, err = p.shutdown(s)
			}
			s.p.removeAbandoned(s)
			p.unreserve(s)
		}
		if err != nil {
			printf("WARNING: Cannot garbage collect %s: %v", s, err)
		}
	}

	return nil
}

func (p *linodeProvider) createMachine(system *System) (*linodeServer, error) {
	debugf("Creating new Linode server for %s...", system.Name)

	location := p.backend.Location
	if location == "" {
		location = "fremont"
	}
	locationID, err := p.locationID(location)
	if err != nil {
		return nil, err
	}

	plan := p.backend.Plan
	if plan == "" {
		plan = "2GB"
	}
	planID, err := p.planID(plan)
	if err != nil {
		return nil, err
	}

	params := linodeParams{
		"api_action":   "linode.create",
		"PlanID":       planID,
		"DatacenterID": locationID,
	}
	var result struct {
		Data linodeServerData `json:"DATA"`
	}
	err = p.do(params, &result)
	if err != nil {
		return nil, fmt.Errorf("cannot allocate new Linode server for %s: %v", system.Name, err)
	}

	s := &linodeServer{
		p: p,
		d: linodeServerData{
			ID:    result.Data.ID,
			Label: fmt.Sprintf("Spread-%d", result.Data.ID),
			Group: linodeEphemeralGroup,
		},
	}

	// Reserve early so that once it's renamed we won't have
	// other goroutines attempting to pick it up.
	p.reserve(s)

	params = linodeParams{
		"api_action":       "linode.update",
		"LinodeID":         s.d.ID,
		"Label":            s.d.Label,
		"lpm_displayGroup": s.d.Group,
	}
	err = p.do(params, &result)
	if err != nil {
		if p.removeMachine(s) != nil {
			return nil, fmt.Errorf("cannot update or deallocate (!) new Linode server %s: %v", s, err)
		}
		return nil, fmt.Errorf("cannot update newly allocated Linode server %s: %v", s, err)
	}

	printf("Configuring new Linode server %s with %s...", s, system.Name)

	return s, nil
}

func (p *linodeProvider) checkLabel(s *linodeServer) error {
	if !linodeSpreadLabel.MatchString(s.d.Label) {
		return fmt.Errorf("Linode server labels must start with \"Spread-\" to prevent mistakes")
	}
	return nil
}

func (p *linodeProvider) removeMachine(s *linodeServer) error {
	if s.d.Status != linodeBrandNew || s.d.Group != "" || !linodeDefaultLabel.MatchString(s.d.Label) {
		if err := p.checkLabel(s); err != nil {
			return fmt.Errorf("cannot deallocate Linode server %s: %v", s, err)
		}
	}

	printf("Removing Linode server %s...", s)

	params := linodeParams{
		"api_action": "linode.delete",
		"LinodeID":   s.d.ID,
		"skipChecks": true,
	}
	err := p.do(params, nil)
	if err != nil {
		return fmt.Errorf("cannot deallocate Linode server %s: %v", s, err)
	}

	p.unreserve(s)
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

func (job *linodeJob) Entered() time.Time {
	return parseLinodeDT(job.EnteredDT)
}

func (job *linodeJob) HostStarted() time.Time {
	return parseLinodeDT(job.HostStartDT)
}

func (job *linodeJob) HostFinished() time.Time {
	return parseLinodeDT(job.HostFinishDT)
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
	Data []*linodeJob `json:"DATA"`
}

func (p *linodeProvider) jobs(s *linodeServer, flags doFlags) ([]*linodeJob, error) {
	params := linodeParams{
		"api_action": "linode.job.list",
		"LinodeID":   s.d.ID,
	}
	var result linodeJobResult
	err := p.dofl(params, &result, flags)
	if err != nil {
		return nil, fmt.Errorf("cannot get job details for %s: %v", s, err)
	}
	return result.Data, nil
}

func (p *linodeProvider) job(s *linodeServer, jobID int) (*linodeJob, error) {
	params := linodeParams{
		"api_action": "linode.job.list",
		"LinodeID":   s.d.ID,
		"JobID":      jobID,
	}
	var result linodeJobResult
	err := p.do(params, &result)
	if err == nil && len(result.Data) == 0 {
		err = fmt.Errorf("empty result")
	}
	if err != nil {
		return nil, fmt.Errorf("cannot get job details for %s: %v", s, err)
	}
	return result.Data[0], nil
}

func (p *linodeProvider) waitJob(s *linodeServer, verb string, jobID int) (*linodeJob, error) {
	debugf("Waiting for %s to %s...", s, verb)

	// Used to be 1 min up to Aug 2016, but disk allocation timeouts were frequently observed.
	timeout := time.After(3 * time.Minute)
	retry := time.NewTicker(5 * time.Second)
	defer retry.Stop()

	var infoErr error
	for {
		select {
		case <-timeout:
			// Don't shutdown. The server may be running something else.
			if infoErr != nil {
				return nil, infoErr
			}
			p.removeConfig(s, "", s.d.Config)
			p.removeDisks(s, "", 0, s.d.Root, s.d.Swap)
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

func (p *linodeProvider) hasActiveJob(s *linodeServer, action string, flags doFlags) (found bool, err error) {
	kind := ""
	if action != "" {
		kind += " " + action
	}
	if flags&noLog == 0 {
		debugf("Checking %s for active%s jobs...", s, kind)
	}
	jobs, err := p.jobs(s, flags)
	if err != nil {
		return false, err
	}
	for _, job := range jobs {
		if job.HostFinishDT == "" && (action == "" || job.Action == action) {
			return true, nil
		}
	}
	return false, nil
}

func (p *linodeProvider) recentActivity(s *linodeServer) (when time.Time, err error) {
	now := time.Now()
	if s.created().After(now.Add(-30 * time.Second)) {
		return now, nil
	}

	debugf("Checking %s for recent activity...", s)
	jobs, err := p.jobs(s, 0)
	if err != nil {
		return time.Time{}, fmt.Errorf("cannot check %s for recent activity: %v", s, err)
	}
	for _, job := range jobs {
		t := job.HostFinished()
		if t.IsZero() {
			return now, nil
		}
		return t, nil
	}
	return now, nil
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
	Data []*linodeDisk `json:"DATA"`
}

func (p *linodeProvider) disks(s *linodeServer) ([]*linodeDisk, error) {
	params := linodeParams{
		"api_action": "linode.disk.list",
		"LinodeID":   s.d.ID,
	}
	var result linodeDiskResult
	err := p.do(params, &result)
	if err != nil {
		return nil, err
	}
	return result.Data, nil
}

// hasRecentDisk returns an error if there's a disk that is ready and
// was created up to a minute before the provided disk ID. If two clients
// use this logic, the most recent one will concede the server usage to
// the oldest one. We need this hack because operations in Linode are not
// atomic, and a server will happily boot a second time on a different
// configuration overriding a recent boot.
func (p *linodeProvider) hasRecentDisk(s *linodeServer, diskID int) (bool, error) {
	debugf("Checking %s for allocation conflict...", s)
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
	debugf("Obtaining address of %s...", s)

	params := linodeParams{
		"api_action": "linode.ip.list",
		"LinodeID":   s.d.ID,
	}
	var result linodeIPResult
	err := p.do(params, &result)
	if err != nil {
		return nil, fmt.Errorf("cannot list IPs for %s: %v", s, err)
	}
	for _, ip := range result.Data {
		if ip.IsPublic == 1 {
			return ip, nil
		}
	}
	return nil, fmt.Errorf("cannot find public IP for %s", s)
}

type linodeTemplateResult struct {
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

	var err error
	if p.backend.Key == "" {
		err = fmt.Errorf("%s missing Linode API key", p.backend)
	} else {
		err = p.do(linodeParams{"api_action": "test.echo"}, nil)
	}
	if err == nil && linodeTimezone == nil {
		err = fmt.Errorf("cannot use Linode backends without a timezone database available")
	}
	if err != nil {
		err = &FatalError{err}
	}

	p.keyChecked = true
	p.keyErr = err
	return err
}

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

type linodeParams map[string]interface{}

type doFlags int

const (
	noLog doFlags = 1 << iota
	noCheckKey
	noPathPrefix
)

var linodeThrottle = throttle(time.Second / 10)

func throttle(d time.Duration) <-chan bool {
	ch := make(chan bool)
	go func() {
		for {
			ch <- true
			time.Sleep(d)
		}
	}()
	return ch
}

func (p *linodeProvider) do(params linodeParams, result interface{}) error {
	return p.dofl(params, result, 0)
}

func (p *linodeProvider) dofl(params linodeParams, result interface{}, flags doFlags) error {
	log := flags&noLog == 0
	if log {
		debugf("Linode request: %# v\n", params)
	}

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

	// Throttle as Linode misbehaves on frequent calls, sometimes returning
	// the error "something wasn't handled well", without a machine ID and
	// the machine never gets an IP address. Messy.
	<-linodeThrottle

	// Linode may *also* 503 on too many calls, apparently due to a rate
	// limit that surfaces as an HTML error page. Retry for now. /o\
	var err error
	var resp *http.Response
	var delays = rand.Perm(10)
	for i := 0; i < 10; i++ {
		resp, err = httpClient.PostForm("https://api.linode.com", values)
		if err == nil && 500 <= resp.StatusCode && resp.StatusCode < 600 {
			time.Sleep(time.Duration(delays[i]) * 250 * time.Millisecond)
			continue
		}
		break
	}
	if err != nil {
		return fmt.Errorf("cannot perform Linode request: %v", err)
	}
	defer resp.Body.Close()

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("cannot read Linode response: %v", err)
	}

	if log && Debug {
		var r interface{}
		if err := json.Unmarshal(data, &r); err == nil {
			debugf("Linode response: %# v\n", r)
		}
	}

	if result != nil {
		// Unmarshal even on errors, so the call site has a chance to inspect the data on errors.
		err = json.Unmarshal(data, result)
	}

	var eresult linodeResult
	if jerr := json.Unmarshal(data, &eresult); jerr == nil {
		if rerr := eresult.err(); rerr != nil {
			return rerr
		}
	}

	if err != nil {
		info := pretty.Sprintf("Request:\n-----\n%# v\n-----\nResponse:\n-----\n%s\n-----\n", params, data)
		return fmt.Errorf("cannot decode Linode response (status %d): %s\n%s", resp.StatusCode, err, info)
	}

	return nil
}

var linodeTimezone *time.Location

func init() {
	linodeTimezone, _ = time.LoadLocation("America/New_York")
}

func parseLinodeDT(dt string) time.Time {
	if dt != "" {
		t, err := time.ParseInLocation("2006-01-02 15:04:05.0", dt, linodeTimezone)
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
