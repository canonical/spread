package spread

import (
	"fmt"
	"gopkg.in/yaml.v2"
	"math/rand"
	"os"
	"os/exec"
	"strconv"
)

func QEMU(b *Backend) Provider {
	return &qemu{b}
}

type qemu struct {
	backend *Backend
}

type qemuServer struct {
	q *qemu
	d qemuServerData
}

type qemuServerData struct {
	Backend string
	System  string
	Address string
	PID     int
}

func (s *qemuServer) String() string {
	return fmt.Sprintf("%s:%s", s.q.backend.Name, s.d.System)
}

func (s *qemuServer) Provider() Provider {
	return s.q
}

func (s *qemuServer) Address() string {
	return s.d.Address
}

func (s *qemuServer) System() *System {
	system := s.q.backend.Systems[s.d.System]
	if system == nil {
		return removedSystem(s.q.backend, s.d.System)
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

func (q *qemu) Backend() *Backend {
	return q.backend
}

func (q *qemu) Reuse(data []byte, password string) (Server, error) {
	server := &qemuServer{}
	err := yaml.Unmarshal(data, &server.d)
	if err != nil {
		return nil, fmt.Errorf("cannot unmarshal qemu reuse data: %v", err)
	}
	server.q = q
	return server, nil
}

func systemPath(system *System) string {
	return os.ExpandEnv("$HOME/.spread/qemu/" + system.Image + ".img")
}

func (q *qemu) Allocate(system *System, password string, keep bool) (Server, error) {
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

	server := &qemuServer{
		q: q,
		d: qemuServerData{
			System:  system.Name,
			Address: "localhost:" + strconv.Itoa(port),
			Backend: q.backend.Name,
			PID:     cmd.Process.Pid,
		},
	}

	printf("Waiting for %s to have an address...", system)
	if err := waitPortUp(system, server.Address()); err != nil {
		server.Discard()
		return nil, fmt.Errorf("cannot connect to %s: %s", server, err)
	}
	printf("Allocated %s.", server)
	return server, nil
}
