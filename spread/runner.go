package spread

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/tomb.v2"
	"math"
	"math/rand"
)

type Options struct {
	Password       string
	Filter         Filter
	Reuse          bool
	ReusePid       int
	Debug          bool
	Shell          bool
	ShellBefore    bool
	ShellAfter     bool
	Abend          bool
	Restore        bool
	Resend         bool
	Discard        bool
	Artifacts      string
	Seed           int64
	Repeat         int
	GarbageCollect bool
}

type Runner struct {
	tomb tomb.Tomb
	mu   sync.Mutex

	project   *Project
	options   *Options
	providers map[string]Provider

	contentTomb tomb.Tomb
	contentFile *os.File
	contentSize int64

	done  chan bool
	alive int

	reuse    *Reuse
	reserved map[string]bool
	servers  []Server
	pending  []*Job
	sequence map[*Job]int
	stats    stats

	suiteWorkers map[[3]string]int
}

func Start(project *Project, options *Options) (*Runner, error) {
	r := &Runner{
		project:   project,
		options:   options,
		providers: make(map[string]Provider),
		reserved:  make(map[string]bool),
		sequence:  make(map[*Job]int),

		suiteWorkers: make(map[[3]string]int),
	}

	for bname, backend := range project.Backends {
		switch backend.Type {
		case "google":
			r.providers[bname] = Google(project, backend, options)
		case "linode":
			r.providers[bname] = Linode(project, backend, options)
		case "lxd":
			r.providers[bname] = LXD(project, backend, options)
		case "qemu":
			r.providers[bname] = QEMU(project, backend, options)
		case "adhoc":
			r.providers[bname] = AdHoc(project, backend, options)
		case "humbox":
			r.providers[bname] = Humbox(project, backend, options)
		default:
			return nil, fmt.Errorf("%s has unsupported type %q", backend, backend.Type)
		}
	}

	pending, err := project.Jobs(options)
	if err != nil {
		return nil, err
	}
	r.pending = pending

	if options.GarbageCollect {
		for _, p := range r.providers {
			if err := p.GarbageCollect(); err != nil {
				printf("Error collecting garbage from %q: %v", p.Backend().Name, err)
			}
		}
	}

	r.reuse, err = OpenReuse(r.reusePath())
	if err != nil {
		return nil, err
	}

	r.tomb.Go(r.loop)
	return r, nil
}

func (r *Runner) reusePath() string {
	if r.options.ReusePid != 0 {
		return filepath.Join(r.project.Path, fmt.Sprintf(".spread-reuse.%d.yaml", r.options.ReusePid))
	}
	if r.options.Reuse {
		return filepath.Join(r.project.Path, ".spread-reuse.yaml")
	}
	return filepath.Join(r.project.Path, fmt.Sprintf(".spread-reuse.%d.yaml", os.Getpid()))
}

type projectContent struct {
	fd  *os.File
	err error
}

func (r *Runner) Wait() error {
	return r.tomb.Wait()
}

func (r *Runner) Stop() error {
	r.tomb.Kill(nil)
	return r.tomb.Wait()
}

