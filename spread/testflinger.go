package spread

import (
	"fmt"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/net/context"
)

func TestFlinger(p *Project, b *Backend, o *Options) Provider {
	return &tfProvider{p, b, o}
}

type tfProvider struct {
	project *Project
	backend *Backend
	options *Options
}

type tfServer struct {
	p *tfProvider
	d tfServerData

	system  *System
	address string
}

type tfServerData struct {
	Name  string
	JobId string
}

type tfFile struct {
	Queue          string              `yaml:"job_queue"`
	ProvisionDdata *tfProvisioningData `yaml:"provision_data"`
	TestData       *tfTestData         `yaml:"test_data"`
	ReserveData    *tfReserveData      `yaml:"reserve_data"`
}

var (
	tfconfigfile = "testflinger.conf."
	tfuser       = "ubuntu"
	tfpassword   = "ubuntu"
	tfpolltime   = 10
)

type tfReserveData struct {
	SSHKeys []string `yaml:"ssh_keys"`
}

type tfTestData struct {
	TestCommands []string `yaml:"test_cmds"`
}

type tfProvisioningData struct {
	Url string
}

func (s *tfServer) String() string {
	return fmt.Sprintf("%s (%s)", s.system, s.d.Name)
}

func (s *tfServer) Label() string {
	return s.d.Name
}

func (s *tfServer) Provider() Provider {
	return s.p
}

func (s *tfServer) Address() string {
	return s.address
}

func (s *tfServer) System() *System {
	return s.system
}

func (s *tfServer) ReuseData() interface{} {
	return &s.d
}

func (s *tfServer) Discard(ctx context.Context) error {
	output, err := exec.Command("testflinger-cli", "cancel", s.d.JobId).CombinedOutput()
	if err != nil {
		return fmt.Errorf("cannot discard testflinger reservation: %v", outputErr(output, err))
	}
	return nil
}

func (p *tfProvider) Backend() *Backend {
	return p.backend
}

func (p *tfProvider) GarbageCollect() error {
	return nil
}

func (p *tfProvider) Reuse(ctx context.Context, rsystem *ReuseSystem, system *System) (Server, error) {
	s := &tfServer{
		p:       p,
		system:  system,
		address: rsystem.Address,
	}
	err := rsystem.UnmarshalData(&s.d)
	if err != nil {
		return nil, fmt.Errorf("cannot unmarshal testflinger reuse data: %v", err)
	}
	return s, nil
}

func (p *tfProvider) Allocate(ctx context.Context, system *System) (Server, error) {
	image := system.Image
	name := tfName(system)

	key := ""
	if p.backend.Key != "" {
		key = p.backend.Key
	}

	file := &tfFile{
		Queue: name,
		ProvisionDdata: &tfProvisioningData{
			Url: image,
		},
		TestData: &tfTestData{
			TestCommands: []string{"ssh $DEVICE_IP cat /proc/cpuinfo",
				"ssh $DEVICE_IP echo ubuntu:ubuntu | sudo chpasswd",
				"ssh $DEVICE_IP echo 'ubuntu ALL=(ALL) NOPASSWD:ALL' | sudo tee /etc/sudoers.d/create-user-ubuntu"},
		},
		ReserveData: &tfReserveData{
			SSHKeys: []string{key},
		},
	}

	d, err := yaml.Marshal(file)
	if err != nil {
		return nil, fmt.Errorf("error: %v", err)
	}

	tmpfile, err := ioutil.TempFile(".", tfconfigfile)
	if err != nil {
		return nil, fmt.Errorf("Error creating temporal file: %v", err)
	}
	defer os.Remove(tmpfile.Name())

	if _, err := tmpfile.Write(d); err != nil {
		return nil, fmt.Errorf("Error writing temporal file: %v", err)
	}
	if err := tmpfile.Close(); err != nil {
		return nil, fmt.Errorf("Error closing temporal file: %v", err)
	}

	// First step is to get the job_id running the submit command
	jobId := ""

	out, err := exec.Command("/snap/bin/testflinger-cli", "submit", tmpfile.Name()).Output()
	if err != nil {
		return nil, fmt.Errorf("Error running command: %v: ", err)
	}
	lines := strings.Split(string(out), "\n")

	for _, line := range lines {
		if strings.HasPrefix(line, "job_id:") {
			parts := strings.Split(line, ":")
			if len(parts) == 2 {
				jobId = strings.TrimSpace(parts[1])
				printf("JobId created for testflinger job: %s", jobId)
				break
			} else {
				return nil, fmt.Errorf("JobId not found")
			}
		}
	}

	s := &tfServer{
		p: p,
		d: tfServerData{
			Name:  name,
			JobId: jobId,
		},
		system: system,
	}

	// Second step is to wait until the ip device reserved
	printf("Waiting for testflinger %s to have an address...", name)

	timeout := time.After(30 * time.Minute)
	warn := time.NewTicker(1 * time.Minute)
	retry := time.NewTicker(15 * time.Second)
	defer retry.Stop()
	for {
		cmd := exec.Command("/snap/bin/testflinger-cli", "status", jobId)
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("Error running command: %v", err)
		}
		line := strings.Split(string(out), "\n")[0]
		state := strings.TrimSpace(line)

		if state == "reserve" {
			printf("System already reserved, detecting ip...")
			break
		}

		if state == "complete" {
			return nil, fmt.Errorf("System already in complete state")
		}

		select {
		case <-retry.C:
		case <-warn.C:
			printf("Warning, system already on %s state", state)
		case <-timeout:
			s.Discard(ctx)
			return nil, fmt.Errorf("System not reserved")
		}

	}

	// wait until the ip is published (poll buffers 10 seconds until it outputs all the data)
	time.Sleep(time.Duration(tfpolltime) * time.Second)

	// third step is to get the user and ip to connect to the device
	timeout = time.After(2 * time.Minute)
	retry = time.NewTicker(time.Duration(tfpolltime) * time.Second)
	for {
		out, err = exec.Command("/snap/bin/testflinger-cli", "poll", "-o", jobId).Output()
		if err != nil {
			return nil, fmt.Errorf("Error running command: %v", err)
		}
		lines = strings.Split(string(out), "\n")

		for _, line := range lines {
			if strings.HasPrefix(line, "You can now connect to") {
				parts := strings.Split(line, "@")
				if len(parts) == 2 {
					s.address = strings.TrimSpace(parts[1])
					printf("Allocated device with ip %s", s.address)
					return s, nil
				} else {
					s.Discard(ctx)
					return nil, fmt.Errorf("ip address not found")
				}
			}
		}

		select {
		case <-retry.C:
		case <-timeout:
			s.Discard(ctx)
			return nil, fmt.Errorf("Ip not published")
		}
	}

	return nil, fmt.Errorf("ip address not found")
}

func tfName(system *System) string {
	if system.Queue != "" {
		return system.Queue
	}

	parts := strings.Split(system.Name, "-")
	if len(parts) > 1 {
		return parts[0]
	} else {
		return system.Name
	}
}
