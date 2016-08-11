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
	"strconv"
	"time"
)

type Project struct {
	Name string `yaml:"project"`

	Backends map[string]*Backend

	Environment map[string]string

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

	Systems        []string
	SystemWorkers  map[string]int      `yaml:"-"`
	SystemVariants map[string][]string `yaml:"-"`

	Prepare     string
	Restore     string
	PrepareEach string `yaml:"prepare-each"`
	RestoreEach string `yaml:"restore-each"`

	Environment map[string]string
	Variants    []string

	WarnTimeout Timeout `yaml:"warn-timeout"`
	KillTimeout Timeout `yaml:"kill-timeout"`
}

func (b *Backend) String() string { return fmt.Sprintf("backend %q", b.Name) }

type Suite struct {
	Summary  string
	Systems  []string
	Backends []string

	Variants    []string
	Environment map[string]string

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
	Environment map[string]string

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
	System  string
	Suite   *Suite
	Task    *Task

	Variant     string
	Environment map[string]string
}

func (job *Job) String() string {
	return job.Name
}

func (job *Job) StringFor(context interface{}) string {
	switch context {
	case job.Project:
		return fmt.Sprintf("project on %s:%s", job.Backend.Name, job.System)
	case job.Backend, job.System:
		return fmt.Sprintf("%s:%s", job.Backend.Name, job.System)
	case job.Suite:
		return fmt.Sprintf("%s:%s:%s", job.Backend.Name, job.System, job.Suite.Name)
	case job.Task:
		return fmt.Sprintf("%s:%s:%s", job.Backend.Name, job.System, job.Task.Name)
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
		buf.WriteString(script)
	}
	return buf.String()
}

func SplitVariants(s string) (prefix string, variants []string) {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[:i], strings.Split(s[i+1:], ",")
	}
	return s, nil
}

