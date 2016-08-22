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
	"strings"

	"gopkg.in/yaml.v2"
	"time"
)

type Project struct {
	Name string `yaml:"project"`

	Backends map[string]*Backend

	Environment *Environment

	Prepare     string
	Restore     string
	PrepareEach string `yaml:"prepare-each"`
	RestoreEach string `yaml:"restore-each"`

	Suites map[string]*Suite

	RemotePath string `yaml:"path"`

	Include []string
	Exclude []string

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

	Systems SystemsMap

	Prepare     string
	Restore     string
	PrepareEach string `yaml:"prepare-each"`
	RestoreEach string `yaml:"restore-each"`

	Environment *Environment
	Variants    []string

	WarnTimeout Timeout `yaml:"warn-timeout"`
	KillTimeout Timeout `yaml:"kill-timeout"`
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

	Environment *Environment
	Variants    []string
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
	PrepareEach string `yaml:"prepare-each"`
	RestoreEach string `yaml:"restore-each"`

	Name  string           `yaml:"-"`
	Path  string           `yaml:"-"`
	Tasks map[string]*Task `yaml:"-"`

	WarnTimeout Timeout `yaml:"warn-timeout"`
	KillTimeout Timeout `yaml:"kill-timeout"`
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

	Prepare string
	Restore string
	Execute string

	Disable string

	Name string `yaml:"-"`
	Path string `yaml:"-"`

	WarnTimeout Timeout `yaml:"warn-timeout"`
	KillTimeout Timeout `yaml:"kill-timeout"`
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
}

func (job *Job) String() string {
	return job.Name
}

