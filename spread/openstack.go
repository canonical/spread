package spread

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	gooseclient "github.com/go-goose/goose/v5/client"
	goosehttp "github.com/go-goose/goose/v5/http"

	"github.com/go-goose/goose/v5/glance"
	"github.com/go-goose/goose/v5/identity"
	"github.com/go-goose/goose/v5/neutron"
	"github.com/go-goose/goose/v5/nova"

	"github.com/joho/godotenv"
	"golang.org/x/crypto/ssh"
	"golang.org/x/net/context"
)

func Openstack(p *Project, b *Backend, o *Options) Provider {
	return &openstackProvider{
		project: p,
		backend: b,
		options: o,
	}
}

type glanceImageClient interface {
	ListImagesDetail() ([]glance.ImageDetail, error)
}

type novaComputeClient interface {
	ListFlavors() ([]nova.Entity, error)
	ListAvailabilityZones() ([]nova.AvailabilityZone, error)
	GetServer(serverId string) (*nova.ServerDetail, error)
	ListServersDetail(filter *nova.Filter) ([]nova.ServerDetail, error)
	RunServer(opts nova.RunServerOpts) (*nova.Entity, error)
	DeleteServer(serverId string) error
}

type openstackProvider struct {
	project *Project
	backend *Backend
	options *Options

	region        string
	osClient      gooseclient.Client
	computeClient novaComputeClient
	networkClient *neutron.Client
	imageClient   glanceImageClient

	mu sync.Mutex

	keyChecked bool
	keyErr     error
}

type openstackServer struct {
	p *openstackProvider
	d openstackServerData

	system  *System
	address string
}

