package spread

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-goose/goose/v5/client"
	"github.com/go-goose/goose/v5/glance"
	"github.com/go-goose/goose/v5/identity"
	"github.com/go-goose/goose/v5/neutron"
	"github.com/go-goose/goose/v5/nova"

	"golang.org/x/crypto/ssh"
	"golang.org/x/net/context"

	"strconv"
)

func Openstack(p *Project, b *Backend, o *Options) Provider {
	return &openstackProvider{
		project: p,
		backend: b,
		options: o,

		imagesCache: make(map[string]*openstackImagesCache),
	}
}

type openstackProvider struct {
	project *Project
	backend *Backend
	options *Options

	openstackProject string
	openstackAvailabilityZone  string

	region string	
	computeClient *nova.Client
	networkClient *neutron.Client
	imageClient *glance.Client

	mu sync.Mutex

	keyChecked bool
	keyErr     error

	imagesCache map[string]*openstackImagesCache
}

type openstackServer struct {
	p *openstackProvider
	d openstackServerData

	system  *System
	address string
}

type openstackServerData struct {
	Name    string
	Flavor  string    `json:"machineType"`
	Status  string    `yaml:"-"`
	Created time.Time `json:"creationTimestamp"`

	Labels map[string]string `yaml:"-"`
}

func (d *openstackServerData) cleanup() {
	if i := strings.LastIndex(d.Flavor, "/"); i >= 0 {
		d.Flavor = d.Flavor[i+1:]
	}
}

func (s *openstackServer) String() string {
	if s.system == nil {
		return s.d.Name
	}
	return fmt.Sprintf("%s (%s)", s.system, s.d.Name)
}

func (s *openstackServer) Label() string {
	return s.d.Name
}

func (s *openstackServer) Provider() Provider {
	return s.p
}

func (s *openstackServer) Address() string {
	return s.address
}

func (s *openstackServer) System() *System {
	return s.system
}

func (s *openstackServer) ReuseData() interface{} {
	return &s.d
}

const (
	openstackStaging      = "STAGING"
	openstackProvisioning = "PROVISIONING"
	openstackRunning      = "RUNNING"
	openstackStopping     = "STOPPING"
	openstackStopped      = "STOPPED"
	openstackSuspending   = "SUSPENDING"
	openstackTerminating  = "TERMINATED"

	openstackPending = "PENDING"
	openstackDone    = "DONE"
)

func (p *openstackProvider) Backend() *Backend {
	return p.backend
}

func (p *openstackProvider) Reuse(ctx context.Context, rsystem *ReuseSystem, system *System) (Server, error) {
	s := &openstackServer{
		p:       p,
		address: rsystem.Address,
		system:  system,
	}
	err := rsystem.UnmarshalData(&s.d)
	if err != nil {
		return nil, fmt.Errorf("cannot unmarshal openstack reuse data: %v", err)
	}
	return s, nil
}

func (p *openstackProvider) Allocate(ctx context.Context, system *System) (Server, error) {
	if err := p.checkKey(); err != nil {
		return nil, err
	}

	s, err := p.createMachine(ctx, system)
	if err != nil {
		return nil, err
	}

	printf("Allocated %s.", s)
	return s, nil
}

func (s *openstackServer) Discard(ctx context.Context) error {
	return s.p.removeMachine(ctx, s)
}

const openstackCloudInitScript = `
#cloud-config
runcmd:
  - echo root:%s | chpasswd
  - sed -i 's/^\s*#\?\s*\(PermitRootLogin\|PasswordAuthentication\)\>.*/\1 yes/' /etc/ssh/sshd_config
  - test -d /etc/ssh/sshd_config.d && echo 'PermitRootLogin=yes' > /etc/ssh/sshd_config.d/00-spread.conf
  - test -d /etc/ssh/sshd_config.d && echo 'PasswordAuthentication=yes' >> /etc/ssh/sshd_config.d/00-spread.conf
  - pkill -o -HUP sshd || true
`
const openstackReadyMarker = "MACHINE-IS-READY"
const openstackNameLayout = "Jan021504.000000"
const openstackDefaultFlavor = "m1.medium"

type openstackImage struct {
	Project string
	Name    string
	Family  string
	Terms   []string
}

type openstackImagesCache struct {
	mu     sync.Mutex
	ready  bool
	images []openstackImage
	err    error
}

func openstackName() string {
	return strings.ToLower(strings.Replace(time.Now().UTC().Format(openstackNameLayout), ".", "-", 1))
}

func (p *openstackProvider) findFlavorId(flavorName string) (string, error) {
	flavors, err := p.computeClient.ListFlavors()
	if err != nil {
		return "", fmt.Errorf("failed to retrieve flavors list: %v", err)
	}

	var flavorId string
	for _, flavor := range flavors {
		if flavor.Name == flavorName {
			flavorId = flavor.Id
			break
		}
	}

	if flavorId == "" {
		return "", fmt.Errorf("specified flavor not found: %s", flavorName)
	}

	return flavorId, nil
}

