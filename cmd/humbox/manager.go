package main

import (
	"bytes"
	"encoding/base32"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"golang.org/x/crypto/blake2b"

	"gopkg.in/yaml.v3"
)

type Setup struct {
	Hostname string
	Images   []*Image
	Accounts []*Account
}

type Manager struct {
	mu    sync.Mutex
	dir   string
	setup Setup

	servers map[string]*Server
	sshname map[string]string
}

type Server struct {
	Name    string    `json:"name"`
	Image   string    `json:"image"`
	Account string    `json:"account"`
	SSH     string    `json:"ssh"`
	Birth   time.Time `json:"birth"`

	Custom map[string]interface{} `json:"custom,omitempty"`

	mu sync.Mutex
}

type Image struct {
	Name string `json:"name"`
	Arch string `json:"arch"`
	UEFI bool   `json:"uefi"`
	KVM  bool   `json:"kvm"`
}

type Account struct {
	Name  string `json:"name"`
	Hash  string `json:"hash"`
	Admin bool   `json:"admin"`
}

func (acc *Account) Authenticate(token string) error {
	if len(acc.Hash) != 67 || acc.Hash[:3] != "v1:" {
		log.Printf("Account %q has unrecognized authentication hash format: %q", acc.Name, acc.Hash)
		return fmt.Errorf("account %q has unrecognized authentication format", acc.Name)
	}

	hash1, err := base32.HexEncoding.DecodeString(strings.ToUpper(acc.Hash[3:]))
	if err != nil {
		log.Printf("Cannot decode authentication hash for account %q: %v", acc.Name, err)
		return fmt.Errorf("cannot decode authentication hash for account %q", acc.Name)
	}

	hash2 := blake2b.Sum256(append([]byte(token), hash1[32:]...))
	if !bytes.Equal(hash1[:32], hash2[:]) {
		log.Printf("Invalid authentication for account %q.", acc.Name)
		return authError{fmt.Errorf("invalid authentication")}
	}

	return nil
}

func NewManager(dir string) (*Manager, error) {
	m := &Manager{
		dir: dir,
	}

	if _, err := os.Stat(*dirFlag); os.IsNotExist(err) {
		return nil, fmt.Errorf("directory %s does not exist", m.dir)
	}
	if err := os.MkdirAll(m.path("servers"), 0700); err != nil {
		return nil, fmt.Errorf("cannot write into %s directory: %v", m.dir, err)
	}
	if err := os.MkdirAll(m.path("images"), 0700); err != nil {
		return nil, fmt.Errorf("cannot write into %s directory: %v", m.dir, err)
	}
	if err := m.Reload(); err != nil {
		return nil, err
	}
	if err := m.cleanupServers(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) path(parts ...string) string {
	return filepath.Join(append([]string{m.dir}, parts...)...)
}

func (m *Manager) Hostname() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.setup.Hostname
}

func (m *Manager) Servers(account *Account) ([]*Server, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var servers []*Server
	for _, server := range m.servers {
		if account == nil || account.Admin || server.Account == account.Name {
			servers = append(servers, server)
		}
	}
	return servers, nil
}

func (m *Manager) Reload() error {
	data, err := ioutil.ReadFile(m.path("setup.yaml"))
	if err != nil {
		return fmt.Errorf("cannot read setup file: %v", err)
	}

	var setup Setup
	if err = yaml.Unmarshal(data, &setup); err != nil {
		return fmt.Errorf("cannot parse setup file: %v", err)
	}

	if setup.Hostname == "" {
		setup.Hostname = "localhost"
	}

	if err := m.checkSystem(&setup); err != nil {
		return err
	}

	m.mu.Lock()
	m.setup = setup
	m.mu.Unlock()

	if err := m.reloadServers(); err != nil {
		return err
	}
	m.ensureServersUp()
	return nil
}

