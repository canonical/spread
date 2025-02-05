package spread

import (
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v2"
)

type Reuse struct {
	filename string
	file     *os.File
	mu       sync.Mutex
	backends map[string]*ReuseBackend `yaml:",omitempty"`
}

func OpenReuse(filename string) (r *Reuse, err error) {
	r = &Reuse{}

	file, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("cannot open reuse lock file: %v", err)
	}
	defer func() {
		if err != nil {
			file.Close()
		}
	}()

	locked := make(chan bool, 1)
	go func() {
		time.Sleep(3 * time.Second)
		select {
		case <-locked:
		default:
			printf("Waiting for another process to release reuse lock.")
		}
	}()
	const LOCK_EX = 2
	err = syscall.Flock(int(file.Fd()), LOCK_EX)
	if err != nil {
		return nil, fmt.Errorf("cannot obtain lock on %s: %v", filename, err)
	}
	locked <- true

	datafile := file

	// Check if the previous process crashed and left a .new file behind.
	if f, err := os.Open(filename + ".new"); err == nil {
		datafile = f
		defer f.Close()
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("cannot open reuse tracking file: %v", err)
	}

	data, err := io.ReadAll(datafile)
	if err != nil {
		return nil, fmt.Errorf("cannot read reuse tracking file: %v", err)
	}

	var content struct{ Backends map[string]*ReuseBackend }
	err = yaml.Unmarshal(data, &content)
	if err != nil {
		return nil, fmt.Errorf("cannot unmarshal reuse tracking data: %v", err)
	}
	r.backends = content.Backends
	if len(r.backends) == 0 {
		r.backends = make(map[string]*ReuseBackend)
	}

	r.file = file
	return r, nil
}

func (r *Reuse) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Close, unlocking the file, and ignore errors. The writes have
	// already synced and reported any relevant problems.
	r.file.Close()
}

func (r *Reuse) write() error {
	var content struct{ Backends map[string]*ReuseBackend }
	content.Backends = make(map[string]*ReuseBackend)
	for bname, rbackend := range r.backends {
		if len(rbackend.Systems) > 0 {
			content.Backends[bname] = rbackend
		}
	}

	var data []byte
	var err error
	if len(content.Backends) > 0 {
		data, err = yaml.Marshal(&content)
		if err != nil {
			return fmt.Errorf("internal error: cannot marshal reuse tracking data: %v", err)
		}
	}

	// First, atomically write the state into a .new file.
	f, err := os.Create(r.filename + ".tmp")
	if err != nil {
		return fmt.Errorf("cannot create reuse tracking file: %v", err)
	}
	err = firstErr(
		werr(f.Write(data)),
		f.Sync(),
		f.Close(),
	)
	if err != nil {
		os.Remove(r.filename + ".tmp")
		return fmt.Errorf("cannot write reuse tracking data: %v", err)
	}
	err = os.Rename(r.filename+".tmp", r.filename+".new")
	if err != nil {
		os.Remove(r.filename + ".tmp")
		return fmt.Errorf("cannot rename temporary reuse tracking file: %v", err)
	}

	// Now non-atomically update the real file, currently locked. If the process crashes,
	// the next process will pick up the .new and complete the process. This enables safe
	// atomic writes while still using a single file for the typical case.
	err = r.file.Truncate(0)
	if err == nil {
		_, err = r.file.WriteAt(data, 0)
	}
	if err == nil {
		err = r.file.Sync()
	}
	if err != nil {
		// Report an error, but at this point the data is already safe.
		// The next process will pick up the .new file and read it out.
		return fmt.Errorf("cannot write reuse tracking file: %v", err)
	}
	err = os.Remove(r.filename + ".new")
	if err != nil {
		return fmt.Errorf("cannot remove temporary reuse tracking file: %v", err)
	}
	return nil
}

func werr(n int, err error) error {
	return err
}

func (r *Reuse) Add(server Server, password string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.has(server) {
		return nil
	}

	system := server.System()
	rsystem := &ReuseSystem{
		Name:     system.Name,
		Username: system.Username,
		Password: system.Password,
		Address:  server.Address(),
		Data:     server.ReuseData(),
	}
	if rsystem.Password == "" {
		rsystem.Password = password
	}

	rbackend, ok := r.backends[system.Backend]
	if !ok {
		rbackend = &ReuseBackend{}
		r.backends[system.Backend] = rbackend
	}

	rbackend.Systems = append(rbackend.Systems, rsystem)

	return r.write()
}

func (r *Reuse) Remove(server Server) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	rbackend, ok := r.backends[server.System().Backend]
	if !ok {
		return nil
	}
	address := server.Address()
	for i := range rbackend.Systems {
		rsystem := rbackend.Systems[i]
		if rsystem.Address == address {
			copy(rbackend.Systems[i:], rbackend.Systems[i+1:])
			rbackend.Systems = rbackend.Systems[:len(rbackend.Systems)-1]
			return r.write()
		}
	}
	return nil
}

func (r *Reuse) has(server Server) bool {
	rbackend, ok := r.backends[server.System().Backend]
	if !ok {
		return false
	}
	address := server.Address()
	for i := range rbackend.Systems {
		rsystem := rbackend.Systems[i]
		if rsystem.Address == address {
			return true
		}
	}
	return false
}

func (r *Reuse) ReuseSystems(system *System) []*ReuseSystem {
	r.mu.Lock()
	defer r.mu.Unlock()

	rbackend, ok := r.backends[system.Backend]
	if !ok {
		return nil
	}

	var result []*ReuseSystem
	for i := range rbackend.Systems {
		rsystem := rbackend.Systems[i]
		if rsystem.Name == system.Name {
			result = append(result, rsystem)
		}
	}
	return result
}

type ReuseBackend struct {
	Systems []*ReuseSystem `yaml:",omitempty"`
}

type ReuseSystem struct {
	Name     string `yaml:"-"`
	Username string `yaml:",omitempty"`
	Password string
	Address  string
	Data     interface{} `yaml:",omitempty"`
}

func (rsys *ReuseSystem) UnmarshalYAML(u func(interface{}) error) error {
	type norecurse ReuseSystem
	var def map[string]norecurse
	if err := u(&def); err != nil {
		return err
	}
	for name, sys := range def {
		sys.Name = name
		*rsys = ReuseSystem(sys)
	}
	return nil
}

func (rsys ReuseSystem) MarshalYAML() (interface{}, error) {
	type norecurse ReuseSystem
	return map[string]norecurse{rsys.Name: norecurse(rsys)}, nil
}

func (rsys *ReuseSystem) UnmarshalData(v interface{}) error {
	data, err := yaml.Marshal(rsys.Data)
	if err != nil {
		return fmt.Errorf("cannot remarshal reuse tracking data for %s: %v", rsys.Name, err)
	}
	return yaml.Unmarshal(data, v)
}