func (p *openstackProvider) findNetworkId() (string, error) {
	networks, err := p.networkClient.ListNetworksV2()
	if err != nil {
		return "", fmt.Errorf("failed to retrieve networks list: %v", err)
	}

	var netId string
	for _, net := range networks {
		if net.External == true {
			netId = net.Id
			break
		}
	}
	if netId == "" {
		return "", fmt.Errorf("no valid network found to create floating IP")
	}

	return netId, nil
}

func (p *openstackProvider) findImageId(imageName string) (string, error) {
    images, err := p.imageClient.ListImagesDetail()
	if err != nil {
		return "", fmt.Errorf("failed to retrieve images list: %v", err)
	}

    var sameImage glance.ImageDetail
    var lastImage glance.ImageDetail
	for _, i := range images {
		if i.Name == imageName {
            sameImage = i
        } else if strings.Contains(i.Name, imageName) {
        	// Check if the creation date for the current image is after the previous selected one
        	currCreatedDate, err := time.Parse(time.RFC3339, i.Created)
        	if err != nil {
				return "", fmt.Errorf("error parsing image: %v", err)
			}
        	lastCreatedDate, err := time.Parse(time.RFC3339, lastImage.Created)
			if err != nil {
				return "", fmt.Errorf("error parsing image: %v", err)
			}
			if currCreatedDate.After(lastCreatedDate) {
        		lastImage = i
        	}
        }
	}

    // return the image when it matchs exactly with the provided name
	if sameImage.Id != "" {		
        return sameImage.Id, nil
    }    

    if lastImage.Id != "" {
        return lastImage.Id, nil
    }

    return "", fmt.Errorf("No matching image found")
}

func (p *openstackProvider) waitServerCompleteBuilding(s *openstackServer, timeoutSeconds int) error {
	// Wait until the server is actually running
	start := time.Now()
	for {
		if time.Since(start) > time.Duration(timeoutSeconds) * time.Second {			
			return &FatalError{fmt.Errorf("timeout reached checking status")}
		}
		server, err := p.computeClient.GetServer(s.d.Name)
		if err != nil {
			return fmt.Errorf("error retrieving server information: %v", err)
		}
		if server.Status != nova.StatusBuild {
			if server.Status != nova.StatusActive {
				return fmt.Errorf("server status is not active: %s", server.Status)
			}
			break
		}
		time.Sleep(5 * time.Second)
	}
	debugf("server %s is running", s.d.Name)
	return nil
}

func (p *openstackProvider) waitServerCompleteSetup(s *openstackServer, timeoutSeconds int) error {
	server, err := p.computeClient.GetServer(s.d.Name)
	if err != nil {
		return fmt.Errorf("error retrieving server information: %v", err)
	}
	// The adreesses for a network is map of networks and list of ip adresses
	// We are configuring just 1 network address for the network
	s.address = server.Addresses[p.backend.Network][0].Address

	config := &ssh.ClientConfig{
		User:            "root",
		Auth:            []ssh.AuthMethod{ssh.Password(p.options.Password)},
		Timeout:         10 * time.Second,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}
	addr := s.address
	if !strings.Contains(addr, ":") {
		addr += ":22"
	}

	// Iterate until the ssh connection to the host can be stablished
	// In openstack the client cannot access to the serial console of the instance
	start := time.Now()
    for {
        if time.Since(start) > time.Duration(timeoutSeconds) * time.Second {
			return &FatalError{fmt.Errorf("failed to ssh to the allocated instance")}
        }

        _, err = ssh.Dial("tcp", addr, config)
        if err == nil {
            break
        }

        time.Sleep(2 * time.Second)
    }
    debugf("connection to server %s is stablished", s.d.Name)
    return nil
}