type openstackServerData struct {
	Id       string
	Name     string
	Flavor   string    `json:"machineType"`
	Networks []string  `json:"network"`
	Status   string    `yaml:"-"`
	Created  time.Time `json:"creationTimestamp"`

	Labels map[string]string `yaml:"-"`
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
  - sed -i 's/^PermitRootLogin=/#PermitRootLogin=/g' /etc/ssh/sshd_config.d/* || true
  - sed -i 's/^PasswordAuthentication=/#PasswordAuthentication=/g' /etc/ssh/sshd_config.d/* || true
  - test -d /etc/ssh/sshd_config.d && echo -e 'PermitRootLogin=yes\nPasswordAuthentication=yes' > /etc/ssh/sshd_config.d/00-spread.conf
  - pkill -o -HUP sshd || true
  - echo '` + openstackReadyMarker + `' > /dev/ttyS0
`

const openstackReadyMarker = "MACHINE-IS-READY"
const openstackNameLayout = "Jan021504.000000"
const openstackDefaultFlavor = "m1.medium"

var timeNow = time.Now

func openstackName() string {
	return strings.ToLower(strings.Replace(timeNow().UTC().Format(openstackNameLayout), ".", "-", 1))
}

type openstackError struct {
	gooseError error
}

// Error messages triggered by modules are string like:
// line 1...n is a high level description about the error like:
// caused by: <highlevel_cause>
// line n+1 contains the error cause and the error info following the formats:
// caused by: <error_cause> ; error info: <json_data>
// caused by: <error_cause> ; error info: <html_data>
//
// This code will find the *last* "caused by:" line as it generally tells
// the most details.
// TODO: check if the above idea works well in practise, otherwise switch
// to e.g. firstLine
func (e *openstackError) cause(errMsg string) string {
	debugf("Full openstack error message: %v", errMsg)

	causedBy := strings.Split(errMsg, "caused by:")
	if len(causedBy) <= 1 {
		return errMsg
	}
	lastCausedBy := causedBy[len(causedBy)-1]
	firstErrLine := strings.SplitN(lastCausedBy, "\n", 2)[0]
	stripErrPart := strings.SplitN(firstErrLine, "; error info:", 2)[0]
	return strings.TrimSpace(stripErrPart)
}

func (e *openstackError) Error() string {
	msg := e.gooseError.Error()

	causedByErrorMsg := e.cause(msg)
	if causedByErrorMsg != "" {
		return causedByErrorMsg
	}

	return msg
}

func (p *openstackProvider) findFlavor(flavorName string) (*nova.Entity, error) {
	flavors, err := p.computeClient.ListFlavors()
	if err != nil {
		return nil, fmt.Errorf("cannot retrieve flavors list: %v", &openstackError{err})
	}

	for _, f := range flavors {
		if f.Name == flavorName {
			return &f, nil
		}
	}

	return nil, &FatalError{fmt.Errorf("cannot find valid flavor with name %q", flavorName)}
}

func (p *openstackProvider) findFirstNetwork() (*neutron.NetworkV2, error) {
	networks, err := p.networkClient.ListNetworksV2()
	if err != nil {
		return nil, fmt.Errorf("cannot retrieve networks list: %v", &openstackError{err})
	}
	// When there are not networks defined, the first network which is not external
	// is returned (external networks could not be allowed to request)
	for _, net := range networks {
		if !net.External {
			return &net, nil
		}
	}
	return nil, &FatalError{errors.New("cannot find valid network to create floating IP")}
}

func (p *openstackProvider) findNetwork(name string) (*neutron.NetworkV2, error) {
	networks, err := p.networkClient.ListNetworksV2()
	if err != nil {
		return nil, fmt.Errorf("cannot retrieve networks list: %v", &openstackError{err})
	}

	for _, net := range networks {
		if name != "" && name == net.Name {
			return &net, nil
		}
	}
	return nil, &FatalError{fmt.Errorf("cannot find valid network with name %q", name)}
}

func (p *openstackProvider) findImage(imageName string) (*glance.ImageDetail, error) {
	var lastImage glance.ImageDetail
	var lastCreatedDate time.Time

	// TODO: consider using an image cache just like the google backend
	// (https://github.com/snapcore/spread/pull/175 needs to be fixed first)
	images, err := p.imageClient.ListImagesDetail()
	if err != nil {
		return nil, fmt.Errorf("cannot retrieve images list: %v", &openstackError{err})
	}

	// In openstack there are no "project" specific images and no
	// concept of a "family" so the lookup is similar but a bit
	// simpler than in the google backend. Google checks for a)
	// exact match, b) family match, c) term match, only (a), (c)
	// are done here as there is no (b) in openstack. If multiple
	// term matches are found the newest image is selected.
	searchTerms := toTerms(imageName)
	// First consider exact matches on name.
	for _, image := range images {
		// First consider exact matches on name.
		if image.Name == imageName {
			// return the image when it matchs exactly with the provided name
			debugf("Name match: %#v matches %#v", imageName, image)
			return &image, nil
		}

		// Otherwise use term matching.
		imageTerms := toTerms(image.Name)
		if containsTerms(imageTerms, searchTerms) {
			debugf("Terms match: %#v matches %#v", imageName, image)
			// Check if the creation date for the current image is after the previous selected one
			currCreatedDate, err := time.Parse(time.RFC3339, image.Created)
			// When the creation date is not set or it cannot be parsed, it is considered as created just now
			if err != nil {
				currCreatedDate = time.Time{}
			}

			// Save the image when either it is the first match or it is newer than the previous match
			if lastImage.Id == "" || currCreatedDate.After(lastCreatedDate) {
				debugf("Newer terms match found: %#v", image)
				lastImage = image
				lastCreatedDate = currCreatedDate
			}
		}
	}

	if lastImage.Id != "" {
		return &lastImage, nil
	}

	return nil, &FatalError{fmt.Errorf("cannot find matching image for %q", imageName)}
}

func (p *openstackProvider) findAvailabilityZone() (*nova.AvailabilityZone, error) {
	zones, err := p.computeClient.ListAvailabilityZones()
	if err != nil {
		return nil, fmt.Errorf("cannot retrieve availability zones: %v", &openstackError{err})
	}

	if len(zones) == 0 {
		return nil, &FatalError{errors.New("cannot find any availability zones")}
	}
	return &zones[0], nil
}

func (p *openstackProvider) findSecurityGroupNames(names []string) ([]nova.SecurityGroupName, error) {
	var secGroupNames []nova.SecurityGroupName

	secGroups, err := p.networkClient.ListSecurityGroupsV2()
	if err != nil {
		return nil, fmt.Errorf("cannot retrieve security groups: %v", &openstackError{err})
	}

	if len(secGroups) == 0 {
		return nil, &FatalError{errors.New("cannot find any groups")}
	}

	for _, name := range names {
		found := false
		for _, sg := range secGroups {
			if name == sg.Name {
				found = true
				break
			}
		}
		if !found {
			return nil, &FatalError{fmt.Errorf("cannot find valid group with name %q", name)}
		}
		secGroupNames = append(secGroupNames, nova.SecurityGroupName{Name: name})
	}
	return secGroupNames, nil
}

var openstackProvisionTimeout = 3 * time.Minute
var openstackProvisionRetry = 5 * time.Second

func (p *openstackProvider) waitProvision(ctx context.Context, s *openstackServer) error {
	debugf("Waiting for %s to provision...", s)

	timeout := time.After(openstackProvisionTimeout)
	retry := time.NewTicker(openstackProvisionRetry)
	defer retry.Stop()

	for {
		select {
		case <-timeout:
			return fmt.Errorf("timeout waiting for %s to provision", s)

		case <-retry.C:
			server, err := p.computeClient.GetServer(s.d.Id)
			if err != nil {
				debugf("cannot get server info: %v", &openstackError{err})
				continue
			}
			if server.Status != nova.StatusBuild {
				if server.Status != nova.StatusActive {
					return fmt.Errorf("cannot use server: status is not active but %s", server.Status)
				}
				return nil
			}
		case <-ctx.Done():
			return fmt.Errorf("cannot wait for %s to provision: interrupted", s)
		}
	}
	panic("unreachable")
}

var openstackServerBootTimeout = 2 * time.Minute
var openstackServerBootRetry = 5 * time.Second

func (p *openstackProvider) waitServerBootSSH(ctx context.Context, s *openstackServer) error {
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
	timeout := time.After(openstackServerBootTimeout)
	retry := time.NewTicker(openstackServerBootRetry)
	defer retry.Stop()

	for {
		select {
		case <-timeout:
			return fmt.Errorf("cannot ssh to the allocated instance: timeout reached")
		case <-retry.C:
			_, err := sshDial("tcp", addr, config)
			if err == nil {
				debugf("Connection to server %s established", s.d.Name)
				return nil
			}
		case <-ctx.Done():
			return fmt.Errorf("cannot wait for %s to boot: interrupted", s)
		}
	}
}

var openstackSerialOutputTimeout = 30 * time.Second

func (p *openstackProvider) getSerialConsoleOutput(s *openstackServer) (string, error) {
	url := fmt.Sprintf("servers/%s/action", s.d.Id)

	var req struct {
		OsGetSerialConsole struct{} `json:"os-getConsoleOutput"`
	}
	var resp struct {
		Output string `json:"output"`
	}
	requestData := goosehttp.RequestData{ReqValue: req, RespValue: &resp, ExpectedStatus: []int{http.StatusOK}}
	timeout := time.After(openstackSerialOutputTimeout)
	retry := time.NewTicker(openstackServerBootRetry)
	defer retry.Stop()

	for {
		err := p.osClient.SendRequest("POST", "compute", "v2", url, &requestData)
		if err != nil {
			debugf("failed to retrieve the serial console for server %s: %v", s, err)
		}
		if len(resp.Output) > 0 {
			return resp.Output, nil
		}
		select {
		case <-retry.C:
		case <-timeout:
			return "", fmt.Errorf("failed to retrieve the serial console for server %s: timeout reached", s)
		}
	}
}

var openstackSerialConsoleErr = fmt.Errorf("cannot get console output")

func (p *openstackProvider) waitServerBootSerial(ctx context.Context, s *openstackServer) error {
	timeout := time.After(openstackServerBootTimeout)
	relog := time.NewTicker(60 * time.Second)
	defer relog.Stop()
	retry := time.NewTicker(openstackServerBootRetry)
	defer retry.Stop()

	var marker = openstackReadyMarker
	for {
		resp, err := p.getSerialConsoleOutput(s)
		if err != nil {
			return fmt.Errorf("%w: %v", openstackSerialConsoleErr, err)
		}

		if strings.Contains(resp, marker) {
			return nil
		}

		select {
		case <-retry.C:
			debugf("Server %s is taking a while to boot...", s)
		case <-relog.C:
			printf("Server %s is taking a while to boot...", s)
		case <-timeout:
			return fmt.Errorf("cannot find ready marker in console output for %s: timeout reached", s)
		case <-ctx.Done():
			return fmt.Errorf("cannot wait for %s to boot: interrupted", s)
		}
	}
	panic("unreachable")
}

func (p *openstackProvider) waitServerBoot(ctx context.Context, s *openstackServer) error {
	printf("Waiting for %s to boot at %s...", s, s.address)
	err := p.waitServerBootSerial(ctx, s)
	if err != nil {
		if !errors.Is(err, openstackSerialConsoleErr) {
			return err
		}
		// It is important to try ssh connection because serial console could
		// be disabled in the nova configuration
		err = p.waitServerBootSSH(ctx, s)
		if err != nil {
			return fmt.Errorf("cannot connect to server %s: %v", s, err)
		}
	}
	return nil
}

func (p *openstackProvider) address(ctx context.Context, s *openstackServer) (string, error) {
	timeout := time.After(30 * time.Second)
	retry := time.NewTicker(5 * time.Second)
	defer retry.Stop()

	for {
		server, err := p.computeClient.GetServer(s.d.Id)
		if err != nil {
			return "", fmt.Errorf("cannot get IP address for Openstack server %s: %v", s, &openstackError{err})
		}
		// The addresses for a network is map of networks to a list of ip adresses
		// We are configuring just 1 network address for the network
		// To get the address is used the first network

		if len(server.Addresses) > 0 {
			if len(s.d.Networks) > 0 {
				return server.Addresses[s.d.Networks[0]][0].Address, nil
			}
			// When no network selected, the we use the first ip in addresses
			for _, net_val := range server.Addresses {
				if len(net_val[0].Address) > 0 {
					return net_val[0].Address, nil
				}
			}
		}

		select {
		case <-retry.C:
			debugf("Server %s is taking a while to get IP address...", s)
		case <-timeout:
			return "", fmt.Errorf("timeout waiting for Openstack server %s IP address", s)
		case <-ctx.Done():
			return "", fmt.Errorf("cannot wait for %s IP address: interrupted", s)
		}
	}
}

func (p *openstackProvider) createMachine(ctx context.Context, system *System) (*openstackServer, error) {
	debugf("Creating new openstack server for %s...", system.Name)

	name := openstackName()
	flavorName := openstackDefaultFlavor
	if system.Plan != "" {
		flavorName = system.Plan
	}
	flavor, err := p.findFlavor(flavorName)
	if err != nil {
		return nil, err
	}

	networks := []nova.ServerNetworks{}
	if len(system.Networks) == 0 {
		net, err := p.findFirstNetwork()
		if err != nil {
			return nil, err
		}
		networks = []nova.ServerNetworks{{NetworkId: net.Id}}
	}

	for _, netName := range system.Networks {
		net, err := p.findNetwork(netName)
		if err != nil {
			return nil, err
		}
		networks = append(networks, nova.ServerNetworks{NetworkId: net.Id})
	}

	image, err := p.findImage(system.Image)
	if err != nil {
		return nil, err
	}

	availabilityZone, err := p.findAvailabilityZone()
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
		FlavorId:         flavor.Id,
		ImageId:          image.Id,
		AvailabilityZone: availabilityZone.Name,
		Networks:         networks,
		Metadata:         tags,
		UserData:         []byte(cloudconfig),
	}

	if len(system.Groups) > 0 {
		sgNames, err := p.findSecurityGroupNames(system.Groups)
		if err != nil {
			return nil, err
		}
		opts.SecurityGroupNames = sgNames
	}

	server, err := p.computeClient.RunServer(opts)
	if err != nil {
		return nil, fmt.Errorf("cannot create instance: %v", &openstackError{err})
	}

	s := &openstackServer{
		p: p,
		d: openstackServerData{
			Id:       server.Id,
			Name:     name,
			Flavor:   flavor.Name,
			Networks: system.Networks,
			Status:   openstackProvisioning,
			Created:  time.Now(),
		},

		system: system,
	}

	err = p.waitProvision(ctx, s)
	if err == nil {
		s.address, err = p.address(ctx, s)
	}
	if err == nil {
		err = p.waitServerBoot(ctx, s)
	}
	if err != nil {
		if p.removeMachine(ctx, s) != nil {
			return nil, &FatalError{fmt.Errorf("cannot allocate or deallocate (!) new Openstack server %s: %v", s, err)}
		}
		return nil, &FatalError{fmt.Errorf("cannot allocate new Openstack server %s: %v", s, err)}
	}

	return s, nil
}

func (p *openstackProvider) list() ([]*openstackServer, error) {
	debug("Listing available openstack instances...")

	filter := nova.NewFilter()
	servers, err := p.computeClient.ListServersDetail(filter)

	if err != nil {
		return nil, fmt.Errorf("cannot list openstack instances: %v", &openstackError{err})
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
				Id:      s.Id,
				Name:    s.Name,
				Created: createdTime,
				Labels:  s.Metadata,
			}
			instances = append(instances, &openstackServer{p: p, d: d})
		}
	}
	return instances, nil
}

func (p *openstackProvider) removeMachine(ctx context.Context, s *openstackServer) error {
	err := p.computeClient.DeleteServer(s.d.Id)
	if err != nil {
		return fmt.Errorf("cannot remove openstack instance: %v", &openstackError{err})
	}
	return err
}

func (p *openstackProvider) GarbageCollect() error {
	if err := p.checkKey(); err != nil {
		return err
	}

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

		printf("Checking openstack instance %s...", s)

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

func (p *openstackProvider) checkKey() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.keyChecked {
		return p.keyErr
	}

	var err error

	if err == nil && p.computeClient == nil {

		// Load environment variables used to authenticate
		if p.backend.Key != "" {
			godotenv.Load(p.backend.Key)
		}

		// retrieve variables used to authenticate from the environment
		cred, err := identity.CompleteCredentialsFromEnv()
		if err != nil {
			return &FatalError{fmt.Errorf("cannot retrieve credentials from env: %v", err)}
		}

		// Select the appropiate authentication method
		var authmode identity.AuthMode
		if os.Getenv("OS_ACCESS_KEY") != "" && os.Getenv("OS_SECRET_KEY") != "" {
			authmode = identity.AuthKeyPair
		} else if os.Getenv("OS_USERNAME") != "" && os.Getenv("OS_PASSWORD") != "" {
			authmode = identity.AuthUserPassV3
			if cred.Version > 0 && cred.Version != 3 {
				authmode = identity.AuthUserPass
			}
		} else {
			return &FatalError{fmt.Errorf("cannot determine authentication method to use")}
		}

		authClient := gooseclient.NewClient(cred, authmode, nil)
		err = authClient.Authenticate()
		if err != nil {
			return &FatalError{fmt.Errorf("cannot authenticate: %v", &openstackError{err})}
		}

		// Create clients for the used modules
		p.region = cred.Region
		p.osClient = authClient
		p.computeClient = nova.New(authClient)
		p.networkClient = neutron.New(authClient)
		p.imageClient = glance.New(authClient)
		p.keyErr = err
	}

	p.keyChecked = true
	p.keyErr = err
	return err
}