func (r *Runner) loop() (err error) {
	if r.options.GarbageCollect {
		return nil
	}
	defer func() {
		r.contentTomb.Kill(nil)
		r.contentTomb.Wait()
		if r.contentFile != nil {
			r.contentFile.Close()
		}

		if !r.options.Discard {
			logNames(debugf, "Pending jobs after workers returned", r.pending, taskName)
			for _, job := range r.pending {
				if job != nil {
					r.add(&r.stats.TaskAbort, job)
				}
			}
			r.stats.log()
		}
		if !r.options.Reuse || r.options.Discard {
			for len(r.servers) > 0 {
				printf("Discarding %s...", r.servers[0])
				r.discardServer(r.servers[0])
			}
			if !r.options.Reuse {
				os.Remove(r.reusePath())
			}
		}
		if len(r.servers) > 0 {
			for _, server := range r.servers {
				printf("Keeping %s at %s", server, server.Address())
			}
		}
		r.reuse.Close()
		if err == nil && (len(r.stats.TaskAbort) > 0 || r.stats.errorCount() > 0) {
			err = fmt.Errorf("unsuccessful run")
		}
	}()

	r.contentTomb.Go(r.prepareContent)

	// Make it sequential for now.
	_, err = r.waitContent()
	if err != nil {
		return err
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

	seed := r.options.Seed
	if !r.options.Discard && seed == 0 && len(r.pending) > r.alive {
		seed = time.Now().Unix()
		printf("Sequence of jobs produced with -seed=%d", seed)
	}
	if !r.options.Discard && !r.options.Reuse && r.options.ReusePid == 0 {
		printf("If killed, discard servers with: spread -reuse-pid=%d -discard", os.Getpid())
	}

	for _, backend := range r.project.Backends {
		for _, system := range backend.Systems {
			n := workers[system]
			for i := 0; i < n; i++ {
				// Use a different seed per worker, so that the work-stealing
				// logic will have a better chance of producing the same
				// ordering on each of the workers.
				order := rand.New(rand.NewSource(seed + int64(i))).Perm(len(r.pending))
				go r.worker(backend, system, order)
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
}

func (r *Runner) prepareContent() (err error) {
	if r.options.Discard {
		return nil
	}

	file, err := ioutil.TempFile("", fmt.Sprintf("spread-content.%d.", os.Getpid()))
	if err != nil {
		return fmt.Errorf("cannot create temporary content file: %v", err)
	}
	defer func() {
		var size string
		if r.contentSize < 1024*1024 {
			size = fmt.Sprintf("%.2fKB", float64(r.contentSize)/1024)
		} else {
			size = fmt.Sprintf("%.2fMB", float64(r.contentSize)/(1024*1024))
		}
		if err == nil {
			printf("Project content is packed for delivery (%s).", size)
		} else {
			printf("Error packing project content for delivery: %v", err)
			file.Close()
			r.tomb.Killf("cannot pack project content for delivery")
		}
	}()

	if err = os.Remove(file.Name()); err != nil {
		return fmt.Errorf("cannot remove temporary content file: %v", err)
	}

	args := []string{"c", "--exclude=.spread-reuse.*"}
	if r.project.Repack == "" {
		args[0] = "cz"
	}
	for _, pattern := range r.project.Exclude {
		args = append(args, "--exclude="+pattern)
	}
	for _, pattern := range r.project.Rename {
		args = append(args, "--transform="+pattern)
	}
	include := r.project.Include
	if len(include) == 0 {
		include, err = filterDir(r.project.Path)
		if err != nil {
			return fmt.Errorf("cannot list project directory: %v", err)
		}
	}
	args = append(args, include...)

	var stderr bytes.Buffer
	cmd := exec.Command("tar", args...)
	cmd.Dir = r.project.Path
	cmd.Stderr = &stderr

	if r.project.Repack == "" {
		// tar cz => temporary file.
		cmd.Stdout = file
		err = cmd.Start()
		if err != nil {
			return fmt.Errorf("cannot start local tar command: %v", err)
		}

		go func() {
			// TODO Kill that when the function quits.
			select {
			case <-r.contentTomb.Dying():
				cmd.Process.Kill()
			}
		}()

		err = cmd.Wait()
		err = outputErr(stderr.Bytes(), err)
		if err != nil {
			return fmt.Errorf("cannot pack project tree: %v", err)
		}
	} else {
		// tar c => repack => gzip => temporary file
		// repack acts via fd 3 and 4
		tarr, tarw, err := os.Pipe()
		if err != nil {
			return fmt.Errorf("cannot create pipe for repack: %v", err)
		}
		defer tarr.Close()
		defer tarw.Close()

		gzr, gzw, err := os.Pipe()
		if err != nil {
			return fmt.Errorf("cannot create pipe for repack: %v", err)
		}
		defer gzr.Close()
		defer gzw.Close()

		cmd.Stdout = tarw

		err = cmd.Start()
		if err != nil {
			return fmt.Errorf("cannot start local tar command: %v", err)
		}

		lscript := localScript{
			script:      r.project.Repack,
			dir:         r.project.Path,
			env:         r.project.Environment.Variant(""),
			warnTimeout: r.project.WarnTimeout.Duration,
			killTimeout: r.project.KillTimeout.Duration,
			mode:        traceOutput,
			extraFiles:  []*os.File{tarr, gzw},
			stop:        r.contentTomb.Dying(),
		}
		gz := gzip.NewWriter(file)

		var errch = make(chan error, 3)
		var wg sync.WaitGroup

		wg.Add(1)
		go func() {
			err := cmd.Wait()
			errch <- outputErr(stderr.Bytes(), err)

			// Unblock script.
			tarw.Close()

			wg.Done()
		}()

		wg.Add(1)
		go func() {
			_, _, err := lscript.run()
			errch <- err

			// Stop tar and unblock gz.
			cmd.Process.Kill()
			gzw.Close()

			wg.Done()
		}()

		wg.Add(1)
		go func() {
			_, err := io.Copy(gz, gzr)
			errch <- firstErr(err, gz.Close())

			// Stop tar.
			cmd.Process.Kill()

			wg.Done()
		}()

		go func() {
			// TODO Kill that when the function quits.
			select {
			case <-r.contentTomb.Dying():
				cmd.Process.Kill()
			}
		}()

		wg.Wait()

		err = firstErr(<-errch, <-errch, <-errch)
		if err != nil {
			return fmt.Errorf("cannot pack project tree: %v", err)
		}
	}

	st, err := file.Stat()
	if err != nil {
		return fmt.Errorf("cannot stat temporary content file: %v", err)
	}

	r.contentSize = st.Size()
	r.contentFile = file
	return nil
}

func (r *Runner) waitContent() (io.Reader, error) {
	if err := r.contentTomb.Wait(); err != nil {
		return nil, err
	}
	return io.NewSectionReader(r.contentFile, 0, r.contentSize), nil
}

const (
	preparing = "preparing"
	executing = "executing"
	restoring = "restoring"
)

func (r *Runner) run(client *Client, job *Job, verb string, context interface{}, script, debug string, abend *bool) bool {
	script = strings.TrimSpace(script)
	server := client.Server()
	if len(script) == 0 {
		return true
	}
	start := time.Now()
	contextStr := job.StringFor(context)
	client.SetJob(contextStr)
	defer client.ResetJob()
	if verb == executing {
		r.mu.Lock()
		if r.sequence[job] == 0 {
			r.sequence[job] = len(r.sequence) + 1
		}
		printft(start, startTime, "%s %s (%s) (%d/%d)...", strings.Title(verb), contextStr, server.Label(), r.sequence[job], len(r.pending))
		r.mu.Unlock()
	} else {
		printft(start, startTime, "%s %s (%s)...", strings.Title(verb), contextStr, server.Label())
	}
	var dir string
	if context == job.Backend || context == job.Project {
		dir = r.project.RemotePath
	} else {
		dir = filepath.Join(r.project.RemotePath, job.Task.Name)
	}
	if (r.options.Shell || r.options.ShellBefore) && verb == executing {
		printf("Starting shell instead of %s %s...", verb, job)
		err := client.Shell("", dir, r.shellEnv(job, job.Environment))
		if err != nil {
			printf("Error running debug shell: %v", err)
		}
		printf("Continuing...")
		if r.options.Shell {
			return true
		}
	}
	client.SetWarnTimeout(job.WarnTimeoutFor(context))
	client.SetKillTimeout(job.KillTimeoutFor(context))
	_, err := client.Trace(script, dir, job.Environment)
	printft(start, endTime, "")
	if err != nil {
		// Use a different time so it has a different id on Travis, but keep
		// the original start time so the error message shows the task time.
		start = start.Add(1)
		printft(start, startTime|endTime|startFold|endFold, "Error %s %s (%s) : %v", verb, contextStr, server.Label(), err)
		if debug != "" {
			start = time.Now()
			output, err := client.Trace(debug, dir, job.Environment)
			if err != nil {
				printft(start, startTime|endTime|startFold|endFold, "Error debugging %s (%s) : %v", contextStr, server.Label(), err)
			} else if len(output) > 0 {
				printft(start, startTime|endTime|startFold|endFold, "Debug output for %s (%s) : %v", contextStr, server.Label(), outputErr(output, nil))
			}
		}
		if r.options.Debug || r.options.ShellAfter {
			printf("Starting shell to debug...")
			err = client.Shell("", dir, r.shellEnv(job, job.Environment))
			if err != nil {
				printf("Error running debug shell: %v", err)
			}
			printf("Continuing...")
		}
		*abend = r.options.Abend
		return false
	}
	if r.options.ShellAfter && verb == executing {
		printf("Starting shell after %s %s...", verb, job)
		err := client.Shell("", dir, r.shellEnv(job, job.Environment))
		if err != nil {
			printf("Error running debug shell: %v", err)
		}
		printf("Continuing...")
	}

	return true
}

func (r *Runner) shellEnv(job *Job, env *Environment) *Environment {
	senv := env.Copy()
	senv.Set("PS1", `\$SPREAD_BACKEND:\$SPREAD_SYSTEM \${PWD/#\$SPREAD_PATH/...}# `)
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

func (r *Runner) worker(backend *Backend, system *System, order []int) {
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
			if r.sequence[job] == 0 {
				r.sequence[job] = len(r.sequence) + 1
			}
			r.suiteWorkers[suiteWorkersKey(job)]--
		}
		if badProject || abend || !r.tomb.Alive() {
			r.mu.Unlock()
			break
		}
		job = r.job(backend, system, insideSuite, last, order)
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
			} else if !r.run(client, last, restoring, insideSuite, insideSuite.Restore, insideSuite.Debug, &abend) {
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
			if !r.options.Restore && !r.run(client, job, preparing, r.project, r.project.Prepare, r.project.Debug, &abend) {
				r.add(&stats.ProjectPrepareError, job)
				r.add(&stats.TaskAbort, job)
				badProject = true
				continue
			}

			insideBackend = true
			if !r.options.Restore && !r.run(client, job, preparing, backend, backend.Prepare, backend.Debug, &abend) {
				r.add(&stats.BackendPrepareError, job)
				r.add(&stats.TaskAbort, job)
				badProject = true
				continue
			}
		}

		if insideSuite != job.Suite {
			insideSuite = job.Suite
			if !r.options.Restore && !r.run(client, job, preparing, job.Suite, job.Suite.Prepare, job.Suite.Debug, &abend) {
				r.add(&stats.SuitePrepareError, job)
				r.add(&stats.TaskAbort, job)
				badSuite[job.Suite] = true
				continue
			}
		}

		debug := job.Debug()
		for repeat := r.options.Repeat; repeat >= 0; repeat-- {
			if r.options.Restore {
				// Do not prepare or execute, and don't repeat.
				repeat = -1
			} else if !r.options.Restore && !r.run(client, job, preparing, job, job.Prepare(), debug, &abend) {
				r.add(&stats.TaskPrepareError, job)
				r.add(&stats.TaskAbort, job)
				debug = ""
				repeat = -1
			} else if !r.options.Restore && r.run(client, job, executing, job, job.Task.Execute, debug, &abend) {
				r.add(&stats.TaskDone, job)
			} else if !r.options.Restore {
				r.add(&stats.TaskError, job)
				debug = ""
				repeat = -1
			}
			if !abend && !r.options.Restore && repeat <= 0 {
				if err := r.fetchArtifacts(client, job); err != nil {
					printf("Cannot fetch artifacts of %s: %v", job, err)
					r.tomb.Killf("cannot fetch artifacts of %s: %v", job, err)
				}
			}
			if !abend && !r.run(client, job, restoring, job, job.Restore(), debug, &abend) {
				r.add(&stats.TaskRestoreError, job)
				badProject = true
				repeat = -1
			}
		}
	}

	if !abend && insideSuite != nil {
		if !r.run(client, last, restoring, insideSuite, insideSuite.Restore, insideSuite.Debug, &abend) {
			r.add(&stats.SuiteRestoreError, last)
		}
		insideSuite = nil
	}
	if !abend && insideBackend {
		if !r.run(client, last, restoring, backend, backend.Restore, backend.Debug, &abend) {
			r.add(&stats.BackendRestoreError, last)
		}
		insideBackend = false
	}
	if !abend && insideProject {
		if !r.run(client, last, restoring, r.project, r.project.Restore, r.project.Debug, &abend) {
			r.add(&stats.ProjectRestoreError, last)
		}
		insideProject = false
	}
	server := client.Server()
	client.Close()
	if r.options.Reuse {
		r.unreserve(server.Address())
	} else {
		printf("Discarding %s...", server)
		r.discardServer(server)
	}
}

func (r *Runner) job(backend *Backend, system *System, suite *Suite, last *Job, order []int) *Job {
	if last != nil && last.Task.Samples > 1 {
		if job := r.minSampleForTask(last); job != nil {
			return job
		}
	}

	// Find the current top priority for this backend and system.
	var priority int64 = math.MinInt64
	for _, job := range r.pending {
		if job != nil && job.Priority > priority && job.Backend == backend && job.System == system {
			priority = job.Priority
		}
	}

	var best = -1
	var bestWorkers = 1000000
	for _, i := range order {
		job := r.pending[i]
		if job == nil || job.Priority < priority {
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
		if job.Task.Samples > 1 {
			// Worst case it will find the same job.
			return r.minSampleForTask(job)
		}
		r.pending[best] = nil
		return job
	}
	return nil
}

// minSampleForTask finds the job with the lowest sample value sharing
// the same backend, system, and task as the provided job, then removes
// it from the pending list and returns it.
func (r *Runner) minSampleForTask(other *Job) *Job {
	var best = -1
	var bestSample = 1000000
	for i, job := range r.pending {
		if job == nil {
			continue
		}
		if job.Task != other.Task || job.Backend != other.Backend || job.System != other.System {
			continue
		}
		if job.Sample < bestSample {
			best = i
			bestSample = job.Sample
		}
	}
	if best > -1 {
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
			if r.options.Reuse && len(r.reuse.ReuseSystems(system)) > 0 {
				break
			}
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
			printf("Sending project content to %s...", server)
			content, err := r.waitContent()
			if err != nil {
				printf("Discarding %s, cannot send project content: %s", server, err)
				r.discardServer(server)
				client.Close()
				return nil
			}
			if err = client.SendTar(content, r.project.RemotePath); err != nil {
				if reused {
					printf("Cannot send project content to %s: %v", server, err)
				} else {
					printf("Discarding %s, cannot send project content: %s", server, err)
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

func (r *Runner) fetchArtifacts(client *Client, job *Job) error {
	if r.options.Artifacts == "" || len(job.Task.Artifacts) == 0 {
		return nil
	}

	localDir := filepath.Join(r.options.Artifacts, job.Name)
	if err := os.MkdirAll(localDir, 0755); err != nil {
		return fmt.Errorf("cannot create artifacts directory: %v", err)
	}

	tarr, tarw := io.Pipe()

	var stderr bytes.Buffer
	cmd := exec.Command("tar", "xJ")
	cmd.Dir = localDir
	cmd.Stdin = tarr
	cmd.Stderr = &stderr
	err := cmd.Start()
	if err != nil {
		return fmt.Errorf("cannot start unpacking tar: %v", err)
	}

	printf("Fetching artifacts of %s...", job)

	remoteDir := filepath.Join(r.project.RemotePath, job.Task.Name)
	err = client.RecvTar(remoteDir, job.Task.Artifacts, tarw)
	tarw.Close()
	terr := cmd.Wait()

	return firstErr(err, terr)
}

func (r *Runner) discardServer(server Server) {
	if err := server.Discard(r.tomb.Context(nil)); err != nil {
		printf("Error discarding %s: %v", server, err)
	}
	if err := r.reuse.Remove(server); err != nil {
		printf("Error removing %s from reuse file: %v", server, err)
	}
	r.unreserve(server.Address())
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
	if r.options.Discard {
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
		server, err = r.providers[backend.Name].Allocate(r.tomb.Context(nil), system)
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

	// Must reserve before adding to reuse, otherwise it might end up used twice.
	r.reserve(server.Address())

	if err := r.reuse.Add(server, r.options.Password); err != nil {
		printf("Error adding %s to reuse file: %v", server, err)
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

	printf("Connected to %s at %s.", server, server.Address())
	r.servers = append(r.servers, server)
	return client
}

func (r *Runner) unreserve(addr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.reserved[addr] {
		panic(fmt.Errorf("attempting to unreserve a system that is not reserved: %s", addr))
	}
	delete(r.reserved, addr)
}

func (r *Runner) reserve(addr string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reserved[addr] {
		return false
	}
	r.reserved[addr] = true
	return true
}

func (r *Runner) reuseServer(backend *Backend, system *System) *Client {
	provider := r.providers[backend.Name]

	for _, rsystem := range r.reuse.ReuseSystems(system) {
		if !r.reserve(rsystem.Address) {
			continue
		}

		server, err := provider.Reuse(r.tomb.Context(nil), rsystem, system)
		if err != nil {
			printf("Cannot reuse %s at %s: %v", system, rsystem.Address, err)
			continue
		}

		if r.options.Discard {
			printf("Discarding %s...", server)
			r.discardServer(server)
			return nil
		}

		printf("Reusing %s...", server)
		username := rsystem.Username
		password := rsystem.Password
		if username == "" {
			username = "root"
		}
		client, err := Dial(server, username, password)
		if err != nil {
			if r.options.Reuse {
				printf("Cannot reuse %s at %s: %v", system, rsystem.Address, err)
			} else {
				printf("Discarding %s: %v", server, err)
				r.discardServer(server)
			}
			continue
		}

		return client
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

func filterDir(path string) (names []string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	names, err = f.Readdirnames(0)
	if err != nil {
		return nil, err
	}
	var filtered []string
	for _, name := range names {
		if !strings.HasPrefix(name, ".spread-reuse.") {
			filtered = append(filtered, name)
		}
	}
	return filtered, nil
}
