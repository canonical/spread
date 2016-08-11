package spread

import (
	"fmt"
	"gopkg.in/yaml.v2"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

func SSH(b *Backend) Provider {
	return &sshBackend{b}
}

type sshBackend struct {
	backend *Backend
}

type sshServer struct {
	l *sshBackend
	d sshServerData
}

type sshServerData struct {
	Name    string
	Backend string
	System  string
	Address string
	Port    int
}

func (s *sshServer) String() string {
	return fmt.Sprintf("%s:%s", s.l.backend.Name, s.d.System)
}

func (s *sshServer) Provider() Provider {
	return s.l
}

func (s *sshServer) Address() string {
	return s.d.Address
}

func (s *sshServer) Port() int {
	return s.d.Port
}

func (s *sshServer) System() string {
	return s.d.System
}

func (s *sshServer) ReuseData() []byte {
	data, err := yaml.Marshal(&s.d)
	if err != nil {
		panic(err)
	}
	return data
}

func (s *sshServer) Discard() error {
	return nil
}

func (l *sshBackend) Backend() *Backend {
	return l.backend
}

func (l *sshBackend) Reuse(data []byte, password string) (Server, error) {
	server := &sshServer{}
	err := yaml.Unmarshal(data, &server.d)
	if err != nil {
		return nil, fmt.Errorf("cannot unmarshal ssh reuse data: %v", err)
	}
	server.l = l
	return server, nil
}

func (l *sshBackend) Allocate(system string, password string, keep bool) (Server, error) {
	n := strings.LastIndex(system, "-")
	name := system[:n]
	port, err := strconv.Atoi(system[n+1:])
	if err != nil {
		return nil, err
	}

	server := &sshServer{
		l: l,
		d: sshServerData{
			System:  name,
			Address: name,
			Port:    port,
			// Name? Backend?
		},
	}

	err = server.addRootUserSSH(password)
	if err != nil {
		server.Discard()
		return nil, err
	}

	printf("Allocated %s.", server.String())
	return server, nil
}

func (l *sshServer) sshConnect() (*ssh.Client, error) {
	user := os.Getenv("SPREAD_SSH_USER")
	if user == "" {
		user = "ubuntu"
	}
	pw := os.Getenv("SPREAD_SSH_PASSWORD")
	if pw == "" {
		pw = "ubuntu"
	}

	config := &ssh.ClientConfig{
		User:    user,
		Auth:    []ssh.AuthMethod{ssh.Password(pw)},
		Timeout: 10 * time.Second,
	}
	addr := fmt.Sprintf("%s:%d", l.Address(), l.Port())
	return ssh.Dial("tcp", addr, config)
}

func (server *sshServer) addRootUserSSH(password string) error {
	sshc, err := server.sshConnect()
	if err != nil {
		return fmt.Errorf("cannot connect to %s:%d: %s", server.Address(), server.Port(), err)
	}
	defer sshc.Close()

	// enable ssh root login, set root password and restart sshd
	cmds := []string{
		`sudo sed -i 's/\(PermitRootLogin\|PasswordAuthentication\)\>.*/\1 yes/' /etc/ssh/sshd_config`,
		fmt.Sprintf(`sudo /bin/bash -c "%s"`, fmt.Sprintf("echo root:%s | chpasswd", password)),
		"sudo systemctl reload sshd",
	}
	for _, cmd := range cmds {
		session, err := sshc.NewSession()
		if err != nil {
			return err
		}
		output, err := session.CombinedOutput(cmd)
		session.Close()
		if err != nil {
			return fmt.Errorf("cannot prepare sshd in ssh container %q: %v", server.Port(), outputErr(output, err))
		}
	}
	return nil
}
