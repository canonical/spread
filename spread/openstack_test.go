package spread_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"regexp"
	"time"

	"github.com/go-goose/goose/v5/glance"
	goosehttp "github.com/go-goose/goose/v5/http"
	"github.com/go-goose/goose/v5/nova"
	"golang.org/x/crypto/ssh"

	"github.com/snapcore/spread/spread"

	. "gopkg.in/check.v1"
)

func newOpenstack() spread.Provider {
	prj := &spread.Project{}
	b := &spread.Backend{}
	opts := &spread.Options{}

	return spread.Openstack(prj, b, opts)
}

type openstackSuite struct{}

var _ = Suite(&openstackSuite{})

func (s *openstackSuite) TestTrivial(c *C) {
	o := newOpenstack()
	c.Check(o, NotNil)
}

func (s *openstackSuite) TestOpenstackName(c *C) {
	restore := spread.MockTimeNow(func() time.Time {
		return time.Date(2007, 8, 22, 11, 59, 58, 987654321, time.UTC)
	})
	defer restore()

	name := spread.OpenstackName()
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

func (s *openstackSuite) TestOpenstackError(c *C) {
	err1 := spread.NewOpenstackError(opstErr1)
	c.Check(err1.Error(), Equals, `request (https://keystone.bos01.canonistack.canonical.com:5000/v3/tokens) returned unexpected status: 404`)

	err2 := spread.NewOpenstackError(opstErr2)
	c.Check(err2.Error(), Equals, `request (https://keystone.bos01.canonistack.canonical.com:5000/v3/tokens) returned unexpected status: 503`)

	err3 := spread.NewOpenstackError(errors.New("other error"))
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
}

var _ = Suite(&openstackFindImageSuite{})

func (s *openstackFindImageSuite) SetUpTest(c *C) {
	s.opst = newOpenstack()
	c.Assert(s.opst, NotNil)
	s.fakeImageClient = &fakeGlanceImageClient{}
	s.fakeComputeClient = &fakeNovaComputeClient{}
	s.fakeOsClient = &fakeOsClient{}

	spread.MockOpenstackImageClient(s.opst, s.fakeImageClient)
	spread.MockOpenstackComputeClient(s.opst, s.fakeComputeClient)
	spread.MockOpenstackGooseClient(s.opst, s.fakeOsClient)
}

func (s *openstackFindImageSuite) TestOpenstackFindImageNotFound(c *C) {
	s.fakeImageClient.res = []glance.ImageDetail{
		{Name: "unreleated"},
	}

	_, err := spread.OpenstackFindImage(s.opst, "ubuntu-22.04-64")
	c.Check(err, ErrorMatches, `cannot find matching image for "ubuntu-22.04-64"`)
}

func (s *openstackFindImageSuite) TestOpenstackFindImageErrors(c *C) {
	s.fakeImageClient.err = fmt.Errorf("boom")

	_, err := spread.OpenstackFindImage(s.opst, "ubuntu-22.04-64")
	c.Check(err, ErrorMatches, `cannot retrieve images list: boom`)
}

func uuid() string {
	b, err := ioutil.ReadFile("/proc/sys/kernel/random/uuid")
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

var fakeOpenstackImageListMadeUp = []string{
	"ubuntu-22.04",
	"ubuntu-20.04",
	"fedora-38",
}

func (s *openstackFindImageSuite) TestOpenstackFindImage(c *C) {
	for _, tc := range []struct {
		imageName       string
		availableImages []string
		expectFind      bool
	}{
		{"ubuntu-14.04", fakeOpenstackImageListMadeUp, false},
		{"ubuntu-22.04", fakeOpenstackImageListMadeUp, true},
	} {
		s.fakeImageClient.res = makeGlanceImageDetails(tc.availableImages)
		idt, err := spread.OpenstackFindImage(s.opst, tc.imageName)
		if tc.expectFind {
			c.Check(err, IsNil, Commentf("%s", tc))
			c.Check(idt.Name, Equals, tc.imageName)
		} else {

			c.Check(err, ErrorMatches, fmt.Sprintf(`cannot find matching image for %q`, tc.imageName), Commentf("%s", tc))
		}
	}
}

var fakeOpenstackImageList = []string{
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

func (s *openstackFindImageSuite) TestOpenstackFindImageComplex(c *C) {
	for _, tc := range []struct {
		imageName         string
		availableImages   []string
		expectedImageName string
	}{
		// trivial
		{"fedora-35", fakeOpenstackImageList, "fedora-35"},
		// simple string match of name
		{"ubuntu-22.04", fakeOpenstackImageList, "auto-sync/ubuntu-22.04-something"},
		// complex sorting based on date of the images
		{"ubuntu-bionic-18.04-amd64", fakeOpenstackImageList, "auto-sync/ubuntu-bionic-18.04-amd64-server-20230530-disk1.img"},
		// term matching is more flexible than simple substring matching
		{"ubuntu-18.04-server", fakeOpenstackImageList, "auto-sync/ubuntu-bionic-18.04-amd64-server-20230530-disk1.img"},
	} {
		s.fakeImageClient.res = makeGlanceImageDetails(tc.availableImages)
		idt, err := spread.OpenstackFindImage(s.opst, tc.imageName)
		if tc.expectedImageName != "" {
			c.Check(err, IsNil, Commentf("%s", tc))
			c.Check(idt.Name, Equals, tc.expectedImageName)
		} else {

			c.Check(err, ErrorMatches, `cannot find matching image for `+tc.imageName, Commentf("%s", tc))
		}
	}
}

func (s *openstackFindImageSuite) TestOpenstackWaitProvisionHappy(c *C) {
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

	restore := spread.MockOpenstackProvisionTimeout(100*time.Millisecond, time.Nanosecond)
	defer restore()

	err := spread.OpenstackWaitProvision(s.opst, context.TODO(), "test-id", "")
	c.Check(err, IsNil)
	c.Check(count, Equals, 2)
}

func (s *openstackFindImageSuite) TestOpenstackWaitProvisionBadStatus(c *C) {
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

	restore := spread.MockOpenstackProvisionTimeout(100*time.Millisecond, time.Nanosecond)
	defer restore()

	err := spread.OpenstackWaitProvision(s.opst, context.TODO(), "test-id", "")
	c.Check(err, ErrorMatches, "cannot use server: status is not active but ERROR")
	c.Check(count, Equals, 1)
}

func (s *openstackFindImageSuite) TestOpenstackWaitProvisionTimeout(c *C) {
	s.fakeComputeClient.getServer = func(serverId string) (*nova.ServerDetail, error) {
		return &nova.ServerDetail{Status: nova.StatusBuild}, nil
	}

	restore := spread.MockOpenstackProvisionTimeout(100*time.Millisecond, time.Nanosecond)
	defer restore()

	err := spread.OpenstackWaitProvision(s.opst, context.TODO(), "", "test-server")
	c.Check(err, ErrorMatches, "timeout waiting for test-server to provision")
}

func (s *openstackFindImageSuite) TestOpenstackWaitServerBootSerialHappy(c *C) {
	restore := spread.MockOpenstackServerBootTimeout(100*time.Millisecond, time.Nanosecond)
	defer restore()
	restore = spread.MockOpenstackSerialOutputTimeout(50 * time.Millisecond)
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

	err := spread.OpenstackWaitServerBoot(s.opst, context.TODO(), "test-id", "", []string{"net-1"})
	c.Check(err, IsNil)
	c.Check(called, Equals, 2)
}

func (s *openstackFindImageSuite) TestOpenstackWaitServerBootSerialTimeout(c *C) {
	restore := spread.MockOpenstackServerBootTimeout(100*time.Millisecond, time.Nanosecond)
	defer restore()
	restore = spread.MockOpenstackSerialOutputTimeout(50 * time.Millisecond)
	defer restore()

	s.fakeOsClient.response = func() interface{} {
		return map[string]string{
			"output": "not-marker",
		}
	}

	err := spread.OpenstackWaitServerBoot(s.opst, context.TODO(), "test-id", "test-server", []string{"net-1"})
	c.Check(err, ErrorMatches, "cannot find ready marker in console output for test-server: timeout reached")
}

func (s *openstackFindImageSuite) TestOpenstackWaitServerBootSSHHappy(c *C) {
	count := 0
	spread.MockSshDial(func(network, addr string, config *ssh.ClientConfig) (*ssh.Client, error) {
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

	restore := spread.MockOpenstackServerBootTimeout(100*time.Millisecond, time.Nanosecond)
	defer restore()
	restore = spread.MockOpenstackSerialOutputTimeout(50 * time.Millisecond)
	defer restore()

	// force fallback to SSH
	s.fakeOsClient.err = fmt.Errorf("serial not supported")

	err := spread.OpenstackWaitServerBoot(s.opst, context.TODO(), "test-id", "", []string{"net-1"})
	c.Check(err, IsNil)
	c.Check(count, Equals, 2)
}

func (s *openstackFindImageSuite) TestOpenstackWaitServerBootSSHTimeout(c *C) {
	spread.MockSshDial(func(network, addr string, config *ssh.ClientConfig) (*ssh.Client, error) {
		return nil, errors.New("connection error")
	})

	restore := spread.MockOpenstackServerBootTimeout(100*time.Millisecond, time.Nanosecond)
	defer restore()
	restore = spread.MockOpenstackSerialOutputTimeout(50 * time.Millisecond)
	defer restore()

	// force fallback to SSH
	s.fakeOsClient.err = fmt.Errorf("serial not supported")

	err := spread.OpenstackWaitServerBoot(s.opst, context.TODO(), "test-id", "test-server", []string{"net-1"})
	c.Check(err, ErrorMatches, "cannot connect to server test-server: cannot ssh to the allocated instance: timeout reached")
}
