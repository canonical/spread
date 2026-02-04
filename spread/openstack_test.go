package spread_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/go-goose/goose/v5/glance"
	goosehttp "github.com/go-goose/goose/v5/http"
	"github.com/go-goose/goose/v5/neutron"
	"github.com/go-goose/goose/v5/nova"
	"golang.org/x/crypto/ssh"

	"github.com/snapcore/spread/spread"

	. "gopkg.in/check.v1"
)

func newOpenStack() spread.Provider {
	prj := &spread.Project{}
	b := &spread.Backend{}
	opts := &spread.Options{}

	return spread.OpenStack(prj, b, opts)
}

type openstackSuite struct{}

var _ = Suite(&openstackSuite{})

func (s *openstackSuite) TestTrivial(c *C) {
	o := newOpenStack()
	c.Check(o, NotNil)
}

func (s *openstackSuite) TestOpenStackName(c *C) {
	restore := spread.FakeTimeNow(func() time.Time {
		return time.Date(2007, 8, 22, 11, 59, 58, 987654321, time.UTC)
	})
	defer restore()

	name := spread.OpenStackName()
	c.Check(name, Equals, "aug221159-987654")
}

var opstErr1 = errors.New(`caused by: requesting token failed
caused by: Resource at https://keystone.bos01.canonistack.canonical.com:5000/v3/tokens not found
caused by: request (https://keystone.bos01.canonistack.canonical.com:5000/v3/tokens) returned unexpected status: 404; error info: <!DOCTYPE HTML PUBLIC "-//W3C//DTD HTML 3.2 Final//EN">
<title>404 Not Found</title>
<h1>Not Found</h1>
<p>The requested URL was not found on the server.  If you entered the URL manually please check your spelling and try again.</p>`)

var opstErr2 = errors.New(`caused by: requesting token failed
caused by: request (https://keystone.bos01.canonistack.canonical.com:5000/v3/tokens) returned unexpected status: 503; error info: <!DOCTYPE HTML PUBLIC "-//IETF//DTD HTML 2.0//EN">
<html><head>
<title>503 Service Unavailable</title>
</head><body>
<h1>Service Unavailable</h1>
<p>The server is temporarily unable to service your
request due to maintenance downtime or capacity
problems. Please try again later.</p>
<hr>
<address>Apache/2.4.29 (Ubuntu) Server at 10.48.7.10 Port 5000</address>
</body></html>`)

func (s *openstackSuite) TestOpenStackError(c *C) {
	err1 := spread.NewOpenStackError(opstErr1)
	c.Check(err1.Error(), Equals, `request (https://keystone.bos01.canonistack.canonical.com:5000/v3/tokens) returned unexpected status: 404`)

	err2 := spread.NewOpenStackError(opstErr2)
	c.Check(err2.Error(), Equals, `request (https://keystone.bos01.canonistack.canonical.com:5000/v3/tokens) returned unexpected status: 503`)

	err3 := spread.NewOpenStackError(errors.New("other error"))
	c.Check(err3.Error(), Equals, `other error`)
}

type fakeGlanceImageClient struct {
	res []glance.ImageDetail
	err error
}

func (ic *fakeGlanceImageClient) ListImagesDetail() ([]glance.ImageDetail, error) {
	return ic.res, ic.err
}

type fakeNovaComputeClient struct {
	listFlavors           func() ([]nova.Entity, error)
	listAvailabilityZones func() ([]nova.AvailabilityZone, error)
	getServer             func(serverId string) (*nova.ServerDetail, error)
	listServersDetail     func(filter *nova.Filter) ([]nova.ServerDetail, error)
	runServer             func(opts nova.RunServerOpts) (*nova.Entity, error)
	deleteServer          func(serverId string) error
}

func (cc *fakeNovaComputeClient) ListFlavors() ([]nova.Entity, error) {
	return cc.listFlavors()
}

func (cc *fakeNovaComputeClient) ListAvailabilityZones() ([]nova.AvailabilityZone, error) {
	return cc.listAvailabilityZones()
}

func (cc *fakeNovaComputeClient) GetServer(serverId string) (*nova.ServerDetail, error) {
	return cc.getServer(serverId)
}

func (cc *fakeNovaComputeClient) ListServersDetail(filter *nova.Filter) ([]nova.ServerDetail, error) {
	return cc.listServersDetail(filter)
}

