package spread

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rackspace/gophercloud"
	"github.com/rackspace/gophercloud/openstack"
	"github.com/rackspace/gophercloud/openstack/compute/v2/servers"
	"github.com/rackspace/gophercloud/openstack/compute/v2/images"
	"github.com/rackspace/gophercloud/openstack/networking/v2/networks"
	"github.com/rackspace/gophercloud/pagination"

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

	computeClient *gophercloud.ServiceClient
	networkClient *gophercloud.ServiceClient
	imageClient *gophercloud.ServiceClient

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

func (p *openstackProvider) createMachine(ctx context.Context, system *System) (*openstackServer, error) {
	debugf("Creating new openstack server for %s...", system.Name)

	name := openstackName()
	flavor := openstackDefaultFlavor
	if system.Plan != "" {
		flavor = system.Plan
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

	networkID, err := networks.IDFromName(p.networkClient, p.backend.Network)
	network, err := networks.Get(p.networkClient, networkID).Extract()
	if err != nil {
		return nil, &FatalError{fmt.Errorf("Could not retrieve network for server", err)}
	}

	// This is disabled because the glance module tested is returning error to retrieve the list of images
	//image, err := p.findImage(system.Image)
	//if err != nil {
	//	return nil, &FatalError{fmt.Errorf("Could not retrieve image for server", err)}
	//}
	image := system.Image

	server, err := servers.Create(p.computeClient, servers.CreateOpts{
		Name:       name,
		ImageName:  image,
		FlavorName: flavor,
		UserData:   []byte(cloudconfig),
		Metadata:   tags,
		Networks:   []servers.Network{servers.Network{UUID: network.ID,}},
	}).Extract()
	if err != nil {
		return nil, &FatalError{fmt.Errorf("Could not create instance", err)}
	}

	s := &openstackServer{
		p: p,
		d: openstackServerData{
			Name:    server.ID,
			Flavor:  flavor,
			Status:  openstackProvisioning,
			Created: time.Now(),
		},

		system: system,
	}

	// First we need to wait until the image is active and there is no erros during the spawning process
	err = servers.WaitForStatus(p.computeClient, server.ID, "ACTIVE", 180)
	if err != nil {
		if p.removeMachine(ctx, s) != nil {
			return nil, &FatalError{fmt.Errorf("cannot allocate or deallocate (!) new openstack server %s: %v", s, err)}
		}
		return nil, &FatalError{fmt.Errorf("cannot allocate new openstack server %s: %v", s, err)}
	}

	instance, _ := servers.Get(p.computeClient, server.ID).Extract()
	// The adreesses for a network is a list of maps, and we are configuring just 1 network address
	networkInfo := instance.Addresses[p.backend.Network].([]interface{})[0].(map[string]interface{})
	s.address = fmt.Sprintf("%v", networkInfo["addr"])

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

	// Iterate 5 minutes until the ssh connection to the host can be stablished
	// In openstack the client cannot access to the serial console of the instance
	start := time.Now()
    for {
        if time.Since(start) > 5*time.Minute {
   			if p.removeMachine(ctx, s) != nil {
				return nil, &FatalError{fmt.Errorf("cannot deallocate new openstack server %s: %v", s, err)}
			}
			return nil, &FatalError{fmt.Errorf("failed to ssh to the allocated instance")}
        }

        _, err = ssh.Dial("tcp", addr, config)
        if err == nil {
            break
        }

        time.Sleep(2 * time.Second)
    }

	return s, nil
}

func (p *openstackProvider) findImage(imageName string) (string, error) {
    var sameImage images.Image
    var lastImage images.Image

    options := &images.ListOpts{Limit: 100, Name: imageName}
    err := images.ListDetail(p.imageClient, options).EachPage(func(page pagination.Page) (bool, error) {
        imageList, err := images.ExtractImages(page)
        if err != nil {
            return false, err
        }

        for _, i := range imageList {
            if i.Name == imageName {
                sameImage = i
            } else if strings.Contains(i.Name, imageName) {
            	// Check if the creation date for the current image is after the previous selected one
            	currCreatedDate, err := time.Parse(time.RFC3339, i.Created)
            	if err != nil {
					return false, &FatalError{fmt.Errorf("error parsing image: %v", err)}
				}
            	lastCreatedDate, err := time.Parse(time.RFC3339, lastImage.Created)
				if err != nil {
					return false, &FatalError{fmt.Errorf("error parsing image: %v", err)}
				}
				if currCreatedDate.After(lastCreatedDate) {
            		lastImage = i
            	}
            }
        }

        return true, nil
    })

    if err != nil {
        return "", err
    }

    // return the image when it matchs exactly with the provided name
	if sameImage.ID != "" {		
        return sameImage.Name, nil
    }    

    if lastImage.ID != "" {
        return lastImage.Name, nil
    }

    return "", fmt.Errorf("No matching image found")
}

func (p *openstackProvider) list() ([]*openstackServer, error) {
	debug("Listing available openstack instances...")

	// Retrieve a pager (i.e. a paginated collection)
	opts := servers.ListOpts{}
	pager := servers.List(p.computeClient, opts)
	var instances []*openstackServer

	// Define an anonymous function to be executed on each page's iteration
	err := pager.EachPage(func(page pagination.Page) (bool, error) {
		serverList, err := servers.ExtractServers(page)
		if err != nil {
			return false, &FatalError{fmt.Errorf("cannot list openstack instances: %v", err)}
		}

		for _, s := range serverList {
			val, ok := s.Metadata["spread"]
			if ok && val == "true" {
				createdTime, err := time.Parse(time.RFC3339, s.Created)
				if err != nil {
					return false, &FatalError{fmt.Errorf("cannot list openstack instances: %v", err)}
				}
				d := openstackServerData{
					Name:    s.Name,
					Created: createdTime,
				}
				instances = append(instances, &openstackServer{p: p, d: d})
			}
		}
		return true, nil
	})
	if err != nil {
		return nil, &FatalError{fmt.Errorf("cannot list openstack instances: %v", err)}
	}

	return instances, nil
}

func (p *openstackProvider) removeMachine(ctx context.Context, s *openstackServer) error {
	result := servers.Delete(p.computeClient, s.d.Name)
	return result.Err
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

	if p.aRegion() == openstackMissingRegion {
		err = fmt.Errorf("location for %q backend must use wrong format", p.backend.Name)
	}

	if err == nil && p.computeClient == nil {
		// retrieve variables used to authenticate from the environment
		authOpts, err := openstack.AuthOptionsFromEnv()
		if err != nil {
			return &FatalError{fmt.Errorf("cannot auto from env: %v", err)}
		}

		// Connect to keystone module to authenticate the client
		var provider *gophercloud.ProviderClient
		err = gophercloud.WaitFor(120, func() (bool, error) {
			provider, err = openstack.AuthenticatedClient(authOpts)
			if err == nil {
				return true, nil
			}
			return false, nil
		})
		if err != nil {
			return &FatalError{fmt.Errorf("cannot authenticate client: %v", err)}
		}

		// Create openstack compute client
		computeClient, err := openstack.NewComputeV2(provider, gophercloud.EndpointOpts{
			Region: p.openstackAvailabilityZone,
		})
		// Create openstack network client
		networkClient, err := openstack.NewNetworkV2(provider, gophercloud.EndpointOpts{
			Region: p.openstackAvailabilityZone,
		})
		// Create openstack image client
		imageClient, err := openstack.NewImageServiceV2(provider, gophercloud.EndpointOpts{
			Region: p.openstackAvailabilityZone,
		})

		p.computeClient = computeClient
		p.networkClient = networkClient
		p.imageClient = imageClient
		p.keyErr = err
	}

	p.keyChecked = true
	p.keyErr = err
	return err
}
