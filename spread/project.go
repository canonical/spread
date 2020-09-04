package spread

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v2"
)

type Project struct {
	Name string `yaml:"project"`

	Backends map[string]*Backend

	Environment *Environment

	Repack      string
	Prepare     string
	Restore     string
	Debug       string
	PrepareEach string `yaml:"prepare-each"`
	RestoreEach string `yaml:"restore-each"`
	DebugEach   string `yaml:"debug-each"`

	Suites map[string]*Suite

	RemotePath string `yaml:"path"`

	Include []string
	Exclude []string
	Rename  []string

	Path string `yaml:"-"`

	WarnTimeout Timeout `yaml:"warn-timeout"`
	KillTimeout Timeout `yaml:"kill-timeout"`
}

func (p *Project) String() string { return "project" }

type Backend struct {
	Name string `yaml:"-"`
	Type string
	Key  string

	// Only for adhoc.
	Allocate string
	Discard  string

	// Only for qemu so far.
	Memory Size

	// Only for Linode and Google so far.
	Plan     string
	Location string
	Storage  Size

	Systems SystemsMap

	Prepare     string
	Restore     string
	Debug       string
	PrepareEach string `yaml:"prepare-each"`
	RestoreEach string `yaml:"restore-each"`
	DebugEach   string `yaml:"debug-each"`

	Environment *Environment
	Variants    []string

	WarnTimeout Timeout `yaml:"warn-timeout"`
	KillTimeout Timeout `yaml:"kill-timeout"`
	HaltTimeout Timeout `yaml:"halt-timeout"`

	Priority OptionalInt
	Manual   bool
}

func (b *Backend) String() string { return fmt.Sprintf("backend %q", b.Name) }

func (b *Backend) systemNames() []string {
	sysnames := make([]string, 0, len(b.Systems))
	for sysname := range b.Systems {
		sysnames = append(sysnames, sysname)
	}
	sort.Strings(sysnames)
	return sysnames
}

type SystemsMap map[string]*System

func (sysmap *SystemsMap) UnmarshalYAML(u func(interface{}) error) error {
	var systems []*System
	if err := u(&systems); err != nil {
		return err
	}
	*sysmap = make(SystemsMap)
	for _, sys := range systems {
		(*sysmap)[sys.Name] = sys
	}
	return nil
}

type System struct {
	Backend string `json:"-"`

	Name     string
	Image    string
	Kernel   string
	Username string
	Password string
	Workers  int

	// Only for Linode and Google so far.
	Storage Size

	// Only for Google so far.
	SecureBoot bool `yaml:"secure-boot"`

	Environment *Environment
	Variants    []string

	Priority OptionalInt
	Manual   bool
}

func (system *System) String() string { return system.Backend + ":" + system.Name }

func (system *System) UnmarshalYAML(u func(interface{}) error) error {
	if err := u(&system.Name); err == nil {
		system.Image = system.Name
		return nil
	}
	type norecurse System
	var def map[string]norecurse
	if err := u(&def); err != nil {
		return err
	}
	for name, sys := range def {
		sys.Name = name
		if sys.Image == "" {
			sys.Image = name
		}
		*system = System(sys)
	}
	return nil
}

type Environment struct {
	err  error
	keys []string
	vals map[string]string
}

func (e *Environment) Keys() []string {
	if e == nil {
		return nil
	}
	return append([]string(nil), e.keys...)
}

func (e *Environment) Copy() *Environment {
	copy := &Environment{}
	copy.err = e.err
	copy.keys = append([]string(nil), e.keys...)
	copy.vals = make(map[string]string)
	for k, v := range e.vals {
		copy.vals[k] = v
	}
	return copy
}

func (e *Environment) Variant(variant string) *Environment {
	env := e.Copy()
NextKey:
	for key, val := range env.vals {
		ekey, evariants := SplitVariants(key)
		for _, evariant := range evariants {
			if evariant == variant {
				env.Replace(key, ekey, val)
				continue NextKey
			}
		}
		if len(evariants) > 0 {
			env.Unset(key)
		}
	}
	return env
}

func (e *Environment) UnmarshalYAML(u func(interface{}) error) error {
	var vals map[string]string
	if err := u(&vals); err != nil {
		return err
	}
	for k := range vals {
		if !varname.MatchString(k) {
			e.err = fmt.Errorf("invalid variable name: %q", k)
			return nil
		}
	}

	var seen = make(map[string]bool)
	var keys = make([]string, len(vals))
	var order yaml.MapSlice
	if err := u(&order); err != nil {
		return err
	}
	for i, item := range order {
		k, ok := item.Key.(string)
		_, good := vals[k]
		if !ok || !good {
			// Shouldn't happen if the regular expression is right.
			e.err = fmt.Errorf("invalid variable name: %v", item.Key)
			return nil
		}
		if seen[k] {
			e.err = fmt.Errorf("variable %q defined multiple times", k)
			return nil
		}
		seen[k] = true
		keys[i] = k
	}
	e.keys = keys
	e.vals = vals
	return nil
}

