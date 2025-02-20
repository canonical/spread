package spread

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	gooseclient "github.com/go-goose/goose/v5/client"
	goosehttp "github.com/go-goose/goose/v5/http"

	"github.com/go-goose/goose/v5/cinder"
	"github.com/go-goose/goose/v5/glance"
	"github.com/go-goose/goose/v5/identity"
	"github.com/go-goose/goose/v5/neutron"
	"github.com/go-goose/goose/v5/nova"

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
	ListVolumeAttachments(serverId string) ([]nova.VolumeAttachment, error)
	RunServer(opts nova.RunServerOpts) (*nova.Entity, error)
	DeleteServer(serverId string) error
}

type openstackServices struct {
	compute OpenstackService
	volume  OpenstackService
}

type OpenstackService struct {
	Name     string
	version  string
	endpoint *url.URL
}

type openstackProvider struct {
	project *Project
	backend *Backend
	options *Options

	region        string
	osClient      gooseclient.AuthenticatingClient
	computeClient novaComputeClient
	networkClient *neutron.Client
	imageClient   glanceImageClient
	volumeClient  *cinder.Client

	services *openstackServices

	mu sync.Mutex

	authComplete bool
	authErr      error
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

type openstackVolumeData struct {
	Id          string             `json:"id"`
	Status      string             `json:"status"`
	SnapshotId  string             `json:"snapshot_id,omitempty"`
	Attachments []VolumeAttachment `json:"attachments,omitempty"`
	Name        string             `json:"name,omitempty"`
}

type openstackSnapshotData struct {
	Id       string `json:"id"`
	Name     string `json:"name"`
	Size     int    `json:"size"`
	Status   string `json:"status"`
	VolumeID string `json:"volume_id,omitempty"`
}

type VolumeAttachment struct {
	Id       string `json:"id"`
	ServerId string `json:"server_id"`
	VolumeId string `json:"volume_id"`
}

type VolumeActionResetStatus struct {
	Action VolumeActionResetStatusDetails `json:"os-reset_status"`
}

type VolumeActionResetStatusDetails struct {
	Status string `json:"status"`
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

var openstackSerialOutputTimeout = 30 * time.Second
var openstackSerialConsoleErr = fmt.Errorf("cannot get console output")

func (s *openstackServer) SerialOutput() (string, error) {
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
		err := s.p.osClient.SendRequest("POST", s.p.services.compute.Name, s.p.services.compute.version, url, &requestData)
		if err != nil {
			debugf("failed to retrieve the serial console for server %s: %v", s, err)
		}
		if len(resp.Output) > 0 {
			return resp.Output, nil
		}
		select {
		case <-retry.C:
		case <-timeout:
			return "", fmt.Errorf("failed to retrieve the serial console for instance %s: timeout reached", s)
		}
	}
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
	if err := p.checkCredentials(); err != nil {
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
  - test -d /etc/ssh/sshd_config.d && echo 'PermitRootLogin=yes' > /etc/ssh/sshd_config.d/00-spread.conf
  - test -d /etc/ssh/sshd_config.d && echo 'PasswordAuthentication=yes' >> /etc/ssh/sshd_config.d/00-spread.conf
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
	return strings.TrimSpace(firstErrLine)
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
				debugf("cannot get instance info: %v", &openstackError{err})
				continue
			}
			if server.Status != nova.StatusBuild {
				if server.Status != nova.StatusActive {
					return fmt.Errorf("server status is %s", server.Status)
				}
				return nil
			}
		case <-ctx.Done():
			return fmt.Errorf("cannot wait for %s to provision: interrupted", s)
		}
	}
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

	// Iterate until the ssh connection to the host can be established
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

