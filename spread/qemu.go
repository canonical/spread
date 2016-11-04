package spread

import (
	"fmt"
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

	system  *System
	address string
}

type qemuServerData struct {
	PID int
}

func (s *qemuServer) String() string {
	return s.system.String()
}

func (s *qemuServer) Provider() Provider {
	return s.p
}

func (s *qemuServer) Address() string {
	return s.address
}

func (s *qemuServer) System() *System {
	return s.system
}

func (s *qemuServer) ReuseData() interface{} {
	return &s.d
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

func (p *qemuProvider) Reuse(rsystem *ReuseSystem, system *System) (Server, error) {
	s := &qemuServer{
		p:       p,
		system:  system,
		address: rsystem.Address,
	}
	err := rsystem.UnmarshalData(&s.d)
	if err != nil {
		return nil, fmt.Errorf("cannot unmarshal qemu reuse data: %v", err)
	}
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

	serial := fmt.Sprintf("telnet::%d,server,nowait", port+100)
	monitor := fmt.Sprintf("telnet::%d,server,nowait", port+200)
	fwd := fmt.Sprintf("user,hostfwd=tcp::%d-:22", port)
	cmd := exec.Command("kvm", "-snapshot", "-m", "1500", "-net", "nic", "-net", fwd, "-serial", serial, "-monitor", monitor, path)
	if os.Getenv("SPREAD_QEMU_GUI") != "1" {
		cmd.Args = append([]string{cmd.Args[0], "-nographic"}, cmd.Args[1:]...)
	}
	printf("Serial port for %q available via 'telnet localhost %d'", system, port+100)
	printf("Montior port for %q available via 'telnet localhost %d'", system, port+200)

	err := cmd.Start()
	if err != nil {
		return nil, &FatalError{fmt.Errorf("cannot launch qemu %s: %v", system, err)}
	}

	s := &qemuServer{
		p: p,
		d: qemuServerData{
			PID: cmd.Process.Pid,
		},
		system:  system,
		address: "localhost:" + strconv.Itoa(port),
	}

	printf("Waiting for %s to make SSH available...", system)
	if err := waitPortUp(system, s.address); err != nil {
		s.Discard()
		return nil, fmt.Errorf("cannot connect to %s: %s", s, err)
	}
	printf("Allocated %s.", s)
	return s, nil
}