func (m *Manager) checkSystem(setup *Setup) error {
	seen := make(map[string]bool)
	uid := os.Getuid()
	for _, image := range setup.Images {
		if image.Name == "" || image.Arch == "" || seen[image.Arch] {
			continue
		}
		if image.KVM && uid != 0 {
			return fmt.Errorf("must run as root so kvm works")
		}
		seen[image.Arch] = true
		if err := m.checkCommand("qemu-system-"+image.Arch, "--version"); err != nil {
			return err
		}
		imagePath := m.path("images", image.Name+".img")
		if _, err := os.Stat(imagePath); err != nil {
			return fmt.Errorf("cannot open image %q: %v", image.Name, err)
		}

		cmd := exec.Command("qemu-img", "check", image.Name+".img")
		cmd.Dir = m.path("images")
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("image %q failed checks: %v", image.Name, outputErr(output, err))
		}

		if image.UEFI {
			typical := "/usr/share/qemu-efi/QEMU_EFI.fd"
			actual := m.path("images", "QEMU_EFI.fd")
			_, err = os.Stat(actual)
			if os.IsNotExist(err) {
				if os.Link(typical, actual) == nil {
					log.Printf("Created %s as a hardlink from %s.", actual, typical)
				}
				_, err = os.Stat(actual)
			}
			if err != nil {
				return fmt.Errorf("cannot find UEFI firmware at %s or %s", actual, typical)
			}
		}
	}

	writeCheck := m.path("servers", ".write-check")
	if f, err := os.Create(writeCheck); err != nil {
		return fmt.Errorf("cannot write into %s directory: %v", m.dir, err)
	} else {
		f.Close()
	}
	if err := os.Remove(writeCheck); err != nil {
		return fmt.Errorf("cannot write into %s directory: %v", m.dir, err)
	}
	if err := m.checkCommand("genisoimage", "--version"); err != nil {
		return err
	}
	if err := m.checkCommand("qemu-img", "--version"); err != nil {
		return err
	}
	return nil
}

func (m *Manager) checkCommand(name string, args ...string) error {
	output, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("cannot find required command %q: %v", name, outputErr(output, err))
	}
	return nil
}

func (m *Manager) cleanupServers() error {
	names, err := m.serversOnDisk()
	if err != nil {
		return err
	}

	for _, name := range names {
		if m.serverReady(name) {
			continue
		}
		if err := os.RemoveAll(m.path("servers", name)); err != nil {
			log.Printf("WARNING: Cannot remove corrupted server %s: %v", name, err)
		} else {
			log.Printf("WARNING: Removed corrupted server %s.", name)
		}
	}
	return nil
}

func (m *Manager) ensureServersUp() {
	m.mu.Lock()
	var servers []*Server
	for _, server := range m.servers {
		servers = append(servers, server)
	}
	m.mu.Unlock()

	for _, server := range servers {
		pid, err := m.serverPid(server.Name)
		if err != nil || pid > 0 {
			continue
		}
		log.Printf("Found server %s down. Restarting it.", server.Name)
		_ = m.Start(server)
	}
}

func (m *Manager) serversOnDisk() (names []string, err error) {
	var dirnames []string
	dir, err := os.Open(m.path("servers"))
	if err == nil {
		dirnames, err = dir.Readdirnames(0)
		dir.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("cannot list servers: %v", err)
	}

	for _, name := range dirnames {
		if _, err := parseServerName(name); err == nil {
			names = append(names, name)
		}
	}
	return names, err
}

func (m *Manager) reloadServers() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	names, err := m.serversOnDisk()
	if err != nil {
		return err
	}

	servers := make(map[string]*Server)
	sshname := make(map[string]string)
	for _, name := range names {
		if !m.serverReady(name) {
			continue
		}

		data, err := ioutil.ReadFile(m.path("servers", name, "server.yaml"))
		if err != nil {
			log.Printf("WARNING: Cannot read server.yaml for server %s: %v", name, err)
			continue
		}

		var server Server
		if err := yaml.Unmarshal(data, &server); err != nil {
			log.Printf("WARNING: Cannot parse server.yaml for server %s: %v", name, err)
			continue
		}

		if server.Name != name {
			log.Printf("WARNING: Server %s is under incorrect directory %s", server.Name, name)
			continue
		}

		log.Printf("Loaded server %s", name)

		servers[name] = &server
		sshname[server.SSH] = name
	}

	for name, oldserver := range m.servers {
		if servers[name] != nil {
			servers[name] = oldserver
		}
	}

	m.servers = servers
	m.sshname = sshname

	return nil
}