func (cc *fakeNovaComputeClient) RunServer(opts nova.RunServerOpts) (*nova.Entity, error) {
	return cc.runServer(opts)
}

func (cc *fakeNovaComputeClient) DeleteServer(serverId string) error {
	return cc.deleteServer(serverId)
}

type fakeNeutronClient struct {
	listNetworksV2       func(filter ...*neutron.Filter) ([]neutron.NetworkV2, error)
	listSecurityGroupsV2 func() ([]neutron.SecurityGroupV2, error)
}

func (f *fakeNeutronClient) ListNetworksV2(filter ...*neutron.Filter) ([]neutron.NetworkV2, error) {
	if f.listNetworksV2 != nil {
		return f.listNetworksV2()
	}
	return nil, nil
}

func (f *fakeNeutronClient) ListSecurityGroupsV2() ([]neutron.SecurityGroupV2, error) {
	if f.listSecurityGroupsV2 != nil {
		return f.listSecurityGroupsV2()
	}
	return nil, nil
}

type fakeOsClientRequest struct {
	method      string
	svcType     string
	svcVersion  string
	url         string
	requestData *goosehttp.RequestData
}

type fakeOsClient struct {
	requests []fakeOsClientRequest
	response func() interface{}
	err      error
}

func (osc *fakeOsClient) SendRequest(method string, svcType string, svcVersion string, url string, requestData *goosehttp.RequestData) error {
	osc.requests = append(osc.requests, fakeOsClientRequest{
		method:      method,
		svcType:     svcType,
		svcVersion:  svcVersion,
		url:         url,
		requestData: requestData,
	})
	if osc.response != nil {
		rawResp, err := json.Marshal(osc.response())
		if err != nil {
			return err
		}
		if err := json.Unmarshal(rawResp, requestData.RespValue); err != nil {
			return err
		}
	}
	return osc.err
}

func (osc *fakeOsClient) MakeServiceURL(serviceType, apiVersion string, parts []string) (string, error) {
	return "", fmt.Errorf("not implemented")
}

type openstackFindImageSuite struct {
	opst spread.Provider

	fakeImageClient   *fakeGlanceImageClient
	fakeComputeClient *fakeNovaComputeClient
	fakeOsClient      *fakeOsClient
	fakeNeutronClient *fakeNeutronClient
}

var _ = Suite(&openstackFindImageSuite{})

func (s *openstackFindImageSuite) SetUpTest(c *C) {
	s.opst = newOpenStack()
	c.Assert(s.opst, NotNil)
	s.fakeImageClient = &fakeGlanceImageClient{}
	s.fakeComputeClient = &fakeNovaComputeClient{}
	s.fakeOsClient = &fakeOsClient{}
	s.fakeNeutronClient = &fakeNeutronClient{}

	spread.FakeOpenStackImageClient(s.opst, s.fakeImageClient)
	spread.FakeOpenStackComputeClient(s.opst, s.fakeComputeClient)
	spread.FakeOpenStackGooseClient(s.opst, s.fakeOsClient)
	spread.FakeOpenStackNeutronClient(s.opst, s.fakeNeutronClient)
}

func (s *openstackFindImageSuite) TestOpenStackFindImageNotFound(c *C) {
	s.fakeImageClient.res = []glance.ImageDetail{
		{Name: "unreleated"},
	}

	_, err := spread.OpenStackFindImage(s.opst, "ubuntu-22.04-64")
	c.Check(err, ErrorMatches, `cannot find matching image for "ubuntu-22.04-64"`)
}

func (s *openstackFindImageSuite) TestOpenStackFindImageErrors(c *C) {
	s.fakeImageClient.err = fmt.Errorf("boom")

	_, err := spread.OpenStackFindImage(s.opst, "ubuntu-22.04-64")
	c.Check(err, ErrorMatches, `cannot retrieve images list: boom`)
}

func uuid() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/uuid")
	if err != nil {
		panic(err)
	}
	return string(b)
}

var fakeImageDetailsDateRegex = regexp.MustCompile(`.*-([0-9]+)-.*`)

func makeGlanceImageDetails(imgs []string) []glance.ImageDetail {
	out := make([]glance.ImageDetail, len(imgs))
	for i, name := range imgs {
		var created string

		id := uuid()
		match := fakeImageDetailsDateRegex.FindStringSubmatch(name)
		if match != nil {
			if t, err := time.Parse("20060102", match[1]); err == nil {
				created = t.Format(time.RFC3339)
			}
		}
		out[i] = glance.ImageDetail{Id: id, Name: name, Created: created}
	}
	return out
}

