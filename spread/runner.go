package spread

import (
	"bytes"
	"fmt"
	"sync"

	"gopkg.in/tomb.v2"
	"gopkg.in/yaml.v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Options struct {
	Password string
	Filter   Filter
	Reuse    []string
	Keep     bool
	Debug    bool
	Shell    bool
	Abend    bool
	Restore  bool
	Resend   bool
	Discard  bool
}

type Runner struct {
	tomb tomb.Tomb
	mu   sync.Mutex

	project   *Project
	options   *Options
	providers map[string]Provider

	done  chan bool
	alive int

	reuse   map[Server]*Client
	servers []Server
	pending []*Job
	stats   stats

	suiteWorkers map[[3]string]int
}

func Start(project *Project, options *Options) (*Runner, error) {
	debugf("Starting runner with passsword %q.", options.Password)

	r := &Runner{
		project:   project,
		options:   options,
		providers: make(map[string]Provider),
		reuse:     make(map[Server]*Client),

		suiteWorkers: make(map[[3]string]int),
	}

	for bname, backend := range project.Backends {
		switch backend.Type {
		case "linode":
			r.providers[bname] = Linode(project, backend, options)
		case "lxd":
			r.providers[bname] = LXD(project, backend, options)
		case "qemu":
			r.providers[bname] = QEMU(project, backend, options)
		case "adhoc":
			r.providers[bname] = AdHoc(project, backend, options)
		default:
			return nil, fmt.Errorf("%s has unsupported type %q", backend, backend.Type)
		}
	}

	pending, err := project.Jobs(options)
	if err != nil {
		return nil, err
	}
	r.pending = pending

	r.tomb.Go(r.loop)
	return r, nil
}

func (r *Runner) Wait() error {
	return r.tomb.Wait()
}

func (r *Runner) Stop() error {
	r.tomb.Kill(nil)
	return r.tomb.Wait()
}

func (r *Runner) loop() (err error) {
	// Discover all servers for which reuse was requested.
	r.done = make(chan bool, r.alive)
	for _, addr := range r.options.Reuse {
		go r.prepareReuse(addr)
	}
	for range r.options.Reuse {
		<-r.done
	}
	if len(r.reuse) != len(r.options.Reuse) {
		seen := make(map[string]bool)
		for server, client := range r.reuse {
			seen[server.Address()] = true
			client.Close()
		}
		missing := make([]string, 0, len(r.options.Reuse))
		for _, addr := range r.options.Reuse {
			if !seen[addr] {
				missing = append(missing, addr)
			}
		}
		return fmt.Errorf("cannot reuse address%s: %s", nth(len(missing), "", "", "es"), strings.Join(missing, ", "))
	}

	defer func() {
		if !r.options.Discard {
			logNames(debugf, "Pending jobs after workers returned", r.pending, taskName)
			for _, job := range r.pending {
				if job != nil {
					r.add(&r.stats.TaskAbort, job)
				}
			}
			r.stats.log()
		}
		if r.options.Keep && len(r.servers) > 0 {
			for _, server := range r.servers {
				printf("Keeping %s at %s", server, server.Address())
			}
			printf("Reuse with: %s %s", os.Args[0], r.reuseArgs())
		}
		if !r.options.Keep {
			for len(r.servers) > 0 {
				printf("Discarding %s...", r.servers[0])
				r.discardServer(r.servers[0])
			}
		}
		if err == nil && (len(r.stats.TaskAbort) > 0 || r.stats.errorCount() > 0) {
			err = fmt.Errorf("unsuccessful run")
		}
	}()

	if r.options.Discard {
		return nil
	}

	// Find out how many workers are needed for each backend system.
	// Even if multiple workers per system are requested, must not
	// have more workers than there are jobs.
	workers := make(map[*System]int)
	for _, backend := range r.project.Backends {
		for _, system := range backend.Systems {
			for _, job := range r.pending {
				if job.Backend == backend && job.System == system {
					if system.Workers > workers[system] {
						workers[system]++
						r.alive++
					} else {
						break
					}
				}
			}
		}
	}

	r.done = make(chan bool, r.alive)

	msg := fmt.Sprintf("Starting %d worker%s for the following jobs", r.alive, nth(r.alive, "", "", "s"))
	logNames(debugf, msg, r.pending, taskName)

	for _, backend := range r.project.Backends {
		for _, system := range backend.Systems {
			n := workers[system]
			for i := 0; i < n; i++ {
				go r.worker(backend, system)
			}
		}
	}

	for {
		select {
		case <-r.done:
			r.alive--
			if r.alive > 0 {
				debugf("Worker terminated. %d still alive.", r.alive)
				continue
			}
			debugf("Worker terminated.")
			return nil
		}
	}

	return nil
}