func (m *Manager) Server(name string) (*Server, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	server, ok := m.servers[name]
	if !ok {
		log.Printf("Server %s not found.", name)
		return nil, notFoundError{fmt.Errorf("server %s not found", name)}
	}
	return server, nil
}

func (m *Manager) Image(name string) (*Image, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, image := range m.setup.Images {
		if image.Name == name {
			return image, nil
		}
	}
	log.Printf("Image %q not found.", name)
	return nil, notFoundError{fmt.Errorf("image %q not found", name)}
}

func (m *Manager) Account(name string) (*Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, account := range m.setup.Accounts {
		if account.Name == name {
			return account, nil
		}
	}
	log.Printf("Account %q not found.", name)
	return nil, authError{fmt.Errorf("invalid authentication")}
}

func (m *Manager) Start(server *Server) error {
	server.mu.Lock()
	defer server.mu.Unlock()

	pid, err := m.serverPid(server.Name)
	if err != nil || pid > 0 {
		return err
	}

	image, err := m.Image(server.Image)
	if err != nil {
		return err
	}

	_, port, err := net.SplitHostPort(server.SSH)
	if err != nil {
		log.Printf("Invalid SSH address allocated for server %s: %q", server.Name, server.SSH)
		return fmt.Errorf("invalid SSH address allocated for server %s: %q", server.Name, server.SSH)
	}

	if err := os.Remove(m.path("servers", server.Name, "qemu.pid")); err != nil && !os.IsNotExist(err) {
		log.Printf("Cannot remove previous qemu.pid from server %s: %v", server.Name, err)
		return fmt.Errorf("cannot remove previous qemu.pid from server %s: %v", server.Name, err)
	}

	args := make([]string, 0, 64)
	args = append(args,
		"qemu-system-"+image.Arch,
		"-name", server.Name,
		"-daemonize",
		"-runas", "nobody",
		"-pidfile", "qemu.pid",
		"-display", "none",
		"-smp", "4",
		"-m", "2G",
		"-cpu", "host",
		"-netdev", "user,id=net0,hostfwd=tcp::"+port+"-:22",
		"-device", "virtio-net-pci,netdev=net0",
		"-drive", "if=none,file=disk.img,id=hd0",
		"-device", "virtio-blk-pci,drive=hd0",
		"-drive", "media=cdrom,readonly,file=cloud-init.iso,if=virtio",
		"-serial", "file:console.out",
		"-qmp-pretty", "unix:qemu.qmp,server,nowait",
	)

	if image.Arch == "aarch64" {
		args = append(args, "-machine", "virt,gic-version=3")
	}
	if image.UEFI {
		args = append(args, "-pflash", "flash0.img", "-pflash", "flash1.img")
	}

	if image.KVM {
		args = append(args, "-enable-kvm")
	}

	qemuOut, err := os.Create(m.path("servers", server.Name, "qemu.out"))
	if err != nil {
		log.Printf("Cannot open qemu output file: %v", err)
		return fmt.Errorf("cannot open qemu output file: %v", err)
	}
	defer qemuOut.Close()

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = m.path("servers", server.Name)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = qemuOut
	cmd.Stderr = qemuOut

	if err := cmd.Run(); err != nil {
		log.Printf("Cannot start server %s: %s", server.Name, err)
		return fmt.Errorf("something went wrong while starting %s", server.Name)
	}
	return nil
}

func (m *Manager) Stop(server *Server) error {
	server.mu.Lock()
	defer server.mu.Unlock()

	return m.unlockedStop(server)
}

func (m *Manager) unlockedStop(server *Server) error {
	pid, err := m.serverPid(server.Name)
	if err != nil || pid == 0 {
		return err
	}
	if err := syscall.Kill(pid, 9); err != nil {
		return fmt.Errorf("cannot stop server: %v", err)
	}
	_ = os.Remove(m.path("servers", server.Name, "qemu.pid"))
	return nil
}