var fakeOpenStackImageListMadeUp = []string{
	"ubuntu-22.04",
	"ubuntu-20.04",
	"fedora-38",
}

type openstackFindImageTest struct {
	imageName       string
	availableImages []string
	expectFind      bool
}

var openstackFindImageTests = []openstackFindImageTest{{
	imageName:       "ubuntu-14.04",
	availableImages: fakeOpenStackImageListMadeUp,
	expectFind:      false,
}, {
	imageName:       "ubuntu-22.04",
	availableImages: fakeOpenStackImageListMadeUp,
	expectFind:      true,
}}

func (s *openstackFindImageSuite) TestOpenStackFindImage(c *C) {
	for _, tc := range openstackFindImageTests {
		s.fakeImageClient.res = makeGlanceImageDetails(tc.availableImages)
		idt, err := spread.OpenStackFindImage(s.opst, tc.imageName)
		if tc.expectFind {
			c.Check(err, IsNil, Commentf("%s", tc))
			c.Check(idt.Name, Equals, tc.imageName)
		} else {

			c.Check(err, ErrorMatches, fmt.Sprintf(`cannot find matching image for %q`, tc.imageName), Commentf("%s", tc))
		}
	}
}

var fakeOpenStackImageList = []string{
	// from real "openstack image list" output but put here unordered
	"auto-sync/ubuntu-bionic-18.04-amd64-server-20210928-disk1.img",
	"auto-sync/ubuntu-bionic-18.04-amd64-server-20230530-disk1.img",
	"auto-sync/ubuntu-bionic-18.04-amd64-server-20210614-disk1.img",
	// made-up: contains search
	"auto-sync/ubuntu-22.04-something",
	// made-up
	"fedora-35",
	"tumbleweed-1",
}

type openstackFindImageComplexTest struct {
	imageName         string
	availableImages   []string
	expectedImageName string
}

var openstackFindImageComplexTests = []openstackFindImageComplexTest{{
	// trivial
	imageName:         "fedora-35",
	availableImages:   fakeOpenStackImageList,
	expectedImageName: "fedora-35",
}, {
	// simple string match of name
	imageName:         "ubuntu-22.04",
	availableImages:   fakeOpenStackImageList,
	expectedImageName: "auto-sync/ubuntu-22.04-something",
}, {
	// complex sorting based on date of the images
	imageName:         "ubuntu-bionic-18.04-amd64",
	availableImages:   fakeOpenStackImageList,
	expectedImageName: "auto-sync/ubuntu-bionic-18.04-amd64-server-20230530-disk1.img",
}, {
	// term matching is more flexible than simple substring matching
	imageName:         "ubuntu-18.04-server",
	availableImages:   fakeOpenStackImageList,
	expectedImageName: "auto-sync/ubuntu-bionic-18.04-amd64-server-20230530-disk1.img",
}}

func (s *openstackFindImageSuite) TestOpenStackFindImageComplex(c *C) {
	for _, tc := range openstackFindImageComplexTests {
		s.fakeImageClient.res = makeGlanceImageDetails(tc.availableImages)
		idt, err := spread.OpenStackFindImage(s.opst, tc.imageName)
		if tc.expectedImageName != "" {
			c.Check(err, IsNil, Commentf("%s", tc))
			c.Check(idt.Name, Equals, tc.expectedImageName)
		} else {

			c.Check(err, ErrorMatches, `cannot find matching image for `+tc.imageName, Commentf("%s", tc))
		}
	}
}

func (s *openstackFindImageSuite) TestOpenStackWaitProvisionHappy(c *C) {
	count := 0
	s.fakeComputeClient.getServer = func(serverId string) (*nova.ServerDetail, error) {
		count++
		c.Check(serverId, Equals, "test-id")
		switch count {
		case 1:
			server := nova.ServerDetail{
				Id:     serverId,
				Status: nova.StatusBuild,
			}
			return &server, nil
		case 2:
			server := nova.ServerDetail{
				Id:     serverId,
				Status: nova.StatusActive,
			}
			return &server, nil
		}
		c.Fatalf("should not reach here")
		return nil, nil
	}

	restore := spread.FakeOpenStackProvisionTimeout(100*time.Millisecond, time.Nanosecond)
	defer restore()

	err := spread.OpenStackWaitProvision(s.opst, context.TODO(), "test-id", "")
	c.Check(err, IsNil)
	c.Check(count, Equals, 2)
}

