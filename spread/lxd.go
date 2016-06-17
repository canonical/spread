package spread

import (
	"bytes"
	"encoding/json"
	"fmt"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func LXD(b *Backend) Provider {
	return &lxd{b}
}

type lxd struct {
	backend *Backend
}

type lxdServer struct {
	l *lxd
	d lxdServerData
}

type lxdServerData struct {
	Name    string
	Backend string
	System  string
	Address string
}

func (s *lxdServer) String() string {
	return fmt.Sprintf("%s:%s (%s)", s.l.backend.Name, s.d.System, s.d.Name)
}

func (s *lxdServer) Provider() Provider {
	return s.l
}

func (s *lxdServer) Address() string {
	return s.d.Address
}

func (s *lxdServer) System() string {
	return s.d.System
}

func (s *lxdServer) ReuseData() []byte {
	data, err := yaml.Marshal(&s.d)
	if err != nil {
		panic(err)
	}
	return data
}

func (s *lxdServer) Discard() error {
	output, err := exec.Command("lxc", "delete", "--force", s.d.Name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("cannot discard lxd container: %v", outputErr(output, err))
	}
	return nil
}

func (l *lxd) Backend() *Backend {
	return l.backend
}

func (l *lxd) Reuse(data []byte, password string) (Server, error) {
	server := &lxdServer{}
	err := yaml.Unmarshal(data, &server.d)
	if err != nil {
		return nil, fmt.Errorf("cannot unmarshal lxd reuse data: %v", err)
	}
	server.l = l
	return server, nil
}

func (l *lxd) Allocate(system string, password string, keep bool) (Server, error) {
	lxdimage := lxdImage(system)
	name, err := lxdName(system)
	if err != nil {
		return nil, err
	}

	args := []string{"launch", lxdimage, name}
	if !keep {
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

	server := &lxdServer{
		l: l,
		d: lxdServerData{
			Name:    name,
			System:  system,
			Backend: l.backend.Name,
		},
	}

	printf("Waiting for LXD container %s to have an address...", name)
	timeout := time.After(10 * time.Second)
	retry := time.NewTicker(1 * time.Second)
	defer retry.Stop()
	for {
		addr, err := l.address(name)
		if err == nil {
			server.d.Address = addr
			break
		}
		if _, ok := err.(*lxdNoAddrError); !ok {
			server.Discard()
			return nil, err
		}

		select {
		case <-retry.C:
		case <-timeout:
			server.Discard()
			return nil, err
		}
	}

	err = l.tuneSSH(name, password)
	if err != nil {
		server.Discard()
		return nil, err
	}

	printf("Allocated %s.", server)
	return server, nil
}

func lxdImage(system string) string {
	parts := strings.Split(system, "-")
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

func lxdName(system string) (string, error) {
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

	return fmt.Sprintf("spread-%d-%s", n, strings.Replace(system, ".", "-", -1)), nil
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

func (l *lxd) address(name string) (string, error) {
	server, err := l.server(name)
	if err != nil {
		return "", err
	}
	for _, addr := range server.State.Network["eth0"].Addresses {
		if addr.Family == "inet" && addr.Address != "" {
			return addr.Address, nil
		}
	}
	return "", &lxdNoAddrError{name}
}

func (l *lxd) server(name string) (*lxdServerJSON, error) {
	var stderr bytes.Buffer
	cmd := exec.Command("lxc", "list", "--format=json", name)
	cmd.Stderr = &stderr

	output, err := cmd.Output()
	if err != nil {
		err = outputErr(stderr.Bytes(), err)
		return nil, fmt.Errorf("cannot list lxd container: %v", err)
	}

	var servers []*lxdServerJSON
	err = json.Unmarshal(output, &servers)
	if err != nil {
		return nil, fmt.Errorf("cannot unmarshal lxd list output: %v", err)
	}

	debugf("lxd list output: %# v\n", servers)

	if len(servers) == 0 {
		return nil, &lxdNoServerError{name}
	}
	if servers[0].Name != name {
		return nil, fmt.Errorf("lxd returned invalid JSON listing for %q: %s", name, outputErr(output, nil))
	}
	return servers[0], nil
}

func (l *lxd) tuneSSH(name, password string) error {
	cmds := [][]string{
		{"sed", "-i", `s/\(PermitRootLogin\|PasswordAuthentication\)\>.*/\1 yes/`, "/etc/ssh/sshd_config"},
		{"/bin/sh", "-c", fmt.Sprintf("echo root:'%s' | chpasswd", password)},
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