func (p *openstackProvider) createMachine(ctx context.Context, system *System) (*openstackServer, error) {
	debugf("Creating new openstack server for %s...", system.Name)

	name := openstackName()
	flavor := openstackDefaultFlavor
	if system.Plan != "" {
		flavor = system.Plan
	}
	flavorId, err := p.findFlavorId(flavor)
	if err != nil {
		return nil, err
	}

	networkID, err := p.findNetworkId()
	if err != nil {
		return nil, err
	}

	imageID, err := p.findImageId(system.Image)
	if err != nil {
		return nil, err
	}	

	// cloud init script
	cloudconfig := fmt.Sprintf(openstackCloudInitScript, p.options.Password)

	// tags to the created instance
	tags := map[string]string{
		"spread":   "true",
		"owner":    strings.ToLower(username()),
		"reuse":    strconv.FormatBool(p.options.Reuse),
		"password": p.options.Password,
	}

	opts := nova.RunServerOpts{
		Name:             name,
		FlavorId:         flavorId,
		ImageId:          imageID,
		AvailabilityZone: p.region,
		Networks:         []nova.ServerNetworks{{NetworkId: networkID,}},
		Metadata:   	  tags,
		UserData:         []byte(cloudconfig),
	}
	server, err := p.computeClient.RunServer(opts)
	if err != nil {
		return nil, &FatalError{fmt.Errorf("Could not create instance", err)}
	}

	s := &openstackServer{
		p: p,
		d: openstackServerData{
			Name:    server.Id,
			Flavor:  flavor,
			Status:  openstackProvisioning,
			Created: time.Now(),
		},

		system: system,
	}

	// First we need to wait until the image is active and there is no erros during the spawning process
	// The timeout for this process is 180 seconds
	err = p.waitServerCompleteBuilding(s, 180)
	if err != nil {
		if p.removeMachine(ctx, s) != nil {
			return nil, &FatalError{fmt.Errorf("cannot allocate or deallocate (!) new openstack server %s: %v", s, err)}
		}
		return nil, &FatalError{fmt.Errorf("cannot allocate new openstack server %s: %v", s, err)}
	}

	// Connect through ssh to the
	err = p.waitServerCompleteSetup(s, 300)
	if err != nil {
		if p.removeMachine(ctx, s) != nil {
			return nil, &FatalError{fmt.Errorf("cannot allocate or deallocate (!) openstack server %s: %v", s, err)}
		}
		return nil, &FatalError{fmt.Errorf("cannot stablish ssh connection to the openstach server %s: %v", s, err)}
	}

	return s, nil
}

func (p *openstackProvider) list() ([]*openstackServer, error) {
	debug("Listing available openstack instances...")

	filter := nova.NewFilter()
	servers, err := p.computeClient.ListServersDetail(filter)
	if err != nil {
		return nil, &FatalError{fmt.Errorf("cannot list openstack instances: %v", err)}
	}

	var instances []*openstackServer
	for _, s := range servers {
		val, ok := s.Metadata["spread"]
		if ok && val == "true" {
			createdTime, err := time.Parse(time.RFC3339, s.Created)
			if err != nil {
				return nil, &FatalError{fmt.Errorf("cannot parse creation date for instances: %v", err)}
			}
			d := openstackServerData{
				Name:    s.Name,
				Created: createdTime,
			}
			instances = append(instances, &openstackServer{p: p, d: d})
		}
	}
	return instances, nil
}

func (p *openstackProvider) removeMachine(ctx context.Context, s *openstackServer) error {
	return p.computeClient.DeleteServer(s.d.Name)
}

func (p *openstackProvider) GarbageCollect() error {
	instances, err := p.list()
	if err != nil {
		return err
	}

	now := time.Now()
	haltTimeout := p.backend.HaltTimeout.Duration

	// Iterate over all the running instances
	for _, s := range instances {
		serverTimeout := haltTimeout
		if value, ok := s.d.Labels["halt-timeout"]; ok {
			d, err := time.ParseDuration(strings.TrimSpace(value))
			if err != nil {
				printf("WARNING: Ignoring bad openstack instances %s halt-timeout label: %q", s, value)
			} else {
				serverTimeout = d
			}
		}

		if serverTimeout == 0 {
			continue
		}

		printf("Checking %s...", s)

		runningTime := now.Sub(s.d.Created)
		if runningTime > serverTimeout {
			printf("Server %s exceeds halt-timeout. Shutting it down...", s)
			err := p.removeMachine(context.Background(), s)
			if err != nil {
				printf("WARNING: Cannot garbage collect %s: %v", s, err)
			}
		}
	}
	return nil
}

const (
	openstackMissingProject = "MISSING-PROJECT"
	openstackMissingRegion  = "MISSING-REGION"
)

func (p *openstackProvider) aRegion() string {
	if len(p.backend.Location) > 0 {
		return p.backend.Location
	}
	return openstackMissingRegion
}

func (p *openstackProvider) checkKey() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.keyChecked {
		return p.keyErr
	}

	var err error

	if err == nil && p.computeClient == nil {
		// retrieve variables used to authenticate from the environment
		cred, err := identity.CompleteCredentialsFromEnv()
		if err != nil {
			return &FatalError{fmt.Errorf("cannot retrieve credentials from env: %v", err)}
		}

		authClient := client.NewClient(cred, identity.AuthUserPass, nil)
		if err = authClient.Authenticate(); err != nil {
			return &FatalError{fmt.Errorf("error authenticating: %v", err)}
		}

		// Create clients for the used modules
		p.region = cred.Region
		p.computeClient = nova.New(authClient)
		p.networkClient = neutron.New(authClient)
		p.imageClient = glance.New(authClient)
		p.keyErr = err
	}

	p.keyChecked = true
	p.keyErr = err
	return err
}
