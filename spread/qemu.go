package spread

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"golang.org/x/net/context"
)

var ovmfDefaultPath = "/usr/share/OVMF/OVMF_CODE.fd"

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

func (s *qemuServer) Label() string {
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

func (s *qemuServer) Discard(ctx context.Context) error {
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

func (p *qemuProvider) GarbageCollect() error {
	return nil
}

func (p *qemuProvider) Reuse(ctx context.Context, rsystem *ReuseSystem, system *System) (Server, error) {
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

func biosPath(biosName string) (string, error) {
	bios := os.ExpandEnv("$HOME/.spread/qemu/bios/" + biosName + ".img")
	if info, err := os.Stat(bios); err == nil && info.Mode().IsRegular() {
		debugf("using local bios file: %v", bios)
		return bios, nil
	}

	if p := os.Getenv("SPREAD_QEMU_FALLBACK_BIOS_PATH"); p != "" {
		fallbackBios := os.ExpandEnv(filepath.Join(p, biosName+".img"))
		debugf("using fallback bios file: %v", fallbackBios)
		return fallbackBios, nil
	}

	switch biosName {
	case "uefi":
		return ovmfDefaultPath, nil
	}

	return "", fmt.Errorf("cannot find bios path for %q", biosName)
}

func qemuCmd(system *System, path string, mem, port int) (*exec.Cmd, error) {
	serial := fmt.Sprintf("telnet:127.0.0.1:%d,server,nowait", port+100)
	monitor := fmt.Sprintf("telnet:127.0.0.1:%d,server,nowait", port+200)
	fwd := fmt.Sprintf("user,hostfwd=tcp:127.0.0.1:%d-:22", port)
	cmd := exec.Command("qemu-system-x86_64",
		"-enable-kvm",
		"-snapshot",
		"-m", strconv.Itoa(mem),
		"-net", "nic",
		"-net", fwd,
		"-serial", serial,
		"-monitor", monitor,
		path)
	if os.Getenv("SPREAD_QEMU_GUI") != "1" {
		cmd.Args = append([]string{cmd.Args[0], "-nographic"}, cmd.Args[1:]...)
	}

	switch system.Bios {
	case "":
		// nothing to do, that is the qemu default
	case "uefi":
		biosPath, err := biosPath(system.Bios)
		if err != nil {
			return nil, err
		}
		cmd.Args = append([]string{cmd.Args[0], "-bios", biosPath}, cmd.Args[1:]...)
	default:
		return nil, fmt.Errorf(`cannot set bios to %q, only "uefi" or unset are supported`, system.Bios)
	}
	return cmd, nil
}

func (p *qemuProvider) Allocate(ctx context.Context, system *System) (Server, error) {
	// FIXME Find an available port more reliably.
	port := 59301 + rand.Intn(99)
	mem := 1500
	if p.backend.Memory > 0 {
		mem = int(p.backend.Memory / mb)
	}
	path := systemPath(system)
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return nil, &FatalError{fmt.Errorf("cannot find qemu image at %s", path)}
	}

	cmd, err := qemuCmd(system, path, mem, port)
	if err != nil {
		return nil, err
	}
	printf("Serial and monitor for %s available at ports %d and %d.", system, port+100, port+200)

	err = cmd.Start()
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
	if err := waitPortUp(ctx, system, s.address); err != nil {
		s.Discard(ctx)
		return nil, err
	}
	printf("Allocated %s.", s)
	return s, nil
}