func NewEnvironment(pairs ...string) *Environment {
	e := &Environment{
		vals: make(map[string]string),
		keys: make([]string, len(pairs)/2),
	}
	for i := 0; i+1 < len(pairs); i += 2 {
		e.vals[pairs[i]] = pairs[i+1]
		e.keys[i/2] = pairs[i]
	}
	return e
}

func (e *Environment) MarshalYAML() (interface{}, error) {
	lines := make([]string, len(e.keys))
	for i := range lines {
		key := e.keys[i]
		lines[i] = key + "=" + e.vals[key]
	}
	return lines, nil
}

func (e *Environment) Unset(key string) {
	l := len(e.vals)
	delete(e.vals, key)
	if len(e.vals) != l {
		for i, k := range e.keys {
			if k == key {
				copy(e.keys[i:], e.keys[i+1:])
				e.keys = e.keys[:len(e.keys)-1]
			}
		}
	}
}

func (e *Environment) Get(key string) string {
	return e.vals[key]
}

func (e *Environment) Set(key, value string) {
	if !varname.MatchString(key) {
		panic("invalid environment variable name: " + key)
	}
	e.Unset(key)
	e.keys = append(e.keys, key)
	e.vals[key] = value
}

func (e *Environment) Replace(oldkey, newkey, value string) {
	if _, ok := e.vals[oldkey]; ok && newkey != oldkey {
		e.Unset(newkey)
		delete(e.vals, oldkey)
		for i, key := range e.keys {
			if key == oldkey {
				e.keys[i] = newkey
				break
			}
		}
	} else if _, ok := e.vals[newkey]; !ok {
		e.keys = append(e.keys, newkey)
	}
	e.vals[newkey] = value
}

type Suite struct {
	Summary  string
	Systems  []string
	Backends []string

	Variants    []string
	Environment *Environment

	Prepare     string
	Restore     string
	Debug       string
	PrepareEach string `yaml:"prepare-each"`
	RestoreEach string `yaml:"restore-each"`
	DebugEach   string `yaml:"debug-each"`

	Name  string           `yaml:"-"`
	Path  string           `yaml:"-"`
	Tasks map[string]*Task `yaml:"-"`

	WarnTimeout Timeout `yaml:"warn-timeout"`
	KillTimeout Timeout `yaml:"kill-timeout"`

	Priority OptionalInt
	Manual   bool
}

func (s *Suite) String() string { return "suite " + s.Name }

type Task struct {
	Suite string `yaml:"-"`

	Summary  string
	Details  string
	Systems  []string
	Backends []string

	Variants    []string
	Environment *Environment
	Samples     int

	Prepare string
	Restore string
	Execute string
	Debug   string

	Artifacts []string

	Name string `yaml:"-"`
	Path string `yaml:"-"`

	WarnTimeout Timeout `yaml:"warn-timeout"`
	KillTimeout Timeout `yaml:"kill-timeout"`

	Priority OptionalInt
	Manual   bool
}

func (t *Task) String() string { return t.Name }

type Job struct {
	Name    string
	Project *Project
	Backend *Backend
	System  *System
	Suite   *Suite
	Task    *Task

	Variant     string
	Environment *Environment
	Sample      int

	Priority int64
}

func (job *Job) String() string {
	return job.Name
}

func (job *Job) StringFor(context interface{}) string {
	switch context {
	case job.Project, job.Backend, job.System:
		return fmt.Sprintf("%s:%s", job.Backend.Name, job.System.Name)
	case job.Suite:
		return fmt.Sprintf("%s:%s:%s", job.Backend.Name, job.System.Name, job.Suite.Name)
	case job.Task:
		return fmt.Sprintf("%s:%s:%s", job.Backend.Name, job.System.Name, job.Task.Name)
	case job:
		return job.Name
	}
	panic(fmt.Errorf("job %s asked to stringify unrelated value: %v", job, context))
}

func (job *Job) Prepare() string {
	return join(job.Project.PrepareEach, job.Backend.PrepareEach, job.Suite.PrepareEach, job.Task.Prepare)
}

func (job *Job) Restore() string {
	return join(job.Task.Restore, job.Suite.RestoreEach, job.Backend.RestoreEach, job.Project.RestoreEach)
}

func (job *Job) Debug() string {
	return join(job.Task.Debug, job.Suite.DebugEach, job.Backend.DebugEach, job.Project.DebugEach)
}