const (
	preparing = "preparing"
	executing = "executing"
	restoring = "restoring"
)

func (r *Runner) run(client *Client, job *Job, verb string, context interface{}, script string, abend *bool) bool {
	script = strings.TrimSpace(script)
	if len(script) == 0 {
		return true
	}
	contextStr := job.StringFor(context)
	logf("%s %s... (%d jobs left)", strings.Title(verb), contextStr, r.pendingJobs())
	var dir string
	if context == job.Backend || context == job.Project {
		dir = r.project.RemotePath
	} else {
		dir = filepath.Join(r.project.RemotePath, job.Task.Name)
	}
	if r.options.Shell && verb == executing {
		printf("Starting shell instead of %s %s...", verb, job)
		err := client.Shell("/bin/bash", dir, r.shellEnv(job, job.Environment))
		if err != nil {
			printf("Error running debug shell: %v", err)
		}
		printf("Continuing...")
		return true
	}
	client.SetWarnTimeout(job.WarnTimeoutFor(context))
	client.SetKillTimeout(job.KillTimeoutFor(context))
	_, err := client.Trace(script, dir, job.Environment)
	if err != nil {
		printf("Error %s %s : %v", verb, contextStr, err)
		if r.options.Debug {
			printf("Starting shell to debug...")
			err = client.Shell("/bin/bash", dir, r.shellEnv(job, job.Environment))
			if err != nil {
				printf("Error running debug shell: %v", err)
			}
			printf("Continuing...")
		}
		*abend = r.options.Abend
		return false
	}
	return true
}

func (r *Runner) shellEnv(job *Job, env *Environment) *Environment {
	senv := env.Copy()
	senv.Set("PS1", `'\$SPREAD_BACKEND:\$SPREAD_SYSTEM \${PWD/#\$SPREAD_PATH/...}\\$ '`)
	return senv
}

func (r *Runner) add(where *[]*Job, job *Job) {
	r.mu.Lock()
	*where = append(*where, job)
	r.mu.Unlock()
}

func suiteWorkersKey(job *Job) [3]string {
	return [3]string{job.Backend.Name, job.System.Name, job.Suite.Name}
}