func SplitCount(s string) (prefix string, count int, ok bool) {
	if i := strings.LastIndex(s, "*"); i >= 0 {
		prefix = s[:i]
		count, err := strconv.Atoi(s[i+1:])
		if err != nil || count < 1 {
			return "", 0, false
		}
		return prefix, count, true
	}
	return s, 1, true
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

	for bname, backend := range project.Backends {
		if !validName.MatchString(bname) {
			return nil, fmt.Errorf("invalid backend name: %q", bname)
		}
		backend.Name = bname
		if backend.Type == "" {
			backend.Type = bname
		}
		switch backend.Type {
		case "linode", "lxd", "ssh":
		default:
			return nil, fmt.Errorf("%s has unsupported type %q", backend, backend.Type)
		}

		backend.Prepare = strings.TrimSpace(backend.Prepare)
		backend.Restore = strings.TrimSpace(backend.Restore)
		backend.PrepareEach = strings.TrimSpace(backend.PrepareEach)
		backend.RestoreEach = strings.TrimSpace(backend.RestoreEach)

		backend.SystemWorkers = make(map[string]int)
		backend.SystemVariants = make(map[string][]string)

		seen := make(map[string]bool)
		for i, system := range backend.Systems {
			system, variants := SplitVariants(system)
			system, workers, ok := SplitCount(system)
			if !ok {
				return nil, fmt.Errorf("%s lists system with invalid count suffix: %q", backend, system)
			}
			if seen[system] {
				return nil, fmt.Errorf("%s lists %s system more than once", backend, system)
			}
			seen[system] = true
			backend.Systems[i] = system
			backend.SystemWorkers[system] = workers
			backend.SystemVariants[system] = variants

			for _, variant := range variants {
				if !contains(backend.Variants, variant) {
					backend.Variants = append(backend.Variants, variant)
				}
			}
		}
		sort.Strings(backend.Variants)

		err = checkSystems(backend, backend.Systems)
		if err != nil {
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

		err = checkSystems(suite, suite.Systems)
		if err != nil {
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

			err = checkSystems(task, task.Systems)
			if err != nil {
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

func contains(set []string, item string) bool {
	for _, v := range set {
		if v == item {
			return true
		}
	}
	return false
}

func (p *Project) backendNames() []string {
	bnames := make([]string, 0, len(p.Backends))
	for bname, _ := range p.Backends {
		bnames = append(bnames, bname)
	}
	return bnames
}

func (p *Project) Jobs(options *Options) ([]*Job, error) {
	var jobs []*Job

	sprenv := envmap{stringer("$SPREAD_*"), map[string]string{
		"SPREAD_JOB":     "",
		"SPREAD_PROJECT": "",
		"SPREAD_PATH":    "",
		"SPREAD_BACKEND": "",
		"SPREAD_SYSTEM":  "",
		"SPREAD_SUITE":   "",
		"SPREAD_TASK":    "",
		"SPREAD_VARIANT": "",
	}}

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
				benv := envmap{task, backend.Environment}
				bevr := strmap{task, evars(backend.Environment, "+")}
				bvar := strmap{task, backend.Variants}
				bsys := strmap{task, backend.Systems}

				systems, err := evalstr("systems", bsys, ssys, tsys)
				if err != nil {
					return nil, err
				}

				for _, system := range systems {
					if backend.SystemWorkers[system] == 0 {
						continue
					}

					strmaps := []strmap{pevr, bevr, bvar, sevr, svar, tevr, tvar}
					variants, err := evalstr("variants", strmaps...)
					if err != nil {
						return nil, err
					}

					for _, variant := range variants {
						if variant == "" && len(variants) > 1 {
							continue
						}
						if vs := backend.SystemVariants[system]; len(vs) > 0 && !contains(vs, variant) {
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
							job.Name = fmt.Sprintf("%s:%s:%s", job.Backend.Name, job.System, job.Task.Name)
						} else {
							job.Name = fmt.Sprintf("%s:%s:%s:%s", job.Backend.Name, job.System, job.Task.Name, job.Variant)
						}

						sprenv.env["SPREAD_JOB"] = job.Name
						sprenv.env["SPREAD_PROJECT"] = job.Project.Name
						sprenv.env["SPREAD_PATH"] = job.Project.RemotePath
						sprenv.env["SPREAD_BACKEND"] = job.Backend.Name
						sprenv.env["SPREAD_SYSTEM"] = job.System
						sprenv.env["SPREAD_SUITE"] = job.Suite.Name
						sprenv.env["SPREAD_TASK"] = job.Task.Name
						sprenv.env["SPREAD_VARIANT"] = job.Variant

						env, err := evalenv(cmdcache, false, penv, benv, senv, tenv, sprenv)
						if err != nil {
							return nil, err
						}
						for key, value := range env {
							ekey, evariants := SplitVariants(key)
							if len(evariants) > 0 {
								delete(env, key)
							}
							for _, evariant := range evariants {
								if evariant == variant {
									env[ekey] = value
								}
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

func evars(env map[string]string, prefix string) []string {
	seen := make(map[string]bool, len(env))
	for key := range env {
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
	env     map[string]string
}

type stringer string

func (s stringer) String() string { return string(s) }

var (
	varref  = regexp.MustCompile(`\$\(.+?\)|\$\[[a-zA-Z0-9_/]+\]`)
	varname = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*(?:/[a-zA-Z0-9_]+)?$`)
)

func evalone(context string, line string, cmdcache map[string]string, maps ...envmap) (string, error) {
	m := envmap{stringer(context), map[string]string{"": line}}
	out, err := evalenv(cmdcache, false, append(maps, m)...)
	if err != nil {
		return "", err
	}
	return out[""], nil
}

func evalenv(cmdcache map[string]string, partial bool, maps ...envmap) (map[string]string, error) {
	merged := make(map[string]string)
	for _, m := range maps {
		for key, value := range m.env {
			if key != "" && !varname.MatchString(key) {
				return nil, fmt.Errorf("invalid variable name in %s environment: %q", m.context)
			}
			merged[key] = value
		}
	}

	// Pre-validation to make errors saner.
	for _, m := range maps {
		for key, value := range m.env {
			for _, ref := range varref.FindAllString(value, -1) {
				inner := ref[2 : len(ref)-1]
				if strings.HasPrefix(ref, "$(") {
					if _, ok := cmdcache[inner]; ok {
						continue
					}
					err := evalcmd(cmdcache, inner)
					if err != nil {
						if key == "" {
							return nil, fmt.Errorf("%s in %s returned error: %v", ref, m.context, err)
						} else {
							return nil, fmt.Errorf("%s in %s environment returned error: %v", ref, m.context, err)
						}
					}
					continue
				}
				if _, ok := merged[inner]; !ok && !partial {
					if key == "" {
						return nil, fmt.Errorf("%s references undefined variable %s", m.context, ref)
					} else {
						return nil, fmt.Errorf("%s in %s environment references undefined variable %s", key, m.context, ref)
					}
				}
			}
		}
	}

	// Perform replacements on each variable only once, and
	// only after referenced variables are replaced.
	done := make(map[string]bool)
	for {
		before := len(done)
	NextVar:
		for key, value := range merged {
			for _, ref := range varref.FindAllString(value, -1) {
				if strings.HasPrefix(ref, "$(") {
					continue
				}
				if !done[ref[2:len(ref)-1]] {
					continue NextVar
				}
			}
			done[key] = true
			merged[key] = varref.ReplaceAllStringFunc(value, func(ref string) string {
				inner := ref[2 : len(ref)-1]
				if strings.HasPrefix(ref, "$(") {
					return cmdcache[inner]
				}
				return merged[inner]
			})
			if key == "" {
				// Stop early for single field case.
				return merged, nil
			}
		}
		if len(done) == before {
			break
		}
	}

	if len(done) != len(merged) {
		var missing []string
		for key := range merged {
			if !done[key] {
				if partial {
					delete(merged, key)
				} else if key != "" {
					missing = append(missing, "$"+key)
				}
			}
		}

		if partial {
			return merged, nil
		}

		sort.Strings(missing)

		// Look for the most specific context.
		var context fmt.Stringer
	ContextLoop:
		for i := len(maps) - 1; i >= 0; i-- {
			m := maps[i]
			for _, key := range missing {
				if _, ok := m.env[key[1:]]; ok {
					context = m.context
					break ContextLoop
				}
			}
		}

		return nil, fmt.Errorf("cannot define %s environment due to circular references: %s", context, strings.Join(missing, ", "))
	}

	return merged, nil
}

func evalcmd(cmdcache map[string]string, cmdline string) error {
	var stderr bytes.Buffer
	cmd := exec.Command("/bin/bash", "-c", cmdline)
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		msg := string(bytes.TrimSpace(stderr.Bytes()))
		if len(msg) > 0 {
			msgs := strings.Split(msg, "\n")
			return fmt.Errorf("%s", strings.Join(msgs, "; "))
		}
		return err
	}
	cmdcache[cmdline] = string(output)
	return nil
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