func (job *Job) WarnTimeoutFor(context interface{}) time.Duration {
	touts := []Timeout{job.Task.WarnTimeout, job.Suite.WarnTimeout, job.Backend.WarnTimeout, job.Project.WarnTimeout}
	return job.timeoutFor("warn", context, touts)
}

func (job *Job) KillTimeoutFor(context interface{}) time.Duration {
	touts := []Timeout{job.Task.KillTimeout, job.Suite.KillTimeout, job.Backend.KillTimeout, job.Project.KillTimeout}
	return job.timeoutFor("kill", context, touts)
}

func (job *Job) timeoutFor(which string, context interface{}, touts []Timeout) time.Duration {
	switch context {
	case job:
	case job.Task:
	case job.Suite:
		touts = touts[1:]
	case job.Backend:
		touts = touts[2:]
	case job.Project:
		touts = touts[3:]
	default:
		panic(fmt.Errorf("job %s asked for %s-timeout of unrelated value: %v", job, which, context))
	}
	for _, tout := range touts {
		if tout.Duration != 0 {
			return tout.Duration
		}
	}
	return 0
}

func join(scripts ...string) string {
	var buf bytes.Buffer
	for _, script := range scripts {
		if len(script) == 0 {
			continue
		}
		if buf.Len() > 0 {
			buf.WriteString("\n\n")
		}
		buf.WriteString("(\n")
		buf.WriteString(script)
		buf.WriteString("\n)")
	}
	return buf.String()
}

type jobsByName []*Job

func (jobs jobsByName) Len() int      { return len(jobs) }
func (jobs jobsByName) Swap(i, j int) { jobs[i], jobs[j] = jobs[j], jobs[i] }
func (jobs jobsByName) Less(i, j int) bool {
	ji, jj := jobs[i], jobs[j]
	if ji.Backend == jj.Backend && ji.System == jj.System && ji.Task == jj.Task {
		return ji.Sample < jj.Sample
	}
	return ji.Name < jj.Name
}

func SplitVariants(s string) (prefix string, variants []string) {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[:i], strings.Split(s[i+1:], ",")
	}
	return s, nil
}

var (
	validName   = regexp.MustCompile("^[a-z0-9]+(?:[-._][a-z0-9]+)*$")
	validSystem = regexp.MustCompile("^[a-z*]+-[a-z0-9*]+(?:[-.][a-z0-9*]+)*$")
	validSuite  = regexp.MustCompile("^(?:[a-z0-9]+(?:[-._][a-z0-9]+)*/)+$")
	validTask   = regexp.MustCompile("^(?:[a-z0-9]+(?:[-._][a-z0-9]+)*/)+[a-z0-9]+(?:[-._][a-z0-9]+)*$")
)