func (job *Job) StringFor(context interface{}) string {
	switch context {
	case job.Project:
		return fmt.Sprintf("project on %s:%s", job.Backend.Name, job.System)
	case job.Backend, job.System:
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

func SplitVariants(s string) (prefix string, variants []string) {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[:i], strings.Split(s[i+1:], ",")
	}
	return s, nil
}

var (
	validName   = regexp.MustCompile("^[a-z0-9]+(?:[-._][a-z0-9]+)*$")
	validSystem = regexp.MustCompile("^[a-z]+-[a-z0-9]+(?:[-.][a-z0-9]+)*$")
	validSuite  = regexp.MustCompile("^(?:[a-z0-9]+(?:[-._][a-z0-9]+)*/)+$")
	validTask   = regexp.MustCompile("^(?:[a-z0-9]+(?:[-._][a-z0-9]+)*/)+[a-z0-9]+(?:[-._][a-z0-9]+)*$")
)

func Load(path string) (*Project, error) {
	filename, data, err := readProject(path)

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
	if project.Include == nil {
		project.Include = []string{"."}
	}

	project.Path = filepath.Dir(filename)

	project.Prepare = strings.TrimSpace(project.Prepare)
	project.Restore = strings.TrimSpace(project.Restore)
	project.PrepareEach = strings.TrimSpace(project.PrepareEach)
	project.RestoreEach = strings.TrimSpace(project.RestoreEach)

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
		case "linode", "lxd", "qemu", "adhoc":
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
		backend.PrepareEach = strings.TrimSpace(backend.PrepareEach)
		backend.RestoreEach = strings.TrimSpace(backend.RestoreEach)

		for sysname, system := range backend.Systems {
			system.Backend = backend.Name
			if system.Workers < 0 {
				return nil, fmt.Errorf("%s has system %q with %d workers", backend, sysname, system.Workers)
			}
			if system.Workers == 0 {
				system.Workers = 1
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
		suite.PrepareEach = strings.TrimSpace(suite.PrepareEach)
		suite.RestoreEach = strings.TrimSpace(suite.RestoreEach)

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
			if !validTask.MatchString(task.Name) {
				return nil, fmt.Errorf("invalid task name: %q", task.Name)
			}
			if task.Summary == "" {
				return nil, fmt.Errorf("%s is missing a summary", task)
			}

			if err := checkEnv(task, &task.Environment); err != nil {
				return nil, err
			}
			if err := checkSystems(task, task.Systems); err != nil {
				return nil, err
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

type filter struct {
	exps []*regexp.Regexp
}

func (f *filter) Pass(job *Job) bool {
	if len(f.exps) == 0 {
		return true
	}
	for _, exp := range f.exps {
		if exp.MatchString(job.Name) {
			return true
		}
	}
	return false
}

var dots = regexp.MustCompile(`\.+|:+`)

func NewFilter(args []string) (Filter, error) {
	var err error
	var exps []*regexp.Regexp
	for _, arg := range args {
		arg = dots.ReplaceAllStringFunc(arg, func(s string) string {
			switch s {
			case ".":
				return `\.`
			case "...":
				return `[^:]*`
			case ":":
				return "(:.+)*:(.+:)*"
			}
			err = fmt.Errorf("invalid filter string: %q", s)
			return s
		})
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(arg, "(:.+)*:") || strings.HasPrefix(arg, "/") {
			arg = ".+" + arg
		}
		if strings.HasSuffix(arg, ":(.+:)*") || strings.HasSuffix(arg, "/") {
			arg = arg + ".+"
		}
		exp, err := regexp.Compile("(?:^|:)" + arg + "(?:$|:)")
		if err != nil {
			return nil, fmt.Errorf("invalid filter string: %q", arg)
		}
		exps = append(exps, exp)

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

	cmdcache := make(map[string]string)
	penv := envmap{p, p.Environment}
	pevr := strmap{p, evars(p.Environment, "")}
	pbke := strmap{p, p.backendNames()}

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

					strmaps := []strmap{pevr, bevr, bvar, yevr, yvar, sevr, svar, tevr, tvar}
					variants, err := evalstr("variants", strmaps...)
					if err != nil {
						return nil, err
					}

					for _, variant := range variants {
						if variant == "" && len(variants) > 1 {
							continue
						}

						job := &Job{
							Project: p,
							Backend: backend,
							System:  system,
							Suite:   p.Suites[task.Suite],
							Task:    task,
							Variant: variant,
						}
						if job.Variant == "" {
							job.Name = fmt.Sprintf("%s:%s:%s", job.Backend.Name, job.System.Name, job.Task.Name)
						} else {
							job.Name = fmt.Sprintf("%s:%s:%s:%s", job.Backend.Name, job.System.Name, job.Task.Name, job.Variant)
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
						)}

						env, err := evalenv(cmdcache, false, penv, benv, yenv, senv, tenv, sprenv)
						if err != nil {
							return nil, err
						}
					NextKey:
						for _, key := range env.Keys() {
							ekey, evariants := SplitVariants(key)
							for _, evariant := range evariants {
								if evariant == variant {
									env.Replace(key, ekey, env.Get(key))
									continue NextKey
								}
							}
							if len(evariants) > 0 {
								env.Unset(key)
							}
						}
						job.Environment = env

						if options.Filter != nil && !options.Filter.Pass(job) {
							continue
						}
						jobs = append(jobs, job)
					}
				}

			}
		}
	}

	value, err := evalone("remote project path", p.RemotePath, cmdcache, penv)
	if err != nil {
		return nil, err
	}
	p.RemotePath = filepath.Clean(value)
	if !filepath.IsAbs(p.RemotePath) || filepath.Dir(p.RemotePath) == p.RemotePath {
		return nil, fmt.Errorf("remote project path must be absolute and not /: %s", p.RemotePath)
	}

	for bname, backend := range p.Backends {
		benv := envmap{backend, backend.Environment}
		value, err := evalone(bname+" backend key", backend.Key, cmdcache, penv, benv)
		if err != nil {
			return nil, err
		}
		backend.Key = strings.TrimSpace(value)
	}

	if len(jobs) == 0 {
		if options.Filter != nil {
			return nil, fmt.Errorf("nothing matches provider filter")
		} else {
			return nil, fmt.Errorf("cannot find any tasks")
		}
	}

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
	varcmd  = regexp.MustCompile(`\$\(HOST:.+?\)`)
	varname = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*(?:/[a-zA-Z0-9_]+)?$`)
)

func evalone(context string, value string, cmdcache map[string]string, maps ...envmap) (string, error) {
	const key = "SPREAD_INTERNAL_EVAL"
	m := envmap{stringer(context), NewEnvironment(key, value)}
	out, err := evalenv(cmdcache, false, append(maps, m)...)
	if err != nil {
		return "", err
	}
	return out.Get(key), nil
}

func evalenv(cmdcache map[string]string, partial bool, maps ...envmap) (*Environment, error) {
	result := NewEnvironment()
	for _, m := range maps {
		for _, key := range m.env.Keys() {
			var failed error
			value := varcmd.ReplaceAllStringFunc(m.env.Get(key), func(ref string) string {
				if failed != nil {
					return ""
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
			if add {
				final[name] = true
				continue
			}
			if remove {
				delete(final, name)
				continue
			}

			if j == 0 && len(final) > 0 {
				for name := range final {
					delete(final, name)
				}
			}

			final[name] = true
		}
	}

	strs := make([]string, 0, len(final))
	for name := range final {
		strs = append(strs, name)
	}
	if len(strs) == 0 {
		return nil, fmt.Errorf("no %s specified for %s", what, strmaps[len(strmaps)-1].context)
	}
	return strs, nil
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