func (m *Manager) serverPid(name string) (int, error) {
	if !m.serverReady(name) {
		return 0, nil
	}

	data, err := ioutil.ReadFile(m.path("servers", name, "qemu.pid"))
	if err != nil && !os.IsNotExist(err) {
		log.Printf("Cannot read pid of server %s: %v", name, err)
		return 0, fmt.Errorf("cannot read pid of server %s: %v", name, err)
	}

	pidstr := string(bytes.TrimSpace(data))
	data, err = ioutil.ReadFile("/proc/" + pidstr + "/cmdline")
	if err == nil && strings.Contains(string(data), "\x00-name\x00"+name+"\x00") {
		pid, err := strconv.Atoi(pidstr)
		if err != nil {
			return 0, fmt.Errorf("internal error: invalid server process ID: %q", pid)
		}
		return pid, nil
	}
	return 0, nil
}

func (m *Manager) serverReady(name string) bool {
	_, err := os.Stat(m.path("servers", name, "server.yaml"))
	return !os.IsNotExist(err)
}

func (m *Manager) Allocate(account *Account, image *Image, password string, custom map[string]interface{}) (s *Server, err error) {

	for _, r := range password {
		if !unicode.IsPrint(r) {
			return nil, fmt.Errorf("password contains non-printable characters")
		}
	}

	server := &Server{
		Name:    createServerName(),
		Account: account.Name,
		Image:   image.Name,
		Birth:   time.Now().UTC(),
		Custom:  custom,
	}

	var serverPath string
	for i := 0; i < 1000; i++ {
		serverPath = m.path("servers", server.Name)
		if _, err := os.Stat(serverPath); os.IsNotExist(err) || err != nil {
			break
		}
	}
	if err := os.Mkdir(serverPath, 0700); err != nil {
		return nil, fmt.Errorf("cannot create directory for server %s: %v", server.Name, err)
	}

	// From here on if there are errors kill the data left behind.
	defer func() {
		if err != nil {
			os.RemoveAll(serverPath)
			m.mu.Lock()
			delete(m.sshname, server.SSH)
			m.mu.Unlock()
		}
	}()

	if err := m.allocateSSHAddr(server); err != nil {
		return nil, err
	}

	// Write both UEFI flash images.
	for i := 0; i < 2; i++ {
		flash, err := os.Create(m.path("servers", server.Name, fmt.Sprintf("flash%d.img", i)))
		if err == nil && i == 0 {
			firmware, err := os.Open(m.path("images", "QEMU_EFI.fd"))
			if err == nil {
				_, err = io.Copy(flash, firmware)
				firmware.Close()
			}
			if err != nil {
				flash.Close()
				log.Printf("Cannot copy UEFI firmware: %v", err)
				return nil, fmt.Errorf("cannot copy UEFI firmware: %v", err)
			}
		}
		if err == nil {
			_, err = flash.Seek(64*1024*1024-1, 0)
		}
		if err == nil {
			_, err = flash.Write([]byte{0})
		}
		if err != nil {
			log.Printf("Cannot create UEFI boot image: %v", err)
			return nil, fmt.Errorf("cannot create UEFI boot image: %v", err)
		}
	}

	// Write disk.img
	cmd := exec.Command("qemu-img", "create", "-f", "qcow2", "-o", "backing_file=../../images/"+image.Name+".img,size=20G", "disk.img")
	cmd.Dir = serverPath
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("Cannot create disk for server %s: %q", server.Name, outputErr(output, err))
		return nil, fmt.Errorf("cannot create disk for server %s: %v", server.Name, outputErr(output, err))
	}

	// Write cloud-init seed disk.
	userdataPath := m.path("servers", server.Name, "user-data")
	metadataPath := m.path("servers", server.Name, "meta-data")
	err = ioutil.WriteFile(userdataPath, []byte(fmt.Sprintf(cloudInitUser, password)), 0600)
	if err != nil {
		log.Printf("Cannot write user-data for server %s: %v", server.Name, err)
		return nil, fmt.Errorf("cannot write user-data for server %s: %v", server.Name, err)
	}
	err = ioutil.WriteFile(metadataPath, []byte(fmt.Sprintf(cloudInitMeta, server.Name)), 0600)
	if err != nil {
		log.Printf("Cannot write meta-data for server %s: %v", server.Name, err)
		return nil, fmt.Errorf("cannot write meta-data for server %s: %v", server.Name, err)
	}
	cmd = exec.Command("genisoimage", "-output", "cloud-init.iso", "-input-charset", "utf-8", "-volid", "cidata", "-joliet", "-rock", "user-data", "meta-data")
	cmd.Dir = serverPath
	output, err = cmd.CombinedOutput()
	if err != nil {
		log.Printf("Cannot create cloud-init seed image for server %s: %v", server.Name, outputErr(output, err))
		return nil, fmt.Errorf("cannot create cloud-init seed image for server %s: %v", server.Name, outputErr(output, err))
	}

	_ = os.Remove(userdataPath)
	_ = os.Remove(metadataPath)

	// Write server.yaml
	yamlPath := m.path("servers", server.Name, "server.yaml")
	data, err := yaml.Marshal(&server)
	if err != nil {
		log.Printf("Cannot generate control data for server %s: %v", server.Name, err)
		return nil, fmt.Errorf("cannot generate control data for server %s: %v", server.Name, err)
	}
	err = ioutil.WriteFile(yamlPath+"~", data, 0600)
	if err != nil {
		log.Printf("Cannot write control data for server %s: %v", server.Name, err)
		return nil, fmt.Errorf("cannot write control data for server %s: %v", server.Name, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if err = atomicRename(yamlPath+"~", yamlPath); err != nil {
		return nil, err
	}
	m.servers[server.Name] = server

	return server, nil
}

func (m *Manager) allocateSSHAddr(server *Server) error {
	for i := 0; i < 10; i++ {
		port := 20000 + rand.Intn(40000)
		addr := fmt.Sprintf("%s:%d", m.Hostname(), port)
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			continue
		}
		m.mu.Lock()
		_, used := m.sshname[addr]
		if !used {
			m.sshname[addr] = server.Name
		}
		m.mu.Unlock()
		if used {
			continue
		}
		server.SSH = addr
		return nil
	}
	hostname := m.Hostname()
	log.Printf("Cannot find port available on host %q", hostname)
	return fmt.Errorf("cannot find port available on host %q", hostname)
}

