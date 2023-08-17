package spread

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	gooseClient "github.com/go-goose/goose/v5/client"
	"github.com/go-goose/goose/v5/glance"
	"github.com/go-goose/goose/v5/identity"
	"github.com/go-goose/goose/v5/neutron"
	"github.com/go-goose/goose/v5/nova"

	"golang.org/x/crypto/ssh"
	"golang.org/x/net/context"
	"golang.org/x/net/html"
)

func Openstack(p *Project, b *Backend, o *Options) Provider {
	return &openstackProvider{
		project: p,
		backend: b,
		options: o,

		imagesCache: make(map[string]*openstackImagesCache),
	}
}

type glanceImageClient interface {
	ListImagesDetail() ([]glance.ImageDetail, error)
}

type openstackProvider struct {
	project *Project
	backend *Backend
	options *Options

	openstackProject          string
	openstackAvailabilityZone string

	region        string
	computeClient *nova.Client
	networkClient *neutron.Client
	imageClient   glanceImageClient

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
	Id       string
	Name     string
	Flavor   string    `json:"machineType"`
	Networks []string  `json:"network"`
	Status   string    `yaml:"-"`
	Created  time.Time `json:"creationTimestamp"`

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

var timeNow = time.Now

func openstackName() string {
	return strings.ToLower(strings.Replace(timeNow().UTC().Format(openstackNameLayout), ".", "-", 1))
}

func findHTMLErrorMsg(node *html.Node) string {
	if node.Type == html.ElementNode && node.Data == "title" {
		return node.FirstChild.Data
	}

	for child := node.FirstChild; child != nil; child = child.NextSibling {
		title := findHTMLErrorMsg(child)
		if title != "" {
			return strings.TrimSpace(title)
		}
	}

	return ""
}

// error messages returned by openstack api are html
// which contain title and details related to the error
func getHTMLErrorMsg(errMsg string) string {
	node, err := html.Parse(strings.NewReader(errMsg))
	if err != nil {
		return ""
	}

	return findHTMLErrorMsg(node)
}

// some error error messages triggered by nova are string like:
// line1...n is a low level description about the error
// line n+1 contains the error cause and the error info following the format
// caused by: <error_cause> ; error info: <json_data>
func getCausedByErrorMsg(errMsg string) string {
	for _, line := range strings.Split(strings.TrimSuffix(errMsg, "\n"), "\n") {
		if strings.HasPrefix(line, "caused by:") {
			errorCause := strings.Split(line, "; error info:")[0]
			return strings.TrimSpace(strings.TrimPrefix(errorCause, "caused by:"))
		}
	}
	return ""
}

func errorMsg(osErr error) string {
	msg := osErr.Error()

	htmlErrorMsg := getHTMLErrorMsg(msg)
	if htmlErrorMsg != "" {
		return htmlErrorMsg
	}

	causedByErrorMsg := getCausedByErrorMsg(msg)
	if causedByErrorMsg != "" {
		return causedByErrorMsg
	}

	return msg
}

func (p *openstackProvider) findFlavor(flavorName string) (*nova.Entity, error) {
	flavors, err := p.computeClient.ListFlavors()
	if err != nil {
		return nil, fmt.Errorf("cannot retrieve flavors list: %s", errorMsg(err))
	}

	for _, f := range flavors {
		if f.Name == flavorName {
			return &f, nil
		}
	}

	return nil, &FatalError{fmt.Errorf("cannot find valid flavor with name %s", flavorName)}
}

func (p *openstackProvider) findFirstNetwork() (*neutron.NetworkV2, error) {
	networks, err := p.networkClient.ListNetworksV2()
	if err != nil {
		return nil, fmt.Errorf("cannot retrieve networks list: %s", errorMsg(err))
	}
	// When there are not networks defined, the first network which is not external
	// is returned (external networks could not be allowed to request)
	for _, net := range networks {
		if net.External == false {
			return &net, nil
		}
	}
	return nil, &FatalError{errors.New("cannot find valid network to create floating IP")}
}

func (p *openstackProvider) findNetwork(name string) (*neutron.NetworkV2, error) {
	networks, err := p.networkClient.ListNetworksV2()
	if err != nil {
		return nil, fmt.Errorf("cannot retrieve networks list: %s", errorMsg(err))
	}

	for _, net := range networks {
		if name != "" && name == net.Name {
			return &net, nil
		}
	}
	return nil, &FatalError{fmt.Errorf("cannot find valid network with name %s", name)}
}

func (p *openstackProvider) findImage(imageName string) (*glance.ImageDetail, error) {
	var sameImage glance.ImageDetail
	var lastImage glance.ImageDetail
	var lastCreatedDate time.Time

	images, err := p.imageClient.ListImagesDetail()
	if err != nil {
		return nil, fmt.Errorf("cannot retrieve images list: %s", errorMsg(err))
	}

	for _, i := range images {
		if i.Name == imageName {
			sameImage = i
		} else if strings.Contains(i.Name, imageName) {
			// Check if the creation date for the current image is after the previous selected one
			currCreatedDate, err := time.Parse(time.RFC3339, i.Created)
			// When the creation date is not set or it cannot be parsed, it is considered as created just now
			if err != nil {
				currCreatedDate = time.Time{}
			}

			// Save the image when either it is the first match or it is newer than the previous match
			if lastImage.Id == "" || currCreatedDate.After(lastCreatedDate) {
				lastImage = i
				lastCreatedDate = currCreatedDate
			}
		}
	}

	// return the image when it matchs exactly with the provided name
	if sameImage.Id != "" {
		return &sameImage, nil
	}

	if lastImage.Id != "" {
		return &lastImage, nil
	}

	return nil, &FatalError{fmt.Errorf("cannot find matching image for %s", imageName)}
}

func (p *openstackProvider) findAvailabilityZone() (*nova.AvailabilityZone, error) {
	zones, err := p.computeClient.ListAvailabilityZones()
	if err != nil {
		return nil, fmt.Errorf("cannot retrieve availability zones: %s", errorMsg(err))
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
		return nil, fmt.Errorf("cannot retrieve security groups: %s", errorMsg(err))
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
			return nil, &FatalError{fmt.Errorf("cannot find valid group with name %s", name)}
		}
		secGroupNames = append(secGroupNames, nova.SecurityGroupName{Name: name})
	}
	return secGroupNames, nil
}