func (p *openstackProvider) waitServerBootSerial(ctx context.Context, s *openstackServer) error {
	timeout := time.After(openstackServerBootTimeout)
	relog := time.NewTicker(60 * time.Second)
	defer relog.Stop()
	retry := time.NewTicker(openstackServerBootRetry)
	defer retry.Stop()

	var marker = openstackReadyMarker
	for {
		resp, err := s.SerialOutput()
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
			return fmt.Errorf("cannot connect to instance %s: %v", s, err)
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
			return "", fmt.Errorf("cannot get IP address for Openstack instance %s: %v", s, &openstackError{err})
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
			return "", fmt.Errorf("timeout waiting for Openstack instance %s IP address", s)
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
		ImageId:          image.Id,
		FlavorId:         flavor.Id,
		AvailabilityZone: availabilityZone.Name,
		Networks:         networks,
		Metadata:         tags,
		UserData:         []byte(cloudconfig),
	}

	// When the storage size is defined, then we use the volume generated
	// with the source image. Otherwise we boot in an ephemeral disk the
	// image using the size described in the flavor
	storage := image.MinimumDisk
	if system.Storage != 0 {
		storage = int(system.Storage / gb)
	}
	// We use 20 GB as default value for the storage size
	if storage == 0 {
		storage = 20
	}

	opts.BlockDeviceMappings = []nova.BlockDeviceMapping{{
		BootIndex:       0,
		SourceType:      "image",
		DestinationType: "volume",
		VolumeSize:      20,
		UUID:            image.Id,
	}}

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
			return nil, &FatalError{fmt.Errorf("cannot allocate or deallocate (!) new Openstack instance %s: %v", s.d.Name, err)}
		}
		return nil, &FatalError{fmt.Errorf("cannot allocate new Openstack instance %s: %v", s.d.Name, err)}
	}

	return s, nil
}

func (p *openstackProvider) listServers() ([]*openstackServer, error) {
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

func (p *openstackProvider) listVolumes() ([]*openstackVolumeData, error) {
	debug("Listing available openstack volumes...")

	volumeResults, err := p.volumeClient.GetVolumesDetail()
	if err != nil {
		return nil, fmt.Errorf("cannot list openstack volumes: %v", &openstackError{err})
	}

	var volumes []*openstackVolumeData
	for _, v := range volumeResults.Volumes {
		status := v.Status
		if status == "available" || strings.HasPrefix(status, "error") {
			attachments := []VolumeAttachment{}
			for _, a := range v.Attachments {
				att := VolumeAttachment{
					Id:       a.Id,
					ServerId: a.ServerId,
					VolumeId: a.VolumeId,
				}
				attachments = append(attachments, att)
			}
			d := openstackVolumeData{
				Id:          v.ID,
				Name:        v.Name,
				Status:      v.Status,
				Attachments: attachments,
			}
			volumes = append(volumes, &d)
		}
	}
	return volumes, nil
}

func (p *openstackProvider) listSnapshots() ([]*openstackSnapshotData, error) {
	debug("Listing available openstack snapshots...")

	snapshotResults, err := p.volumeClient.GetSnapshotsDetail()
	if err != nil {
		return nil, fmt.Errorf("cannot list openstack snapshots: %v", &openstackError{err})
	}

	var snapshots []*openstackSnapshotData
	for _, s := range snapshotResults.Snapshots {
		d := openstackSnapshotData{
			Id:       s.ID,
			Name:     s.Name,
			Status:   s.Status,
			Size:     s.Size,
			VolumeID: s.VolumeID,
		}
		snapshots = append(snapshots, &d)
	}
	return snapshots, nil
}

var openstackRemoveServerTimeout = 1 * time.Minute
var openstackRemoveServerRetry = 5 * time.Second

func (p *openstackProvider) removeMachine(ctx context.Context, s *openstackServer) error {
	volumeAttachments, err := p.computeClient.ListVolumeAttachments(s.d.Id)
	if err != nil {
		return fmt.Errorf("failed to retrieve the volumes attached to the instance: %v", err)
	}
	err = p.computeClient.DeleteServer(s.d.Id)
	if err != nil {
		return fmt.Errorf("cannot remove openstack instance: %v", &openstackError{err})
	}
	timeout := time.After(openstackRemoveServerTimeout)
	retry := time.NewTicker(openstackRemoveServerRetry)
	defer retry.Stop()

	for {
		_, err := p.computeClient.GetServer(s.d.Id)
		if err != nil {
			// this is when the server was already removed
			break
		}
		select {
		case <-retry.C:
		case <-timeout:
			printf("cannot remove the openstack server %s: timeout reached", s.d.Id)
			return nil
		}
	}

	for _, volumeAttachment := range volumeAttachments {
		volumeResults, err := p.volumeClient.GetVolume(volumeAttachment.VolumeId)
		if err != nil {
			// this is when the volume was already removed
			return nil
		}

		status := volumeResults.Volume.Status
		if status == "available" || strings.HasPrefix(status, "error") {
			// When the status is either available or error, the volume is deleted
			// otherwise, it will garbage collected after a time when it becomes available
			err := p.volumeClient.DeleteVolume(volumeAttachment.VolumeId)
			if err != nil {
				return fmt.Errorf("cannot remove openstack volume: %v", &openstackError{err})
			}
		}
	}
	return err
}

func (p *openstackProvider) GarbageCollect() error {
	if err := p.checkCredentials(); err != nil {
		return err
	}

	instances, err := p.listServers()
	if err != nil {
		return err
	}

	volumes, err := p.listVolumes()
	if err != nil {
		return err
	}
	snapshots, err := p.listSnapshots()
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
				printf("WARNING: Cannot garbage collect server %s: %v", s, err)
			}
		}
	}

	// Iterate over all the volumes
	for _, v := range volumes {
		printf("Checking openstack volume %s...", v.Id)

		snapshotAttached := ""
		for _, s := range snapshots {
			if s.VolumeID == v.Id {
				snapshotAttached = s.Id
				break
			}
		}

		if snapshotAttached == "" {
			printf("Volume ready to be deleted %s. Shutting it down...", v.Id)
			err := p.volumeClient.DeleteVolume(v.Id)
			if err != nil {
				printf("WARNING: Cannot garbage collect volume %s: %v", v.Id, err)
			}
		}
	}

	return nil
}