func (s *openstackFindImageSuite) TestOpenStackWaitProvisionBadStatus(c *C) {
	count := 0
	s.fakeComputeClient.getServer = func(serverId string) (*nova.ServerDetail, error) {
		count++
		c.Check(serverId, Equals, "test-id")
		switch count {
		case 1:
			server := nova.ServerDetail{
				Id:     serverId,
				Status: nova.StatusError,
			}
			return &server, nil
		}
		return nil, nil
	}

	restore := spread.FakeOpenStackProvisionTimeout(100*time.Millisecond, time.Nanosecond)
	defer restore()

	err := spread.OpenStackWaitProvision(s.opst, context.TODO(), "test-id", "")
	c.Check(err, ErrorMatches, "cannot use server: status is not active but ERROR")
	c.Check(count, Equals, 1)
}

func (s *openstackFindImageSuite) TestOpenStackWaitProvisionTimeout(c *C) {
	s.fakeComputeClient.getServer = func(serverId string) (*nova.ServerDetail, error) {
		return &nova.ServerDetail{Status: nova.StatusBuild}, nil
	}

	restore := spread.FakeOpenStackProvisionTimeout(100*time.Millisecond, time.Nanosecond)
	defer restore()

	err := spread.OpenStackWaitProvision(s.opst, context.TODO(), "", "test-server")
	c.Check(err, ErrorMatches, "timeout waiting for test-server to provision")
}

func (s *openstackFindImageSuite) TestOpenStackWaitServerBootSerialHappy(c *C) {
	restore := spread.FakeOpenStackServerBootTimeout(100*time.Millisecond, time.Nanosecond)
	defer restore()
	restore = spread.FakeOpenStackSerialOutputTimeout(50 * time.Millisecond)
	defer restore()

	var called int
	s.fakeOsClient.response = func() interface{} {
		called++
		switch called {
		case 1:
			return map[string]string{
				"output": "not-marker",
			}
		default:
			return map[string]string{
				"output": "MACHINE-IS-READY",
			}
		}
	}

	err := spread.OpenStackWaitServerBoot(s.opst, context.TODO(), "test-id", "", []string{"net-1"})
	c.Check(err, IsNil)
	c.Check(called, Equals, 2)
}

func (s *openstackFindImageSuite) TestOpenStackWaitServerBootSerialTimeout(c *C) {
	restore := spread.FakeOpenStackServerBootTimeout(100*time.Millisecond, time.Nanosecond)
	defer restore()
	restore = spread.FakeOpenStackSerialOutputTimeout(50 * time.Millisecond)
	defer restore()

	s.fakeOsClient.response = func() interface{} {
		return map[string]string{
			"output": "not-marker",
		}
	}

	err := spread.OpenStackWaitServerBoot(s.opst, context.TODO(), "test-id", "test-server", []string{"net-1"})
	c.Check(err, ErrorMatches, "cannot find ready marker in console output for test-server: timeout reached")
}

func (s *openstackFindImageSuite) TestOpenStackWaitServerBootSSHHappy(c *C) {
	count := 0
	spread.FakeSshDial(func(network, addr string, config *ssh.ClientConfig) (*ssh.Client, error) {
		count++
		switch count {
		case 1:
			return nil, errors.New("connection error")
		case 2:
			return &ssh.Client{}, nil
		}
		c.Fatalf("should not reach here")
		return nil, nil
	})

	restore := spread.FakeOpenStackServerBootTimeout(100*time.Millisecond, time.Nanosecond)
	defer restore()
	restore = spread.FakeOpenStackSerialOutputTimeout(50 * time.Millisecond)
	defer restore()

	// force fallback to SSH
	s.fakeOsClient.err = fmt.Errorf("serial not supported")

	err := spread.OpenStackWaitServerBoot(s.opst, context.TODO(), "test-id", "", []string{"net-1"})
	c.Check(err, IsNil)
	c.Check(count, Equals, 2)
}