func Load(path string) (*Project, error) {
	filename, data, err := readProject(path)
	if err != nil {
		return nil, fmt.Errorf("cannot load project file from %s: %v", path, err)
	}

	project := &Project{}
	err = yaml.Unmarshal(data, project)
	if err != nil {
		return nil, fmt.Errorf("cannot load %s: %v", filename, err)
	}

	if !validName.MatchString(project.Name) {
		return nil, fmt.Errorf("invalid project name: %q", project.Name)
	}
	if project.RemotePath == "" {
		return nil, fmt.Errorf("missing project path field with remote project location")
	}

	project.Path = filepath.Dir(filename)

	project.Repack = strings.TrimSpace(project.Repack)
	project.Prepare = strings.TrimSpace(project.Prepare)
	project.Restore = strings.TrimSpace(project.Restore)
	project.Debug = strings.TrimSpace(project.Debug)
	project.PrepareEach = strings.TrimSpace(project.PrepareEach)
	project.RestoreEach = strings.TrimSpace(project.RestoreEach)
	project.DebugEach = strings.TrimSpace(project.DebugEach)

	if err := checkEnv(project, &project.Environment); err != nil {
		return nil, err
	}

	for bname, backend := range project.Backends {
		if !validName.MatchString(bname) {
			return nil, fmt.Errorf("invalid backend name: %q", bname)
		}
		if backend == nil {
			delete(project.Backends, bname)
			continue
		}
		backend.Name = bname
		if backend.Type == "" {
			backend.Type = bname
		}
		switch backend.Type {
		case "google", "linode", "lxd", "qemu", "adhoc", "humbox":
		default:
			return nil, fmt.Errorf("%s has unsupported type %q", backend, backend.Type)
		}

		if backend.Type != "adhoc" && (backend.Allocate != "" || backend.Discard != "") {
			return nil, fmt.Errorf("%s cannot use allocate and dispose fields", backend)
		}
		if backend.Type == "adhoc" && strings.TrimSpace(backend.Allocate) == "" {
			return nil, fmt.Errorf("%s requires an allocate field", backend)
		}

		backend.Prepare = strings.TrimSpace(backend.Prepare)
		backend.Restore = strings.TrimSpace(backend.Restore)
		backend.Debug = strings.TrimSpace(backend.Debug)
		backend.PrepareEach = strings.TrimSpace(backend.PrepareEach)
		backend.RestoreEach = strings.TrimSpace(backend.RestoreEach)
		backend.DebugEach = strings.TrimSpace(backend.DebugEach)

		for sysname, system := range backend.Systems {
			system.Backend = backend.Name
			if system.Workers < 0 {
				return nil, fmt.Errorf("%s has system %q with %d workers", backend, sysname, system.Workers)
			}
			if system.Workers == 0 {
				system.Workers = 1
			}
			if system.Storage == 0 {
				system.Storage = backend.Storage
			}
			if err := checkEnv(system, &system.Environment); err != nil {
				return nil, err
			}
		}
		sort.Strings(backend.Variants)

		if err := checkEnv(backend, &backend.Environment); err != nil {
			return nil, err
		}
		if err = checkSystems(backend, backend.systemNames()); err != nil {
			return nil, err
		}
		if len(backend.Systems) == 0 {
			return nil, fmt.Errorf("no systems specified for %s", backend)
		}
	}

	if len(project.Backends) == 0 {
		return nil, fmt.Errorf("must define at least one backend")
	}
	if len(project.Suites) == 0 {
		return nil, fmt.Errorf("must define at least one task suite")
	}

	orig := project.Suites
	project.Suites = make(map[string]*Suite)
	for sname, suite := range orig {
		if suite == nil {
			suite = &Suite{}
		}
		if !strings.HasSuffix(sname, "/") {
			return nil, fmt.Errorf("invalid suite name (must end with /): %q", sname)
		}
		if !validSuite.MatchString(sname) {
			return nil, fmt.Errorf("invalid suite name: %q", sname)
		}
		sname = strings.Trim(sname, "/")
		suite.Name = sname + "/"
		suite.Path = filepath.Join(project.Path, sname)
		suite.Summary = strings.TrimSpace(suite.Summary)
		suite.Prepare = strings.TrimSpace(suite.Prepare)
		suite.Restore = strings.TrimSpace(suite.Restore)
		suite.Debug = strings.TrimSpace(suite.Debug)
		suite.PrepareEach = strings.TrimSpace(suite.PrepareEach)
		suite.RestoreEach = strings.TrimSpace(suite.RestoreEach)
		suite.DebugEach = strings.TrimSpace(suite.DebugEach)

		project.Suites[suite.Name] = suite

		if suite.Summary == "" {
			return nil, fmt.Errorf("%s is missing a summary", suite)
		}

		if err := checkEnv(suite, &suite.Environment); err != nil {
			return nil, err
		}
		if err := checkSystems(suite, suite.Systems); err != nil {
			return nil, err
		}

		f, err := os.Open(suite.Path)
		if err != nil {
			return nil, fmt.Errorf("cannot list %s: %v", suite, err)
		}

		tnames, err := f.Readdirnames(0)
		if err != nil {
			return nil, fmt.Errorf("cannot list %s: %v", suite, err)
		}

		suite.Tasks = make(map[string]*Task)
		for _, tname := range tnames {
			tfilename := filepath.Join(suite.Path, tname, "task.yaml")
			if fi, _ := os.Stat(filepath.Dir(tfilename)); !fi.IsDir() {
				continue
			}
			tdata, err := ioutil.ReadFile(tfilename)
			if os.IsNotExist(err) {
				debugf("Skipping %s/%s: task.yaml missing", sname, tname)
				continue
			}
			if err != nil {
				return nil, err
			}

			task := &Task{}
			err = yaml.Unmarshal(tdata, &task)
			if err != nil {
				return nil, fmt.Errorf("cannot load %s/%s/task.yaml: %v", sname, tname, err)
			}

			task.Suite = suite.Name
			task.Name = suite.Name + tname
			task.Path = filepath.Dir(tfilename)
			task.Summary = strings.TrimSpace(task.Summary)
			task.Prepare = strings.TrimSpace(task.Prepare)
			task.Restore = strings.TrimSpace(task.Restore)
			task.Debug = strings.TrimSpace(task.Debug)
			if !validTask.MatchString(task.Name) {
				return nil, fmt.Errorf("invalid task name: %q", task.Name)
			}
			if task.Summary == "" {
				return nil, fmt.Errorf("%s is missing a summary", task)
			}
			if task.Samples == 0 {
				task.Samples = 1
			}

			if err := checkEnv(task, &task.Environment); err != nil {
				return nil, err
			}
			if err := checkSystems(task, task.Systems); err != nil {
				return nil, err
			}

			for _, fname := range task.Artifacts {
				if filepath.IsAbs(fname) || fname != filepath.Clean(fname) || strings.HasPrefix(fname, "../") {
					return nil, fmt.Errorf("%s has improper artifact path: %s", task.Name, fname)
				}
			}

			suite.Tasks[tname] = task
		}
	}

	debugf("Loaded project: %# v", project)
	return project, nil
}

