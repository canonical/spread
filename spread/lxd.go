package spread

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func LXD(p *Project, b *Backend, o *Options) Provider {
	return &lxdProvider{p, b, o}
}

type lxdProvider struct {
	project *Project
	backend *Backend
	options *Options
}

type lxdServer struct {
	p *lxdProvider
	d lxdServerData

	system  *System
	address string
}

type lxdServerData struct {
	Name string
}

func (s *lxdServer) String() string {
	return fmt.Sprintf("%s (%s)", s.system, s.d.Name)
}

func (s *lxdServer) Provider() Provider {
	return s.p
}

func (s *lxdServer) Address() string {
	return s.address
}

func (s *lxdServer) System() *System {
	return s.system
}

func (s *lxdServer) ReuseData() interface{} {
	return &s.d
}

func (s *lxdServer) Discard() error {
	output, err := exec.Command("lxc", "delete", "--force", s.d.Name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("cannot discard lxd container: %v", outputErr(output, err))
	}
	return nil
}

func (p *lxdProvider) Backend() *Backend {
	return p.backend
}

func (p *lxdProvider) Reuse(rsystem *ReuseSystem, system *System) (Server, error) {
	s := &lxdServer{
		p:       p,
		system:  system,
		address: rsystem.Address,
	}
	err := rsystem.UnmarshalData(&s.d)
	if err != nil {
		return nil, fmt.Errorf("cannot unmarshal lxd reuse data: %v", err)
	}
	return s, nil
}

func (p *lxdProvider) Allocate(system *System) (Server, error) {
	lxdimage := p.lxdImage(system)
	name, err := lxdName(system)
	if err != nil {
		return nil, err
	}

	args := []string{"launch", lxdimage, name}
	if !p.options.Reuse {
		args = append(args, "--ephemeral")
	}
	output, err := exec.Command("lxc", args...).CombinedOutput()
	if err != nil {
		err = outputErr(output, err)
		if bytes.Contains(output, []byte("error: not found")) {
			err = fmt.Errorf("%s not found", lxdimage)
		}
		return nil, &FatalError{fmt.Errorf("cannot launch lxd container: %v", err)}
	}

	s := &lxdServer{
		p: p,
		d: lxdServerData{
			Name: name,
		},
		system: system,
	}

	printf("Waiting for lxd container %s to have an address...", name)
	timeout := time.After(10 * time.Second)
	retry := time.NewTicker(1 * time.Second)
	defer retry.Stop()
	for {
		addr, err := p.address(name)
		if err == nil {
			s.address = addr
			break
		}
		if _, ok := err.(*lxdNoAddrError); !ok {
			s.Discard()
			return nil, err
		}

		select {
		case <-retry.C:
		case <-timeout:
			s.Discard()
			return nil, err
		}
	}

	err = p.tuneSSH(name)
	if err != nil {
		s.Discard()
		return nil, err
	}

	printf("Allocated %s.", s)
	return s, nil
}

func (p *lxdProvider) lxdImage(system *System) string {
	if strings.Contains(system.Image, ":") {
		return system.Image
	}
	parts := strings.Split(system.Image, "-")
	if parts[0] == "ubuntu" {
		return "ubuntu:" + strings.Join(parts[1:], "/")
	}
	switch parts[len(parts)-1] {
	case "amd64", "i386", "armel", "armhf", "arm64", "powerpc", "ppc64el", "s390x":
	default:
		parts = append(parts, "amd64")
	}
	return "images:" + strings.Join(parts, "/")
}