func (p *openstackProvider) waitServerCompleteBuilding(s *openstackServer, timeoutSeconds int) error {
	// Wait until the server is actually running
	start := time.Now()
	for {
		if time.Since(start) > time.Duration(timeoutSeconds)*time.Second {
			return &FatalError{fmt.Errorf("cannot check status: timeout reached")}
		}
		server, err := p.computeClient.GetServer(s.d.Id)
		// When the server info cannot be retrieved, wait 2 seconds to retry
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		if server.Status != nova.StatusBuild {
			if server.Status != nova.StatusActive {
				return &FatalError{fmt.Errorf("cannot use server: status is not active but %s", server.Status)}
			}
			break
		}
		time.Sleep(5 * time.Second)
	}
	debugf("Server %s is running", s.d.Name)
	return nil
}

func (p *openstackProvider) waitServerCompleteSetup(s *openstackServer, timeoutSeconds int) error {
	server, err := p.computeClient.GetServer(s.d.Id)
	if err != nil {
		return fmt.Errorf("cannot retrieving server information: %s", errorMsg(err))
	}
	// The adreesses for a network is map of networks and list of ip adresses
	// We are configuring just 1 network address for the network
	// To get the address is used the first network
	s.address = server.Addresses[s.d.Networks[0]][0].Address

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
		if time.Since(start) > time.Duration(timeoutSeconds)*time.Second {
			return &FatalError{fmt.Errorf("cannot ssh to the allocated instance")}
		}

		_, err = ssh.Dial("tcp", addr, config)
		if err == nil {
			break
		}

		time.Sleep(2 * time.Second)
	}
	debugf("Connection to server %s established", s.d.Name)
	return nil
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
		errorMsg(err)
		return nil, fmt.Errorf("cannot create instance %s", errorMsg(err))
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

	// First we need to wait until the image is active and there is no erros during the spawning process
	// The timeout for this process is 180 seconds
	err = p.waitServerCompleteBuilding(s, 120)
	if err != nil {
		if p.removeMachine(ctx, s) != nil {
			return nil, &FatalError{fmt.Errorf("cannot allocate or deallocate (!) new openstack server %s: %v", s, err)}
		}
		return nil, &FatalError{fmt.Errorf("cannot allocate new openstack server %s: %v", s, err)}
	}

	// Connect through ssh to the
	err = p.waitServerCompleteSetup(s, 240)
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
		return nil, fmt.Errorf("cannot list openstack instances: %s", errorMsg(err))
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
			}
			instances = append(instances, &openstackServer{p: p, d: d})
		}
	}
	return instances, nil
}

func (p *openstackProvider) removeMachine(ctx context.Context, s *openstackServer) error {
	err := p.computeClient.DeleteServer(s.d.Id)
	if err != nil {
		return fmt.Errorf("cannot remove openstack instance: %s", errorMsg(err))
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

		// Select the appropiate version of the UserPass authentication method
		var authmode = identity.AuthUserPassV3
		if cred.Version > 0 && cred.Version != 3 {
			authmode = identity.AuthUserPass
		}

		authClient := gooseClient.NewClient(cred, authmode, nil)
		err = authClient.Authenticate()
		if err != nil {
			return &FatalError{fmt.Errorf("cannot authenticate: %s", errorMsg(err))}
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