func readProject(path string) (filename string, data []byte, err error) {
	path, err = filepath.Abs(path)
	if err != nil {
		return "", nil, fmt.Errorf("cannot get absolute path for %s: %v", path, err)
	}

	for {
		filename = filepath.Join(path, "spread.yaml")
		debugf("Trying to read %s...", filename)
		data, err = ioutil.ReadFile(filename)
		if os.IsNotExist(err) {
			filename = filepath.Join(path, ".spread.yaml")
			debugf("Trying to read %s...", filename)
			data, err = ioutil.ReadFile(filename)
		}
		if err == nil {
			logf("Found %s.", filename)
			return filename, data, nil
		}
		newpath := filepath.Dir(path)
		if newpath == path {
			break
		}
		path = newpath
	}
	return "", nil, fmt.Errorf("cannot find spread.yaml or .spread.yaml")
}

func checkEnv(context fmt.Stringer, env **Environment) error {
	if *env == nil {
		*env = NewEnvironment()
	} else if (*env).err != nil {
		return fmt.Errorf("invalid %s environment: %s", context, (*env).err)
	}
	return nil
}

func checkSystems(context fmt.Stringer, systems []string) error {
	for _, system := range systems {
		if strings.HasPrefix(system, "+") || strings.HasPrefix(system, "-") {
			system = system[1:]
		}
		if !validSystem.MatchString(system) {
			return fmt.Errorf("%s refers to invalid system name: %q", context, system)
		}
	}
	return nil
}

type Filter interface {
	Pass(job *Job) bool
}

type filterExp struct {
	regexp      *regexp.Regexp
	firstSample int
	lastSample  int
}

type filter struct {
	exps []*filterExp
}

func (f *filter) Pass(job *Job) bool {
	if len(f.exps) == 0 {
		return true
	}
	for _, exp := range f.exps {
		if exp.firstSample > 0 {
			if job.Sample < exp.firstSample {
				continue
			}
			if job.Sample > exp.lastSample {
				continue
			}
		}
		if exp.regexp.MatchString(job.Name) {
			return true
		}
	}
	return false
}

func NewFilter(args []string) (Filter, error) {
	var dots = regexp.MustCompile(`\.+|:+|#`)
	var sample = regexp.MustCompile(`^(.*)#(\d+)(?:\.\.(\d+))?$`)
	var err error
	var exps []*filterExp
	for _, arg := range args {
		var argre = arg
		var firstSample, lastSample int
		if m := sample.FindStringSubmatch(argre); len(m) > 0 {
			argre = m[1]
			firstSample, err = strconv.Atoi(m[2])
			if err == nil && m[3] != "" {
				lastSample, err = strconv.Atoi(m[3])
			}
			if err != nil {
				panic(fmt.Sprintf("internal error: regexp matched non-int on %q", arg))
			}
			if firstSample > 0 && lastSample == 0 {
				lastSample = firstSample
			}
			if firstSample < 1 || lastSample < firstSample {
				return nil, fmt.Errorf("invalid sample range in filter string: %q", arg)
			}
		}
		argre = dots.ReplaceAllStringFunc(argre, func(s string) string {
			switch s {
			case ".":
				return `\.`
			case "...":
				return `[^:]*`
			case ":":
				return "(:.+)*:(.+:)*"
			case "#":
				// Error below. Should have been parsed above.
			}
			err = fmt.Errorf("invalid filter string: %q", s)
			return s
		})
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(argre, "(:.+)*:") || strings.HasPrefix(argre, "/") {
			argre = ".+" + argre
		}
		if strings.HasSuffix(argre, ":(.+:)*") || strings.HasSuffix(argre, "/") {
			argre = argre + ".+"
		}
		exp, err := regexp.Compile("(?:^|:)" + argre + "(?:$|[:#])")
		if err != nil {
			return nil, fmt.Errorf("invalid filter string: %q", arg)
		}
		exps = append(exps, &filterExp{
			regexp:      exp,
			firstSample: firstSample,
			lastSample:  lastSample,
		})

	}
	return &filter{exps}, nil
}

func (p *Project) backendNames() []string {
	bnames := make([]string, 0, len(p.Backends))
	for bname := range p.Backends {
		bnames = append(bnames, bname)
	}
	return bnames
}