func lxdName(system *System) (string, error) {
	filename := os.ExpandEnv("$HOME/.spread/lxd-count")
	file, err := os.OpenFile(filename, os.O_RDWR, 0644)
	if os.IsNotExist(err) {
		err = os.Mkdir(filepath.Dir(filename), 0755)
		if err != nil && !os.IsExist(err) {
			return "", fmt.Errorf("cannot create ~/.spread/: %v", err)
		}
		file, err = os.OpenFile(filename, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0644)
		if err != nil {
			return "", fmt.Errorf("cannot create ~/.spread/lxd-count: %v", err)
		}
	} else if err != nil {
		return "", fmt.Errorf("cannot open ~/.spread/lxd-count: %v", err)
	}
	defer file.Close()

	const LOCK_EX = 2
	err = syscall.Flock(int(file.Fd()), LOCK_EX)
	if err != nil {
		return "", fmt.Errorf("cannot obtain lock on ~/.spread/lxd-count: %v", err)
	}

	data, err := ioutil.ReadAll(file)
	if err != nil {
		return "", fmt.Errorf("cannot read ~/.spread/lxd-count: %v", err)
	}

	n := 0
	nstr := strings.TrimSpace(string(data))
	if nstr != "" {
		n, err = strconv.Atoi(nstr)
		if err != nil {
			return "", fmt.Errorf("expected integer in ~/.spread/lxd-count, got %q", nstr)
		}
	}

	n++
	_, err = file.WriteAt([]byte(strconv.Itoa(n)), 0)
	if err != nil {
		return "", fmt.Errorf("cannot write to ~/.spread/lxd-count: %v", err)
	}

	return fmt.Sprintf("spread-%d-%s", n, strings.Replace(system.Name, ".", "-", -1)), nil
}

type lxdServerJSON struct {
	Name  string `json:"name"`
	State struct {
		Network map[string]lxdDeviceJSON `json:"network"`
	} `json:"state"`
}

type lxdDeviceJSON struct {
	State     string           `json:"state"`
	Addresses []lxdAddressJSON `json:"addresses"`
}

type lxdAddressJSON struct {
	Family  string `json:"family"`
	Address string `json:"address"`
}

type lxdNoServerError struct {
	name string
}

func (e *lxdNoServerError) Error() string {
	return fmt.Sprintf("cannot find lxd server %q", e.name)
}

type lxdNoAddrError struct {
	name string
}

func (e *lxdNoAddrError) Error() string {
	return fmt.Sprintf("lxd server %s has no address available", e.name)
}

func (p *lxdProvider) address(name string) (string, error) {
	sjson, err := p.serverJSON(name)
	if err != nil {
		return "", err
	}
	for _, addr := range sjson.State.Network["eth0"].Addresses {
		if addr.Family == "inet" && addr.Address != "" {
			return addr.Address, nil
		}
	}
	return "", &lxdNoAddrError{name}
}

func (p *lxdProvider) serverJSON(name string) (*lxdServerJSON, error) {
	var stderr bytes.Buffer
	cmd := exec.Command("lxc", "list", "--format=json", name)
	cmd.Stderr = &stderr

	output, err := cmd.Output()
	if err != nil {
		err = outputErr(stderr.Bytes(), err)
		return nil, fmt.Errorf("cannot list lxd container: %v", err)
	}

	var sjsons []*lxdServerJSON
	err = json.Unmarshal(output, &sjsons)
	if err != nil {
		return nil, fmt.Errorf("cannot unmarshal lxd list output: %v", err)
	}

	debugf("lxd list output: %# v\n", sjsons)

	if len(sjsons) == 0 {
		return nil, &lxdNoServerError{name}
	}
	if sjsons[0].Name != name {
		return nil, fmt.Errorf("lxd returned invalid JSON listing for %q: %s", name, outputErr(output, nil))
	}
	return sjsons[0], nil
}

func (p *lxdProvider) tuneSSH(name string) error {
	cmds := [][]string{
		{"sed", "-i", `s/\(PermitRootLogin\|PasswordAuthentication\)\>.*/\1 yes/`, "/etc/ssh/sshd_config"},
		{"/bin/bash", "-c", fmt.Sprintf("echo root:'%s' | chpasswd", p.options.Password)},
		{"killall", "-HUP", "sshd"},
	}
	for _, args := range cmds {
		output, err := exec.Command("lxc", append([]string{"exec", name, "--"}, args...)...).CombinedOutput()
		if err != nil && args[0] != "killall" {
			return fmt.Errorf("cannot prepare sshd in lxd container %q: %v", name, outputErr(output, err))
		}
	}
	return nil
}
