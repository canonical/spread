package spread

import (
	"bytes"
	"fmt"
	"sync"

	"gopkg.in/tomb.v2"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Options struct {
	Password string
	Filter   Filter
	Reuse    map[string][]string
	Keep     bool
	Debug    bool
}

type Runner struct {
	tomb tomb.Tomb
	mu   sync.Mutex

	project   *Project
	options   *Options
	providers map[string]Provider
	reused    map[string]bool

	done  chan bool
	alive int

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
		reused:    make(map[string]bool),

		suiteWorkers: make(map[[3]string]int),
	}

	for bname, backend := range project.Backends {
		switch backend.Type {
		case "linode":
			r.providers[bname] = Linode(backend)
		case "lxd":
			r.providers[bname] = LXD(backend)
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

func (r *Runner) loop() error {
	defer func() {
		logNames(debugf, "Pending jobs after workers returned", r.pending, taskName)
		for _, job := range r.pending {
			if job != nil {
				r.add(&r.stats.TaskAbort, job)
			}
		}
		r.stats.log()
		if (r.options.Keep || r.options.Debug) && len(r.servers) > 0 {
			for _, server := range r.servers {
				printf("Keeping %s with %s at %s", server, server.Image(), server.Address())
			}
			if r.options.Debug {
				filter := ""
				if job := r.failedJob(); job != nil {
					filter = " " + job.Name
				}
				printf("Retry with: snap %s%s", r.reuseArgs(), filter)
			} else if r.options.Keep {
				printf("Reuse with: snap %s", r.reuseArgs())
			}
		}
	}()

	for _, backend := range r.project.Backends {
		for _, system := range backend.Systems {
			r.alive += backend.SystemWorkers[system]
		}
	}

	r.done = make(chan bool, r.alive)
	debugf("Creating %d worker%s...", r.alive, nth(r.alive, "", "", "s"))
	for _, backend := range r.project.Backends {
		for _, system := range backend.Systems {
			n := backend.SystemWorkers[system]
			for i := 0; i < n; i++ {
				go r.worker(backend, ImageID(system))
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

func (r *Runner) run(client *Client, job *Job, verb string, context interface{}, script string, debug *bool) bool {
	script = strings.TrimSpace(script)
	if len(script) == 0 {
		return true
	}
	contextStr := job.StringFor(context)
	logf("%s %s...", strings.Title(verb), contextStr)
	var cwd string
	if context == job.Backend || context == job.Project {
		cwd = r.project.RemotePath
	} else {
		cwd = filepath.Join(r.project.RemotePath, job.Task.Name)
	}
	_, err := client.Trace(script, cwd, job.Environment)
	if err != nil {
		printf("Error %s %s: %v", verb, contextStr, err)
		*debug = r.options.Debug
		return false
	}
	return true
}

func (r *Runner) add(where *[]*Job, job *Job) {
	r.mu.Lock()
	*where = append(*where, job)
	r.mu.Unlock()
}

func suiteWorkersKey(job *Job) [3]string {
	return [3]string{job.Backend.Name, string(job.System), job.Suite.Name}
}

func (r *Runner) worker(backend *Backend, system ImageID) {
	defer func() { r.done <- true }()

	var stats = &r.stats

	var debug bool
	var badProject bool
	var badSuite = make(map[*Suite]bool)

	var insideProject bool
	var insideBackend bool
	var insideSuite *Suite

	var client *Client
	var job, last *Job

	for {
		r.mu.Lock()
		if job != nil {
			r.suiteWorkers[suiteWorkersKey(job)]--
		}
		if badProject || debug || !r.tomb.Alive() {
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
			insideSuite = nil
			if !r.run(client, last, "restoring", insideSuite, insideSuite.Restore, &debug) {
				r.add(&stats.SuiteRestoreError, last)
				r.add(&stats.TaskAbort, job)
				badProject = true
				continue
			}
		}

		last = job

		if !insideProject {
			client = r.client(backend, job.System)
			if client == nil {
				r.add(&stats.TaskAbort, job)
				badProject = true
				continue
			}

			insideProject = true

			if !r.run(client, job, "preparing", r.project, r.project.Prepare, &debug) {
				r.add(&stats.ProjectPrepareError, job)
				r.add(&stats.TaskAbort, job)
				badProject = true
				continue
			}

			insideBackend = true

			if !r.run(client, job, "preparing", backend, backend.Prepare, &debug) {
				r.add(&stats.BackendPrepareError, job)
				r.add(&stats.TaskAbort, job)
				badProject = true
				continue
			}
		}

		if insideSuite != job.Suite {
			insideSuite = job.Suite
			if !r.run(client, job, "preparing", job.Suite, job.Suite.Prepare, &debug) {
				r.add(&stats.SuitePrepareError, job)
				r.add(&stats.TaskAbort, job)
				badSuite[job.Suite] = true
				continue
			}
		}

		if !r.run(client, job, "preparing", job, job.Task.Prepare, &debug) {
			r.add(&stats.TaskPrepareError, job)
			r.add(&stats.TaskAbort, job)
		} else if r.run(client, job, "executing", job, job.Task.Execute, &debug) {
			r.add(&stats.TaskDone, job)
		} else {
			r.add(&stats.TaskError, job)
		}
		if !debug && !r.run(client, job, "restoring", job, job.Task.Restore, &debug) {
			r.add(&stats.TaskRestoreError, job)
			badProject = true
		}
	}

	if client != nil {
		if !debug && insideSuite != nil {
			if !r.run(client, last, "restoring", insideSuite, insideSuite.Restore, &debug) {
				r.add(&stats.SuiteRestoreError, last)
			}
		}
		if !debug && insideBackend {
			if !r.run(client, last, "restoring", backend, backend.Restore, &debug) {
				r.add(&stats.BackendRestoreError, last)
			}
		}
		if !debug && insideProject {
			if !r.run(client, last, "restoring", r.project, r.project.Restore, &debug) {
				r.add(&stats.ProjectRestoreError, last)
			}
		}
		server := client.Server()
		if r.options.Debug {
			printf("Keeping data for debugging at %s on %s...", r.project.RemotePath, server)
		} else if r.options.Keep {
			printf("Removing data from %s on %s...", r.project.RemotePath, server)
			if err := client.RemoveAll(r.project.RemotePath); err != nil {
				printf("Error remove project from %s: %v", server, err)
			}
		}
		client.Close()
		if !r.options.Keep && !r.options.Debug {
			printf("Discarding %s...", server)
			if err := server.Discard(); err != nil {
				printf("Error discarding %s: %v", server, err)
			}
		}
	}
}

func (r *Runner) job(backend *Backend, system ImageID, suite *Suite) *Job {
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

func (r *Runner) client(backend *Backend, image ImageID) *Client {

	// TODO Consider stopping the runner after too many retries.

	var client *Client
	var server Server
	var err error
	for r.tomb.Alive() {

		// Look for a server available for reuse.
		reused := false
		r.mu.Lock()
		for _, addr := range r.options.Reuse[backend.Name] {
			if r.reused[addr] {
				continue
			}
			r.reused[addr] = true
			server = &UnknownServer{addr}
			reused = true
			printf("Reused %s:%s.", backend.Name, image.SystemID())
		}
		r.mu.Unlock()

		// Allocate a server when all else failed.
		if !reused {
			if len(r.options.Reuse) > 0 {
				printf("Reuse requested but none left for %s:%s, aborting.", backend.Name, image.SystemID())
				return nil
			}

			printf("Allocating %s:%s...", backend.Name, image.SystemID())
			var timeout = time.After(30 * time.Second)
			var relog = time.NewTicker(8 * time.Second)
			defer relog.Stop()
			var retry = time.NewTicker(5 * time.Second)
			defer retry.Stop()
			err = nil
		Allocate:
			for {
				lerr := err
				server, err = r.providers[backend.Name].Allocate(image, r.options.Password)
				if err == nil {
					break
				}
				if lerr == nil || lerr.Error() != err.Error() {
					printf("Cannot allocate %s:%s: %v", backend.Name, image.SystemID(), err)
				}

				// TODO Check if the error is unrecoverable (bad key, no machines whatsoever, etc).

				select {
				case <-retry.C:
				case <-relog.C:
					printf("Cannot allocate %s:%s: %v", backend.Name, image.SystemID(), err)
				case <-timeout:
					break Allocate
				case <-r.tomb.Dying():
					break Allocate
				}
			}
			if err != nil {
				continue
			}
		}

		printf("Connecting to %s...", server)

		var timeout = time.After(30 * time.Second)
		var relog = time.NewTicker(8 * time.Second)
		defer relog.Stop()
		var retry = time.NewTicker(5 * time.Second)
		defer retry.Stop()
	Dial:
		for {
			lerr := err
			client, err = Dial(server, r.options.Password)
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
			if reused {
				printf("Cannot connect to %s: %v", server, err)
			} else {
				printf("Discarding %s, cannot connect: %v", server, err)
				server.Discard()
			}
			continue
		}
		if !reused {
			err = client.WriteFile("/.spread.yaml", server.ReuseData())
			if err != nil {
				printf("Discarding %s, cannot write reuse data: %s", server, err)
				server.Discard()
				continue
			}
		}

		if _, ok := server.(*UnknownServer); ok {
			data, err := client.ReadFile("/.spread.yaml")
			if err != nil {
				printf("Cannot read reuse data for %s: %v", server, err)
				continue
			}
			s, err := r.providers[backend.Name].Reuse(data, r.options.Password)
			if err != nil {
				printf("Cannot reuse %s on %s: %v", server, backend, err)
				continue
			}
			server = s
			client.server = s
		}

		printf("Connected to %s.", server)

		send := true
		if r.options.Debug {
			empty, err := client.MissingOrEmpty(r.project.RemotePath)
			if err != nil {
				printf("Cannot send data to %s: %v", server, err)
				return nil
			}
			send = empty
		}

		if send {
			printf("Sending data to %s...", server)
			err := client.Send(r.project.Path, r.project.RemotePath, r.project.Include, r.project.Exclude)
			if err != nil {
				if reused {
					printf("Cannot send data to %s: %v", server, err)
				} else {
					printf("Discarding %s, cannot send data: %s", server, err)
					server.Discard()
				}
				continue
			}
		} else {
			printf("Debugging on %s and remote path has data. Won't send again.", server)
		}

		r.servers = append(r.servers, server)
		return client
	}

	return nil
}

func (r *Runner) reuseArgs() string {
	buf := &bytes.Buffer{}
	reuse := make(map[string][]string)
	backends := make([]string, 0, len(r.servers))
	for _, server := range r.servers {
		backend := server.Provider().Backend().Name
		backends = append(backends, backend)
		reuse[backend] = append(reuse[backend], server.Address())
	}
	sort.Strings(backends)
	buf.WriteString("-pass=")
	buf.WriteString(r.options.Password)
	buf.WriteString(" -reuse=")
	if len(reuse) > 1 {
		buf.WriteString("'")
	}
	for _, backend := range backends {
		buf.WriteString(backend)
		buf.WriteString(":")
		addrs := reuse[backend]
		sort.Strings(addrs)
		for _, addr := range addrs {
			buf.WriteString(addr)
			buf.WriteString(",")
		}
		buf.Truncate(buf.Len() - 1)
		buf.WriteString(" ")
	}
	buf.Truncate(buf.Len() - 1)
	if len(reuse) > 1 {
		buf.WriteString("'")
	}
	if r.options.Debug {
		buf.WriteString(" -debug")
	} else if r.options.Keep {
		buf.WriteString(" -keep")
	}
	return buf.String()
}

func (r *Runner) failedJob() *Job {
	errors := [][]*Job{
		r.stats.TaskError,
		r.stats.TaskPrepareError,
		r.stats.TaskRestoreError,
		r.stats.SuitePrepareError,
		r.stats.SuiteRestoreError,
		r.stats.BackendPrepareError,
		r.stats.BackendRestoreError,
		r.stats.ProjectPrepareError,
		r.stats.ProjectRestoreError,
	}
	for _, jobs := range errors {
		for _, job := range jobs {
			return job
		}
	}
	return nil
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
		names = append(names, fmt.Sprintf("%s:%s:%s", job.Backend.Name, job.System, name(job)))
	}
	if len(names) == 0 {
		return
	}
	sort.Strings(names)
	const dash = "\n    - "
	printf("%s: %d%s%s", prefix, len(names), dash, strings.Join(names, dash))
}
