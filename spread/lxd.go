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
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/context"
)

type Distro int

const (
	Unknown Distro = iota
	Ubuntu
	Debian
	Fedora
	Alpine
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

func (s *lxdServer) Label() string {
	return s.d.Name
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

func (s *lxdServer) Distro() Distro {
	// TODO: This doesn't handle explicit LXD image specifiers
	// (like images:fedora/26)
	//
	// Fixing that in general might require the user to specify the distro
	// in the spread.yaml metadata
	parts := strings.Split(s.System().Image, "-")
	if parts[0] == "ubuntu" {
		return Ubuntu
	}
	if parts[0] == "debian" {
		return Debian
	}
	if parts[0] == "fedora" {
		return Fedora
	}
	if parts[0] == "alpine" {
		return Alpine
	}
	return Unknown
}

func (s *lxdServer) Discard(ctx context.Context) error {
	output, err := exec.Command("lxc", "delete", "--force", s.d.Name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("cannot discard lxd container: %v", outputErr(output, err))
	}
	return nil
}

func (s *lxdServer) Prepare(ctx context.Context) error {
	if s.Distro() == Unknown {
		s.Discard(ctx)
		return fmt.Errorf("Unknown distro type for image %s, bailing", s.System().Image)
	}

	args := []string{"exec", s.d.Name, "--"}
	args = append(args, sshInstallCommand(s.Distro())...)


	output, err := exec.Command("lxc", args...).CombinedOutput()
	if err != nil {
		printf("Command output: %s", output)
		s.Discard(ctx)
		return err
	}

	err = s.p.tuneSSH(s.d.Name, s.Distro())
	if err != nil {
		s.Discard(ctx)
		return err
	}

	if s.Distro() == Fedora {
		// Fedora LXD images do *not* contain tar by default, so we must install it manually
		args = []string{"exec", s.d.Name, "--", "dnf", "install", "--assumeyes", "tar"}
		output, err = exec.Command("lxc", args...).CombinedOutput()

		if err != nil {
			printf("Command output: %s", output)
			s.Discard(ctx)
			return err
		}
	}
    if s.Distro() == Alpine {
        // Alpine images do not contain bash, which rather inconveniences spread
        args = []string{"exec", s.d.Name, "--", "apk", "add", "bash"}
        output, err = exec.Command("lxc", args...).CombinedOutput()

		if err != nil {
			printf("Command output: %s", output)
			s.Discard(ctx)
			return err
		}
    }
	return nil
}

func (p *lxdProvider) Backend() *Backend {
	return p.backend
}

func (p *lxdProvider) GarbageCollect() error {
	return nil
}

func (p *lxdProvider) Reuse(ctx context.Context, rsystem *ReuseSystem, system *System) (Server, error) {
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

func (p *lxdProvider) Allocate(ctx context.Context, system *System) (Server, error) {
	lxdimage, err := p.lxdImage(system)
	if err != nil {
		return nil, err
	}
	name, err := lxdName(system)
	if err != nil {
		return nil, err
	}

	if p.backend.Location != "" {
		name = p.backend.Location + ":" + name
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
	timeout := time.After(30 * time.Second)
	retry := time.NewTicker(1 * time.Second)
	defer retry.Stop()
	for {
		addr, err := p.address(name)
		if err == nil {
			s.address = addr
			break
		}
		if _, ok := err.(*lxdNoAddrError); !ok {
			s.Discard(ctx)
			return nil, err
		}

		select {
		case <-retry.C:
		case <-timeout:
			s.Discard(ctx)
			return nil, err
		}
	}

	err = s.Prepare(ctx)
	if err != nil {
		return nil, err
	}

	printf("Allocated %s.", s)
	return s, nil
}

func isDebArch(s string) bool {
	switch s {
	case "amd64", "i386", "armel", "armhf", "arm64", "powerpc", "ppc64el", "s390x":
		return true
	}
	return false
}

func debArch() string {
	switch runtime.GOARCH {
	case "386":
		return "i386"
	case "amd64":
		return "amd64"
	case "arm":
		return "armhf"
	case "arm64":
		return "arm64"
	case "ppc64le":
		return "ppc64el"
	case "s390x":
		return "s390x"
	case "ppc":
		return "powerpc"
	case "ppc64":
		return "ppc64"
	}
	return "amd64"
}

func sshInstallCommand(distro Distro) []string {
	if distro == Ubuntu || distro == Debian {
		return []string{"apt", "install", "openssh-server"}
	}
	if distro == Fedora {
		return []string{"dnf", "--assumeyes", "install", "openssh-server"}
	}
	if distro == Alpine {
		return []string{"apk", "add", "openssh-server"}
	}
	// Precondition failure - unknown distro!
	return []string{}
}

func (p *lxdProvider) lxdImage(system *System) (string, error) {
	// LXD loves the network. Force it to use a local image if available.
	fingerprint, err := p.lxdLocalImage(system)
	if err == nil {
		return fingerprint, nil
	}
	if err != errNoImage {
		return "", err
	}

	logf("Cannot find cached LXD image for %s.", system)

	// If a remote was explicitly provided, use LXD image name as-is.
	if strings.Contains(system.Image, ":") {
		return system.Image, nil
	}

	// Translate spread-like name to LXD-like URL.
	parts := strings.Split(system.Image, "-")
	if !isDebArch(parts[len(parts)-1]) {
		parts = append(parts, debArch())
	}
	if parts[0] == "ubuntu" {
		return "ubuntu:" + strings.Join(parts[1:], "/"), nil
	}
	return "images:" + strings.Join(parts, "/"), nil
}

type lxdImageInfo struct {
	Properties struct {
		OS           string
		Label        string
		Release      string
		Version      string
		Aliases      string
		Architecture string
		Remote       string
	} `yaml:"Properties"`
	Source struct {
		Server string `yaml:"Server"`
	} `yaml:"Source"`
}

var errNoImage = fmt.Errorf("image not found")

var lxdRemoteServer = map[string]string{
	"ubuntu": "https://cloud-images.ubuntu.com/releases",
	"images": "https://images.linuxcontainers.org",
}

func (p *lxdProvider) lxdRemoteNames() (map[string]string, error) {
	var stderr bytes.Buffer
	cmd := exec.Command("lxc", "remote", "list")
	cmd.Stderr = &stderr

	output, err := cmd.Output()
	if err != nil {
		err = outputErr(stderr.Bytes(), err)
		return nil, fmt.Errorf("cannot list lxd remotes: %v", err)
	}

	var names = make(map[string]string)
	var lines = strings.Split(string(output), "\n")
	for i := 3; i < len(lines); i += 2 {
		fields := strings.Split(lines[i], "|")
		if len(fields) < 3 {
			break
		}
		names[strings.TrimSpace(fields[2])] = strings.TrimSpace(fields[1])
	}
	return names, nil
}

func (p *lxdProvider) lxdLocalImage(system *System) (string, error) {
	parts := strings.Split(system.Image, "/")
	remote := ""
	if pair := strings.Split(parts[0], ":"); len(pair) > 1 {
		parts[0] = pair[1]
		remote = pair[0]
	} else if len(parts) == 1 && !strings.Contains(parts[0], "/") {
		// Translate spread-like name to LXD-like URL.
		parts = strings.Split(parts[0], "-")
	}
	if remote == "" {
		if parts[0] == "ubuntu" {
			parts = parts[1:]
			remote = "ubuntu"
		} else {
			remote = "images"
		}
	}

	if !isDebArch(parts[len(parts)-1]) {
		parts = append(parts, debArch())
	}

	remoteNames, err := p.lxdRemoteNames()
	if err != nil {
		return "", err
	}

	var stderr bytes.Buffer
	cmd := exec.Command("lxc", "image", "list")
	cmd.Stderr = &stderr

	output, err := cmd.Output()
	if err != nil {
		err = outputErr(stderr.Bytes(), err)
		return "", fmt.Errorf("cannot list lxd images: %v", err)
	}

	var fingerprints []string
	var lines = strings.Split(string(output), "\n")
	for i := 3; i < len(lines); i += 2 {
		fields := strings.Split(lines[i], "|")
		if len(fields) < 3 {
			break
		}
		fingerprints = append(fingerprints, strings.TrimSpace(fields[2]))
	}

NextImage:
	for _, fingerprint := range fingerprints {
		stderr.Truncate(0)
		cmd := exec.Command("lxc", "image", "info", fingerprint)
		cmd.Stderr = &stderr

		output, err := cmd.Output()
		if err != nil {
			err = outputErr(stderr.Bytes(), err)
			return "", fmt.Errorf("cannot obtain info about lxd image %s: %v", fingerprint, err)
		}

		var info lxdImageInfo
		err = yaml.Unmarshal(output, &info)
		if err != nil {
			return "", fmt.Errorf("cannot obtain info about lxd image %s: %v", fingerprint, err)
		}

		props := info.Properties
		aliases := strings.Split(props.Aliases, ",")

		if info.Source.Server != "" && remoteNames[info.Source.Server] != remote {
			continue
		}
		// This is a hack. Unfortunatley exported+imported images lose their remote.
		if info.Source.Server == "" && props.Remote != remote {
			continue
		}

		for _, part := range parts {
			switch part {
			case props.OS, props.Label, props.Release, props.Version, props.Architecture:
				continue
			}
			if contains(aliases, part) {
				continue
			}
			continue NextImage
		}

		logf("Using cached LXD image for %s: %s", system, fingerprint)
		return fingerprint, nil
	}

	return "", errNoImage
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

func sshReloadCommand(distro Distro) []string {
	if distro == Fedora {
		return []string{"systemctl", "restart", "sshd"}
	}
	if distro == Ubuntu || distro == Debian {
		return []string{"systemctl", "restart", "ssh"}
	}
	if distro == Alpine {
		return []string{"service", "sshd", "restart"}
	}
	// Precondition failure: unknown distro!
	return []string{}
}

func (p *lxdProvider) tuneSSH(name string, distro Distro) error {
	cmds := [][]string{
		{"sed", "-i", `s/^\s*#\?\s*\(PermitRootLogin\|PasswordAuthentication\)\>.*/\1 yes/`, "/etc/ssh/sshd_config"},
		{"/bin/sh", "-c", fmt.Sprintf("echo root:'%s' | chpasswd", p.options.Password)},
		sshReloadCommand(distro),
	}
	for _, args := range cmds {
		output, err := exec.Command("lxc", append([]string{"exec", name, "--"}, args...)...).CombinedOutput()
		if err != nil && args[0] != "killall" {
			return fmt.Errorf("cannot prepare sshd in lxd container %q: %v", name, outputErr(output, err))
		}
	}
	return nil
}

func contains(strs []string, s string) bool {
	for _, si := range strs {
		if si == s {
			return true
		}
	}
	return false
}