func (p *Project) Jobs(options *Options) ([]*Job, error) {
	var jobs []*Job

	hasFilter := options.Filter != nil
	manualBackends := hasFilter
	manualSystems := hasFilter
	manualSuites := hasFilter
	manualTasks := hasFilter

	cmdcache := make(map[string]string)
	penv := envmap{p, p.Environment}
	pevr := strmap{p, evars(p.Environment, "")}
	pbke := strmap{p, p.backendNames()}

	value, err := evalone("remote project path", p.RemotePath, cmdcache, true, penv)
	if err != nil {
		return nil, err
	}
	p.RemotePath = filepath.Clean(value)
	if !filepath.IsAbs(p.RemotePath) || filepath.Dir(p.RemotePath) == p.RemotePath {
		return nil, fmt.Errorf("remote project path must be absolute and not /: %s", p.RemotePath)
	}

	for _, suite := range p.Suites {
		senv := envmap{suite, suite.Environment}
		sevr := strmap{suite, evars(suite.Environment, "+")}
		svar := strmap{suite, suite.Variants}
		sbke := strmap{suite, suite.Backends}
		ssys := strmap{suite, suite.Systems}

		for _, task := range suite.Tasks {
			tenv := envmap{task, task.Environment}
			tevr := strmap{task, evars(task.Environment, "+")}
			tvar := strmap{task, task.Variants}
			tbke := strmap{task, task.Backends}
			tsys := strmap{task, task.Systems}

			backends, err := evalstr("backends", pbke, sbke, tbke)
			if err != nil {
				return nil, err
			}

			for _, bname := range backends {
				backend := p.Backends[bname]
				benv := envmap{backend, backend.Environment}
				bevr := strmap{backend, evars(backend.Environment, "+")}
				bvar := strmap{backend, backend.Variants}
				bsys := strmap{backend, backend.systemNames()}

				systems, err := evalstr("systems", bsys, ssys, tsys)
				if err != nil {
					return nil, err
				}

				for _, sysname := range systems {
					system := backend.Systems[sysname]
					// not for us
					if system == nil {
						continue
					}
					yenv := envmap{system, system.Environment}
					yevr := strmap{system, evars(system.Environment, "+")}
					yvar := strmap{system, system.Variants}

					priority := evaloint(task.Priority, suite.Priority, system.Priority, backend.Priority)

					strmaps := []strmap{pevr, bevr, bvar, yevr, yvar, sevr, svar, tevr, tvar}
					variants, err := evalstr("variants", strmaps...)
					if err != nil {
						return nil, err
					}

					for _, variant := range variants {
						if variant == "" && len(variants) > 1 {
							continue
						}

						for sample := 1; sample <= task.Samples; sample++ {
							job := &Job{
								Project:  p,
								Backend:  backend,
								System:   system,
								Suite:    p.Suites[task.Suite],
								Task:     task,
								Variant:  variant,
								Sample:   sample,
								Priority: priority,
							}
							if job.Variant == "" {
								job.Name = fmt.Sprintf("%s:%s:%s", job.Backend.Name, job.System.Name, job.Task.Name)
							} else {
								job.Name = fmt.Sprintf("%s:%s:%s:%s", job.Backend.Name, job.System.Name, job.Task.Name, job.Variant)
							}
							if task.Samples > 1 {
								job.Name += "#" + strconv.Itoa(sample)
							}

							sprenv := envmap{stringer("$SPREAD_*"), NewEnvironment(
								"SPREAD_JOB", job.Name,
								"SPREAD_PROJECT", job.Project.Name,
								"SPREAD_PATH", job.Project.RemotePath,
								"SPREAD_BACKEND", job.Backend.Name,
								"SPREAD_SYSTEM", job.System.Name,
								"SPREAD_SUITE", job.Suite.Name,
								"SPREAD_TASK", job.Task.Name,
								"SPREAD_VARIANT", job.Variant,
								"SPREAD_SAMPLE", strconv.Itoa(job.Sample),
							)}

							env, err := evalenv(cmdcache, true, sprenv, penv, benv, yenv, senv, tenv)
							if err != nil {
								return nil, err
							}
							job.Environment = env.Variant(variant)

							if options.Filter != nil && !options.Filter.Pass(job) {
								continue
							}

							jobs = append(jobs, job)

							if !job.Backend.Manual {
								manualBackends = false
							}
							if !job.System.Manual {
								manualSystems = false
							}
							if !job.Suite.Manual {
								manualSuites = false
							}
							if !job.Task.Manual {
								manualTasks = false
							}
						}
					}
				}

			}
		}
	}

	all := jobs
	jobs = make([]*Job, 0, len(all))
	backends := make(map[string]bool)
	for _, job := range all {
		if !manualBackends && job.Backend.Manual {
			continue
		}
		if !manualSystems && job.System.Manual {
			continue
		}
		if !manualSuites && job.Suite.Manual {
			continue
		}
		if !manualTasks && job.Task.Manual {
			continue
		}
		jobs = append(jobs, job)
		backends[job.Backend.Name] = true
	}

	env, err := evalenv(cmdcache, true, penv)
	if err != nil {
		return nil, err
	}
	p.Environment = env
	p.Environment.Set("SPREAD_BACKENDS", strings.Join(sortedKeys(backends), " "))

	// TODO Should probably cascade environments, so that backend.Environment contains
	// project.Environment, and suite.Environmnet contains both project.Environment and
	// backend.Environment, etc. This would make logic such as the one below saner.
	// Also, should probably have Enviornment.Evaluate instead of evalone and evalenv.

	for bname, backend := range p.Backends {
		benv := envmap{backend, backend.Environment}
		value, err := evalone(bname+" backend key", backend.Key, cmdcache, true, penv, benv)
		if err != nil {
			return nil, err
		}
		backend.Key = strings.TrimSpace(value)

		for _, system := range backend.Systems {
			if system.Username != "" {
				value, err := evalone(system.String()+" username", system.Username, cmdcache, false, penv, benv)
				if err != nil {
					return nil, err
				}
				system.Username = value
			}
			// Passwords may easily include $, so only replace if it matches a varname entirely.
			if system.Password != "" && varref.FindString(system.Password) == system.Password {
				value, err := evalone(system.String()+" password", system.Password, cmdcache, false, penv, benv)
				if err != nil {
					return nil, err
				}
				system.Password = value
			}
		}
	}

	err1 := evalslice("rename expression", p.Rename, cmdcache, false, penv)
	err2 := evalslice("include list", p.Include, cmdcache, false, penv)
	err3 := evalslice("exclude list", p.Exclude, cmdcache, false, penv)
	if err := firstErr(err1, err2, err3); err != nil {
		return nil, err
	}

	if len(jobs) == 0 {
		if options.Filter != nil {
			return nil, fmt.Errorf("nothing matches provider filter")
		} else {
			return nil, fmt.Errorf("cannot find any tasks")
		}
	}

	sort.Sort(jobsByName(jobs))

	return jobs, nil
}