func (s *openstackFindImageSuite) TestOpenStackWaitServerBootSSHTimeout(c *C) {
	spread.FakeSshDial(func(network, addr string, config *ssh.ClientConfig) (*ssh.Client, error) {
		return nil, errors.New("connection error")
	})

	restore := spread.FakeOpenStackServerBootTimeout(100*time.Millisecond, time.Nanosecond)
	defer restore()
	restore = spread.FakeOpenStackSerialOutputTimeout(50 * time.Millisecond)
	defer restore()

	// force fallback to SSH
	s.fakeOsClient.err = fmt.Errorf("serial not supported")

	err := spread.OpenStackWaitServerBoot(s.opst, context.TODO(), "test-id", "test-server", []string{"net-1"})
	c.Check(err, ErrorMatches, "cannot connect to server test-server: cannot ssh to the allocated instance: timeout reached")
}

func (s *openstackFindImageSuite) TestOpenStackCreateMachineVolumeAndOpts(c *C) {
	// flavors
	s.fakeComputeClient.listFlavors = func() ([]nova.Entity, error) {
		return []nova.Entity{{Id: "f1", Name: spread.OpenstackDefaultFlavor}}, nil
	}

	// availability zones returned
	s.fakeComputeClient.listAvailabilityZones = func() ([]nova.AvailabilityZone, error) {
		return []nova.AvailabilityZone{{Name: "zone-1"}}, nil
	}

	// image with minimal disk
	imgID := "i1"
	s.fakeImageClient.res = []glance.ImageDetail{
		{Id: imgID, Name: "ubuntu-20.04", Status: "ACTIVE", MinimumDisk: 1},
	}

	// network
	s.fakeNeutronClient.listNetworksV2 = func(filter ...*neutron.Filter) ([]neutron.NetworkV2, error) {
		return []neutron.NetworkV2{{Id: "net-id1", Name: "net-1"}}, nil
	}

	s.fakeNeutronClient.listSecurityGroupsV2 = func() ([]neutron.SecurityGroupV2, error) {
		return []neutron.SecurityGroupV2{{Id: "sg1", Name: "default"}}, nil
	}

	// capture the RunServer opts
	var capturedOpts nova.RunServerOpts
	s.fakeComputeClient.runServer = func(opts nova.RunServerOpts) (*nova.Entity, error) {
		capturedOpts = opts
		return &nova.Entity{Id: "srv1"}, nil
	}

	// getServer: BUILD then ACTIVE with addresses
	call := 0
	s.fakeComputeClient.getServer = func(id string) (*nova.ServerDetail, error) {
		call++
		if call == 1 {
			return &nova.ServerDetail{Id: id, Status: nova.StatusBuild}, nil
		}
		return &nova.ServerDetail{
			Id:     id,
			Status: nova.StatusActive,
			Addresses: map[string][]nova.IPAddress{
				"net-1": {
					{Address: "1.2.3.4", Version: 4},
				},
			},
		}, nil
	}

	// ensure serial console returns ready immediately
	s.fakeOsClient.response = func() interface{} {
		return map[string]string{"output": spread.OpenstackReadyMarker}
	}

	// Request a storage of 50 GB (in bytes) and a security group
	sys := &spread.System{
		Name:     "test",
		Image:    "ubuntu-20.04",
		Networks: []string{"net-1"},
		Storage:  50 * 1024 * 1024 * 1024,
		Groups:   []string{"default"},
	}

	srv, err := s.opst.Allocate(context.TODO(), sys)
	c.Assert(err, IsNil)
	c.Assert(srv, NotNil)

	// Verify block device mapping attached with expected volume size and properties.
	c.Assert(len(capturedOpts.BlockDeviceMappings), Equals, 1)
	bdm := capturedOpts.BlockDeviceMappings[0]
	c.Check(bdm.UUID, Equals, imgID)
	c.Check(bdm.VolumeSize, Equals, 50)
	c.Check(bdm.DestinationType, Equals, "volume")
	c.Check(bdm.SourceType, Equals, "image")
	c.Check(bdm.BootIndex, Equals, 0)
	// Default behaviour when VolumeAutoDelete is nil is to delete on termination
	c.Check(bdm.DeleteOnTermination, Equals, true)

	// Check other RunServer options
	c.Check(capturedOpts.FlavorId, Equals, "f1")
	c.Check(capturedOpts.ImageId, Equals, imgID)
	c.Check(len(capturedOpts.Networks), Equals, 1)
	c.Check(capturedOpts.Networks[0].NetworkId, Equals, "net-id1")
	c.Check(capturedOpts.AvailabilityZone, Equals, "zone-1")

	// Metadata contains expected keys
	c.Check(capturedOpts.Metadata["spread"], Equals, "true")
	c.Check(capturedOpts.Metadata["reuse"], Equals, "false")
}