func (r *Runner) worker(backend *Backend, system *System) {
	defer func() { r.done <- true }()

	client := r.client(backend, system)
	if client == nil {
		return
	}

	var stats = &r.stats

	var abend bool
	var badProject bool
	var badSuite = make(map[*Suite]bool)

	var insideProject bool
	var insideBackend bool
	var insideSuite *Suite

	var job, last *Job

	for {
		r.mu.Lock()
		if job != nil {
			r.suiteWorkers[suiteWorkersKey(job)]--
		}
		if badProject || abend || !r.tomb.Alive() {
			r.mu.Unlock()
			break
		}
		job = r.job(backend, system, insideSuite)
		if job == nil {
			r.mu.Unlock()
			break
		}
		r.suiteWorkers[suiteWorkersKey(job)]++
		r.mu.Unlock()

		if badSuite[job.Suite] {
			r.add(&stats.TaskAbort, job)
			continue
		}

		if insideSuite != nil && insideSuite != job.Suite {
			if false {
				printf("WARNING: Was inside missing suite %s on last run, so cannot restore it.", insideSuite)
			} else if !r.run(client, last, restoring, insideSuite, insideSuite.Restore, &abend) {
				r.add(&stats.SuiteRestoreError, last)
				r.add(&stats.TaskAbort, job)
				badProject = true
				continue
			}
			insideSuite = nil
		}

		last = job

		if !insideProject {
			insideProject = true
			if !r.options.Restore && !r.run(client, job, preparing, r.project, r.project.Prepare, &abend) {
				r.add(&stats.ProjectPrepareError, job)
				r.add(&stats.TaskAbort, job)
				badProject = true
				continue
			}

			insideBackend = true
			if !r.options.Restore && !r.run(client, job, preparing, backend, backend.Prepare, &abend) {
				r.add(&stats.BackendPrepareError, job)
				r.add(&stats.TaskAbort, job)
				badProject = true
				continue
			}
		}

		if insideSuite != job.Suite {
			insideSuite = job.Suite
			if !r.options.Restore && !r.run(client, job, preparing, job.Suite, job.Suite.Prepare, &abend) {
				r.add(&stats.SuitePrepareError, job)
				r.add(&stats.TaskAbort, job)
				badSuite[job.Suite] = true
				continue
			}
		}

		if r.options.Restore {
			// Do not prepare or execute.
		} else if !r.options.Restore && !r.run(client, job, preparing, job, job.Prepare(), &abend) {
			r.add(&stats.TaskPrepareError, job)
			r.add(&stats.TaskAbort, job)
		} else if !r.options.Restore && r.run(client, job, executing, job, job.Task.Execute, &abend) {
			r.add(&stats.TaskDone, job)
		} else if !r.options.Restore {
			r.add(&stats.TaskError, job)
		}
		if !abend && !r.run(client, job, restoring, job, job.Restore(), &abend) {
			r.add(&stats.TaskRestoreError, job)
			badProject = true
		}
	}

	if !abend && insideSuite != nil {
		if !r.run(client, last, restoring, insideSuite, insideSuite.Restore, &abend) {
			r.add(&stats.SuiteRestoreError, last)
		}
		insideSuite = nil
	}
	if !abend && insideBackend {
		if !r.run(client, last, restoring, backend, backend.Restore, &abend) {
			r.add(&stats.BackendRestoreError, last)
		}
		insideBackend = false
	}
	if !abend && insideProject {
		if !r.run(client, last, restoring, r.project, r.project.Restore, &abend) {
			r.add(&stats.ProjectRestoreError, last)
		}
		insideProject = false
	}
	server := client.Server()
	client.Close()
	if !r.options.Keep {
		printf("Discarding %s...", server)
		r.discardServer(server)
	}
}

func (r *Runner) pendingJobs() int {
	n := 0
	for _, job := range r.pending {
		if job != nil {
			n++
		}
	}
	return n
}

func (r *Runner) job(backend *Backend, system *System, suite *Suite) *Job {
	var best = -1
	var bestWorkers = 1000000
	for i, job := range r.pending {
		if job == nil {
			continue
		}
		if job.Backend != backend || job.System != system {
			// Different backend or system is not an option at all.
			continue
		}
		if job.Suite == suite {
			// Best possible case.
			best = i
			break
		}
		if c := r.suiteWorkers[suiteWorkersKey(job)]; c < bestWorkers {
			best = i
			bestWorkers = c
		}
	}
	if best >= 0 {
		job := r.pending[best]
		r.pending[best] = nil
		return job
	}
	return nil
}

func (r *Runner) client(backend *Backend, system *System) *Client {

	retries := 0
	for r.tomb.Alive() {
		if retries == 3 {
			printf("Cannot allocate %s after too many retries.", system)
			break
		}
		retries++

		client := r.reuseServer(backend, system)
		reused := client != nil
		if !reused {
			client = r.allocateServer(backend, system)
			if client == nil {
				break
			}
		}

		server := client.Server()
		send := true
		if reused && r.options.Resend {
			printf("Removing project data from %s at %s...", server, r.project.RemotePath)
			if err := client.RemoveAll(r.project.RemotePath); err != nil {
				printf("Cannot remove project data from %s: %v", server, err)
			}
		} else if reused {
			empty, err := client.MissingOrEmpty(r.project.RemotePath)
			if err != nil {
				printf("Cannot send project data to %s: %v", server, err)
				client.Close()
				continue
			}
			send = empty
		}

		if send {
			printf("Sending project data to %s...", server)
			err := client.Send(r.project.Path, r.project.RemotePath, r.project.Include, r.project.Exclude)
			if err != nil {
				if reused {
					printf("Cannot send project data to %s: %v", server, err)
				} else {
					printf("Discarding %s, cannot send project data: %s", server, err)
					r.discardServer(server)
				}
				client.Close()
				continue
			}
		} else {
			printf("Reusing project data on %s...", server)
		}
		return client
	}

	return nil
}