func (m *Manager) Deallocate(server *Server) error {
	server.mu.Lock()
	defer server.mu.Unlock()

	if err := m.unlockedStop(server); err != nil {
		return err
	}

	yamlPath := m.path("servers", server.Name, "server.yaml")
	if err := atomicRename(yamlPath, yamlPath+"~"); err != nil {
		return err
	}

	delete(m.servers, server.Name)
	delete(m.sshname, server.SSH)

	if err := os.RemoveAll(m.path("servers", server.Name)); err != nil {
		log.Printf("WARNING: Cannot cleanly remove server %s: %v", server.Name, err)
	}
	return nil
}

const cloudInitUser = `#cloud-config
datasource_list: [None]
disable_root: False
ssh_pwauth: True
chpasswd: 
  list: |
    root:%s
  expire: False
runcmd:
  - sed -i 's/^\s*#\?\s*\(PermitRootLogin\|PasswordAuthentication\)\>.*/\1 yes/' /etc/ssh/sshd_config
  - pkill -HUP sshd
`

const cloudInitMeta = `
instance-id: %[1]s
local-hostname: %[1]s
`

const serverNameLayout = "Jan021504.000"

func createServerName() string {
	return strings.ToLower(strings.Replace(time.Now().Format(serverNameLayout), ".", "-", 1))
}

func parseServerName(name string) (time.Time, error) {
	t, err := time.Parse(serverNameLayout, strings.Replace(name, "-", ".", 1))
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid Google machine name for spread: %s", name)
	}
	return t, nil
}

func outputErr(output []byte, err error) error {
	output = bytes.TrimSpace(output)
	if len(output) > 0 {
		if bytes.Contains(output, []byte{'\n'}) {
			err = fmt.Errorf("\n-----\n%s\n-----", output)
		} else {
			err = fmt.Errorf("%s", output)
		}
	}
	return err
}

func atomicRename(oldName, newName string) error {
	dir, err := os.Open(filepath.Dir(oldName))
	if err == nil {
		defer dir.Close()
	}
	if err == nil {
		err = dir.Sync()
	}
	if err == nil {
		err = os.Rename(oldName, newName)
	}
	if err == nil {
		err = dir.Sync()
	}
	if err == nil {
		return nil
	}
	log.Printf("Cannot rename %s atomically: %v", newName, err)
	return fmt.Errorf("cannot rename %s atomically: %v", newName, err)
}
