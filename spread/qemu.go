package spread

import (
	"fmt"
	"gopkg.in/yaml.v2"
	"math/rand"
	"os"
	"os/exec"
	"strconv"
)

func QEMU(p *Project, b *Backend, o *Options) Provider {
	return &qemuProvider{p, b, o}
}

type qemuProvider struct {
	project *Project
	backend *Backend
	options *Options
}

type qemuServer struct {
	p *qemuProvider
	d qemuServerData
}

type qemuServerData struct {
	Backend string
	System  string
	Address string
	PID     int
}

func (s *qemuServer) String() string {
	return fmt.Sprintf("%s:%s", s.p.backend.Name, s.d.System)
}

func (s *qemuServer) Provider() Provider {
	return s.p
}

func (s *qemuServer) Address() string {
	return s.d.Address
}

func (s *qemuServer) System() *System {
	system := s.p.backend.Systems[s.d.System]
	if system == nil {
		return removedSystem(s.p.backend, s.d.System)
	}
	return system
}

func (s *qemuServer) ReuseData() []byte {
	data, err := yaml.Marshal(&s.d)
	if err != nil {
		panic(err)
	}
	return data
}

func (s *qemuServer) Discard() error {
	p, err := os.FindProcess(s.d.PID)
	if err != nil {
		return nil // But never happens on Unix, per docs.
	}
	err = p.Kill()
	// Ought to have a better way to distinguish the error. :-/
	if err != nil && err.Error() == "os: process already finished" {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot discard qemu %s: %v", s, err)
	}
	return nil
}

func (p *qemuProvider) Backend() *Backend {
	return p.backend
}

func (p *qemuProvider) Reuse(data []byte) (Server, error) {
	s := &qemuServer{}
	err := yaml.Unmarshal(data, &s.d)
	if err != nil {
		return nil, fmt.Errorf("cannot unmarshal qemu reuse data: %v", err)
	}
	s.p = p
	return s, nil
}

func systemPath(system *System) string {
	return os.ExpandEnv("$HOME/.spread/qemu/" + system.Image + ".img")
}

func (p *qemuProvider) Allocate(system *System) (Server, error) {
	// FIXME Find an available port more reliably.
	port := 59301 + rand.Intn(99)

	path := systemPath(system)
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return nil, &FatalError{fmt.Errorf("cannot find qemu image at %s", path)}
	}

	fwd := fmt.Sprintf("user,hostfwd=tcp::%d-:22", port)
	cmd := exec.Command("kvm", "-snapshot", "-m", "1500", "-net", "nic", "-net", fwd, path)
	if os.Getenv("SPREAD_QEMU_GUI") != "1" {
		cmd.Args = append([]string{cmd.Args[0], "-nographic"}, cmd.Args[1:]...)
	}
	err := cmd.Start()
	if err != nil {
		return nil, &FatalError{fmt.Errorf("cannot launch qemu %s: %v", system, err)}
	}

	s := &qemuServer{
		p: p,
		d: qemuServerData{
			System:  system.Name,
			Address: "localhost:" + strconv.Itoa(port),
			Backend: p.backend.Name,
			PID:     cmd.Process.Pid,
		},
	}

	printf("Waiting for %s to make SSH available...", system)
	if err := waitPortUp(system, s.Address()); err != nil {
		s.Discard()
		return nil, fmt.Errorf("cannot connect to %s: %s", s, err)
	}
	printf("Allocated %s.", s)
	return s, nil
}