func (r *Runner) discardServer(server Server) {
	if err := server.Discard(); err != nil {
		printf("Error discarding %s: %v", server, err)
	}
	r.mu.Lock()
	for i, s := range r.servers {
		if s == server {
			r.servers = append(r.servers[:i], r.servers[i+1:]...)
			break
		}
	}
	r.mu.Unlock()
}

func (r *Runner) allocateServer(backend *Backend, system *System) *Client {
	if len(r.options.Reuse) > 0 {
		printf("Reuse requested but none left for %s, aborting.", system)
		return nil
	}

	printf("Allocating %s...", system)
	var timeout = time.After(5 * time.Minute)
	var relog = time.NewTicker(15 * time.Second)
	defer relog.Stop()
	var retry = time.NewTicker(5 * time.Second)
	defer retry.Stop()

	var server Server
	var err error
Allocate:
	for {
		lerr := err
		server, err = r.providers[backend.Name].Allocate(system)
		if err == nil {
			break
		}
		if lerr == nil || lerr.Error() != err.Error() {
			printf("Cannot allocate %s: %v", system, err)
			if _, ok := err.(*FatalError); ok {
				return nil
			}
		}

		select {
		case <-retry.C:
		case <-relog.C:
			printf("Cannot allocate %s: %v", system, err)
		case <-timeout:
			break Allocate
		case <-r.tomb.Dying():
			break Allocate
		}
	}
	if err != nil {
		return nil
	}

	printf("Connecting to %s...", server)

	timeout = time.After(1 * time.Minute)
	relog = time.NewTicker(8 * time.Second)
	defer relog.Stop()
	retry = time.NewTicker(5 * time.Second)
	defer retry.Stop()

	username := system.Username
	password := system.Password
	if username == "" {
		username = "root"
	}
	if password == "" {
		password = r.options.Password
	}

	var client *Client
Dial:
	for {
		lerr := err
		client, err = Dial(server, username, password)
		if err == nil {
			break
		}
		if lerr == nil || lerr.Error() != err.Error() {
			debugf("Cannot connect to %s: %v", server, err)
		}

		select {
		case <-retry.C:
		case <-relog.C:
			debugf("Cannot connect to %s: %v", server, err)
		case <-timeout:
			break Dial
		case <-r.tomb.Dying():
			break Dial
		}
	}
	if err != nil {
		printf("Discarding %s, cannot connect: %v", server, err)
		r.discardServer(server)
		return nil
	}
	if username != "root" || password != r.options.Password {
		err = client.SetupRootAccess(r.options.Password)
		if err != nil {
			printf("Discarding %s, %v", server, err)
			client.Close()
			r.discardServer(server)
			return nil
		}
		if username != "root" {
			printf("Reconnecting to %s as root...", server)
			client.Close()
			username = "root"
			password = r.options.Password
			goto Dial
		}
	}
	err = client.WriteFile("/.spread.yaml", server.ReuseData())
	if err != nil {
		printf("Discarding %s, cannot write reuse data: %s", server, err)
		client.Close()
		r.discardServer(server)
		return nil
	}

	printf("Connected to %s.", server)
	r.servers = append(r.servers, server)
	return client
}

func (r *Runner) reuseServer(backend *Backend, system *System) *Client {
	r.mu.Lock()
	defer r.mu.Unlock()

	for server, client := range r.reuse {
		if server.Provider().Backend() == backend && server.System() == system {
			delete(r.reuse, server)
			printf("Reusing %s...", server)
			return client
		}
	}
	return nil
}