func evars(env *Environment, prefix string) []string {
	keys := env.Keys()
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		_, variants := SplitVariants(key)
		for _, variant := range variants {
			seen[variant] = true
		}
	}
	variants := make([]string, 0, len(seen)+1)
	for variant := range seen {
		variants = append(variants, prefix+variant)
	}
	if len(variants) == 0 && len(prefix) == 0 {
		// Ensure there's at least one variant at the end.
		// This must be dropped if other variants are found.
		variants = append(variants, "")
	}
	sort.Strings(variants)
	return variants
}

type envmap struct {
	context fmt.Stringer
	env     *Environment
}

type stringer string

func (s stringer) String() string { return string(s) }

var (
	varname = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*(?:/[a-zA-Z0-9_]+(?:,[a-zA-Z0-9_]+)*)?$`)
	varcmd  = regexp.MustCompile(`\$\(HOST:.+?\)`)
	varref  = regexp.MustCompile(`\$(?:\(HOST:.+?\)|[a-zA-Z_][a-zA-Z0-9_]*|\{[a-zA-Z_][a-zA-Z0-9_]*\})`)
)

func evalslice(context string, values []string, cmdcache map[string]string, hostOnly bool, maps ...envmap) error {
	for i, value := range values {
		value, err := evalone(context, value, cmdcache, hostOnly, maps...)
		if err != nil {
			return err
		}
		values[i] = value
	}
	return nil
}

func evalone(context string, value string, cmdcache map[string]string, hostOnly bool, maps ...envmap) (string, error) {
	const key = "SPREAD_INTERNAL_EVAL"
	m := envmap{stringer(context), NewEnvironment(key, value)}
	out, err := evalenv(cmdcache, hostOnly, append(maps, m)...)
	if err != nil {
		return "", err
	}
	return out.Get(key), nil
}

func evalenv(cmdcache map[string]string, hostOnly bool, maps ...envmap) (*Environment, error) {
	result := NewEnvironment()
	for _, m := range maps {
		for _, key := range m.env.Keys() {
			var failed error
			varexp := varref
			if hostOnly {
				varexp = varcmd
			}
			value := varexp.ReplaceAllStringFunc(m.env.Get(key), func(ref string) string {
				if failed != nil {
					return ""
				}
				if !strings.HasPrefix(ref, "$(") {
					return result.Get(strings.Trim(ref, "${}"))
				}
				inner := ref[len("$(HOST:") : len(ref)-len(")")]
				if output, ok := cmdcache[inner]; ok {
					return output
				}
				output, err := evalcmd(inner)
				if err != nil && failed == nil {
					if key == "" {
						failed = fmt.Errorf("%s in %s returned error: %v", ref, m.context, err)
					} else {
						failed = fmt.Errorf("%s in %s environment returned error: %v", ref, m.context, err)
					}
					return ""
				}
				cmdcache[inner] = output
				return output
			})
			if failed != nil {
				return nil, failed
			}
			result.Set(key, value)
		}
	}
	return result, nil
}

func evalcmd(cmdline string) (string, error) {
	var stderr bytes.Buffer
	cmd := exec.Command("/bin/bash", "-c", cmdline)
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		msg := string(bytes.TrimSpace(stderr.Bytes()))
		if len(msg) > 0 {
			msgs := strings.Split(msg, "\n")
			return "", fmt.Errorf("%s", strings.Join(msgs, "; "))
		}
		return "", err
	}
	return string(bytes.TrimRight(output, "\n")), nil
}

type strmap struct {
	context fmt.Stringer
	strings []string
}

func matches(pattern string, strmaps ...strmap) ([]string, error) {
	var matches []string
	for _, strmap := range strmaps {
		for _, name := range strmap.strings {
			if strings.HasPrefix(name, "+") || strings.HasPrefix(name, "-") {
				name = name[1:]
			}
			m, err := filepath.Match(pattern, name)
			if err != nil {
				return nil, err
			}
			if m {
				matches = append(matches, name)
			}
		}
	}
	return matches, nil
}

func evalstr(what string, strmaps ...strmap) ([]string, error) {
	final := make(map[string]bool)
	for i, strmap := range strmaps {
		delta := 0
		plain := 0
		for j, name := range strmap.strings {
			add := strings.HasPrefix(name, "+")
			remove := strings.HasPrefix(name, "-")
			if add || remove {
				name = name[1:]
				if i == 0 {
					return nil, fmt.Errorf("%s specifies %s in delta format", strmap.context, what)
				}
				delta++
			} else {
				plain++
			}
			if delta > 0 && plain > 0 {
				return nil, fmt.Errorf("%s specifies %s both in delta and plain format", strmap.context, what)
			}
			matches, err := matches(name, strmaps[:i+1]...)
			if err != nil {
				return nil, err
			}

			if add {
				for _, match := range matches {
					final[match] = true
				}
				continue
			}
			if remove {
				for _, match := range matches {
					delete(final, match)
				}
				continue
			}

			if j == 0 && len(final) > 0 {
				for name := range final {
					delete(final, name)
				}
			}

			for _, match := range matches {
				final[match] = true
			}
		}
	}

	strs := make([]string, 0, len(final))
	for name := range final {
		strs = append(strs, name)
	}
	return strs, nil
}

func evaloint(values ...OptionalInt) int64 {
	for _, v := range values {
		if v.IsSet {
			return v.Value
		}
	}
	return 0
}

type Timeout struct {
	time.Duration
}

func (t *Timeout) UnmarshalYAML(u func(interface{}) error) error {
	var s string
	_ = u(&s)
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("timeout must look like 10s or 15m or 1.5h, not %q", s)
	}
	t.Duration = d
	return nil
}

type Size int64

const (
	kb = 1024
	mb = kb * 1024
	gb = mb * 1024
)

func (s Size) String() string {
	switch {
	case s > gb && s%gb == 0:
		return strconv.Itoa(int(s/gb)) + "G"
	case s > mb && s%mb == 0:
		return strconv.Itoa(int(s/mb)) + "M"
	case s > kb && s%kb == 0:
		return strconv.Itoa(int(s/mb)) + "K"
	}
	return strconv.Itoa(int(s)) + "B"
}

func (s *Size) UnmarshalYAML(u func(interface{}) error) error {
	var str string
	_ = u(&str)
	if len(str) == 0 {
		*s = 0
		return nil
	}
	if str == "preserve-size" {
		*s = -1
		return nil
	}
	n, err := strconv.Atoi(str[:len(str)-1])
	if err != nil {
		return fmt.Errorf("invalid size string: %q", str)
	}
	switch str[len(str)-1] {
	case 'G':
		*s = Size(gb * n)
	case 'M':
		*s = Size(mb * n)
	case 'K':
		*s = Size(kb * n)
	case 'B':
		*s = Size(n)
	default:
		return fmt.Errorf("unknown size suffix in %q, must be one of: B, K, M, G", str)
	}
	return nil
}

type OptionalInt struct {
	IsSet bool
	Value int64
}

func (s OptionalInt) String() string {
	return strconv.FormatInt(s.Value, 64)
}

func (s *OptionalInt) UnmarshalYAML(u func(interface{}) error) error {
	var value int64
	err := u(&value)
	if err != nil {
		return err
	}
	s.Value = value
	s.IsSet = true
	return nil
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, len(m))
	i := 0
	for key := range m {
		keys[i] = key
		i++
	}
	sort.Strings(keys)
	return keys
}