func (p *openstackProvider) checkCredentials() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.authComplete {
		return p.authErr
	}

	var err error
	if p.computeClient == nil {
		err = p.authenticate()
	}

	p.authComplete = true
	p.authErr = err
	return err
}

func (p *openstackProvider) authenticate() error {
	// Only identity API v3 is currently supported
	if !strings.HasSuffix(p.backend.Endpoint, "/v3") {
		return &FatalError{errors.New("identity API version not supported")}
	}
	identityAPIVersion := 3

	// The location entry contains project/region
	var region, proj string
	loc := strings.SplitN(p.backend.Location, "/", 2)
	if len(loc) == 2 {
		proj = loc[0]
		region = loc[1]
	}

	// Authenticate using the project credentials.
	creds := &identity.Credentials{
		URL:        p.backend.Endpoint, // The authentication URL
		User:       p.backend.Account,  // The username to authenticate as
		Secrets:    p.backend.Key,      // The authentication secret
		Region:     region,             // The OS region
		TenantName: proj,               // The OS project name
		Version:    identityAPIVersion, // The identity API version
	}
	authClient := gooseclient.NewClient(creds, identity.AuthUserPassV3, nil)
	if err := authClient.Authenticate(); err != nil {
		err = &FatalError{fmt.Errorf("cannot authenticate: %v", &openstackError{err})}
	}

	p.region = creds.Region
	p.osClient = authClient
	p.computeClient = nova.New(authClient)
	p.networkClient = neutron.New(authClient)
	p.imageClient = glance.New(authClient)

	if err := p.saveServices(); err != nil {
		return &FatalError{fmt.Errorf("failed to save services: %v", &openstackError{err})}
	}

	// Create cinder client
	handleRequest := cinder.SetAuthHeaderFn(p.osClient.Token, func(req *http.Request) (*http.Response, error) {
		return http.DefaultClient.Do(req)
	})
	p.volumeClient = cinder.NewClient(p.osClient.TenantId(), p.services.volume.endpoint, handleRequest)

	return nil
}

func (p *openstackProvider) saveServices() error {
	endpoints := p.osClient.EndpointsForRegion(p.region)
	p.services = &openstackServices{}

	for k, v := range endpoints {
		if strings.HasPrefix(k, "volume") || strings.HasPrefix(k, "compute") {
			ver := "v2"
			if strings.HasSuffix(k, "v3") {
				ver = "v3"
			}
			endpointUrl, err := url.Parse(v)
			if err != nil {
				return &FatalError{fmt.Errorf("error parsing endpoint: %v", &openstackError{err})}
			}

			service := OpenstackService{
				Name:     k,
				version:  ver,
				endpoint: endpointUrl,
			}
			if strings.HasPrefix(k, "compute") {
				p.services.compute = service
			} else {
				p.services.volume = service
			}
		}
	}
	if p.services.compute.Name == "" {
		return &FatalError{fmt.Errorf("compute services endpoint not found")}
	}
	if p.services.volume.Name == "" {
		return &FatalError{fmt.Errorf("volume services endpoint not found")}
	}
	return nil
}