func (r *Runner) prepareReuse(addr string) {
	defer func() { r.done <- true }()

	printf("Connecting to %s for reuse...", addr)
	var server Server = &UnknownServer{addr}
	client, err := Dial(server, "root", r.options.Password)
	if err != nil {
		printf("Cannot connect to %s: %v", addr, err)
		return
	}
	data, err := client.ReadFile("/.spread.yaml")
	if err != nil {
		printf("Cannot read reuse data for %s: %v", server, err)
		return
	}
	var info struct {
		Backend string
	}
	err = yaml.Unmarshal(data, &info)
	if err != nil {
		printf("Cannot read reuse data for %s: %v", server, err)
		return
	}
	provider, ok := r.providers[info.Backend]
	if !ok {
		printf("Cannot reuse %s: backend %q is missing", server, info.Backend)
		return
	}
	server, err = provider.Reuse(data)
	if err != nil {
		printf("Cannot reuse %s on %s: %v", addr, info.Backend, err)
		return
	}
	client.server = server

	r.mu.Lock()
	r.reuse[server] = client
	r.servers = append(r.servers, server)
	debugf("Prepared %s:%s server for reuse.", server.Provider().Backend().Name, server.System())
	r.mu.Unlock()
}

func (r *Runner) reuseArgs() string {
	buf := &bytes.Buffer{}
	var addrs []string
	for _, server := range r.servers {
		addrs = append(addrs, server.Address())
	}
	sort.Strings(addrs)
	buf.WriteString("-pass=")
	buf.WriteString(r.options.Password)
	buf.WriteString(" -reuse=")
	buf.WriteString(strings.Join(addrs, ","))
	if r.options.Keep {
		buf.WriteString(" -keep")
	}
	switch {
	case r.options.Debug:
		buf.WriteString(" -debug")
	case r.options.Shell:
		buf.WriteString(" -shell")
	case r.options.Abend:
		buf.WriteString(" -abend")
	case r.options.Restore:
		buf.WriteString(" -restore")
	}
	return buf.String()
}

type stats struct {
	TaskDone            []*Job
	TaskError           []*Job
	TaskAbort           []*Job
	TaskPrepareError    []*Job
	TaskRestoreError    []*Job
	SuitePrepareError   []*Job
	SuiteRestoreError   []*Job
	BackendPrepareError []*Job
	BackendRestoreError []*Job
	ProjectPrepareError []*Job
	ProjectRestoreError []*Job
}

func (s *stats) errorCount() int {
	errors := [][]*Job{
		s.TaskError,
		s.TaskPrepareError,
		s.TaskRestoreError,
		s.SuitePrepareError,
		s.SuiteRestoreError,
		s.BackendPrepareError,
		s.BackendRestoreError,
		s.ProjectPrepareError,
		s.ProjectRestoreError,
	}
	count := 0
	for _, jobs := range errors {
		count += len(jobs)
	}
	return count
}

func (s *stats) log() {
	printf("Successful tasks: %d", len(s.TaskDone))
	printf("Aborted tasks: %d", len(s.TaskAbort))

	logNames(printf, "Failed tasks", s.TaskError, taskName)
	logNames(printf, "Failed task prepare", s.TaskPrepareError, taskName)
	logNames(printf, "Failed task restore", s.TaskRestoreError, taskName)
	logNames(printf, "Failed suite prepare", s.SuitePrepareError, suiteName)
	logNames(printf, "Failed suite restore", s.SuiteRestoreError, suiteName)
	logNames(printf, "Failed backend prepare", s.BackendPrepareError, backendName)
	logNames(printf, "Failed backend restore", s.BackendRestoreError, backendName)
	logNames(printf, "Failed project prepare", s.ProjectPrepareError, projectName)
	logNames(printf, "Failed project restore", s.ProjectRestoreError, projectName)
}

func projectName(job *Job) string { return "project" }
func backendName(job *Job) string { return job.Backend.Name }
func suiteName(job *Job) string   { return job.Suite.Name }

func taskName(job *Job) string {
	if job.Variant == "" {
		return job.Task.Name
	}
	return job.Task.Name + ":" + job.Variant
}

func logNames(f func(format string, args ...interface{}), prefix string, jobs []*Job, name func(job *Job) string) {
	names := make([]string, 0, len(jobs))
	for _, job := range jobs {
		if job == nil {
			continue
		}
		names = append(names, fmt.Sprintf("%s:%s", job.System, name(job)))
	}
	if len(names) == 0 {
		return
	}
	sort.Strings(names)
	const dash = "\n    - "
	f("%s: %d%s%s", prefix, len(names), dash, strings.Join(names, dash))
}
