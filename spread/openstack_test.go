package spread_test

import (
	"errors"
	"fmt"
	"io/ioutil"
	"time"

	"github.com/go-goose/goose/v5/glance"

	"github.com/snapcore/spread/spread"

	. "gopkg.in/check.v1"
)

func newOpenstack() *spread.OpenstackProvider {
	prj := &spread.Project{}
	b := &spread.Backend{}
	opts := &spread.Options{}

	o := spread.Openstack(prj, b, opts)
	return o.(*spread.OpenstackProvider)
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

func (s *openstackSuite) TestErrorMsg(c *C) {
	msg1 := spread.ErrorMsg(opstErr1)
	c.Check(msg1, Equals, `request (https://keystone.bos01.canonistack.canonical.com:5000/v3/tokens) returned unexpected status: 404`)

	msg2 := spread.ErrorMsg(opstErr2)
	c.Check(msg2, Equals, `request (https://keystone.bos01.canonistack.canonical.com:5000/v3/tokens) returned unexpected status: 503`)

	msg3 := spread.ErrorMsg(errors.New("other error"))
	c.Check(msg3, Equals, `other error`)
}

type fakeGlanceImageClient struct {
	res []glance.ImageDetail
	err error
}

func (ic *fakeGlanceImageClient) ListImagesDetail() ([]glance.ImageDetail, error) {
	return ic.res, ic.err
}

type openstackFindImageSuite struct {
	opst *spread.OpenstackProvider

	fakeImageClient *fakeGlanceImageClient
}

var _ = Suite(&openstackFindImageSuite{})

func (s *openstackFindImageSuite) SetUpTest(c *C) {
	s.opst = newOpenstack()
	c.Assert(s.opst, NotNil)
	s.fakeImageClient = &fakeGlanceImageClient{}

	spread.MockOpenstackImageClient(s.opst, s.fakeImageClient)
}

func (s *openstackFindImageSuite) TestOpenstackFindImageNotFound(c *C) {
	s.fakeImageClient.res = []glance.ImageDetail{
		{Name: "unreleated"},
	}

	_, err := s.opst.FindImage("ubuntu-22.04-64")
	c.Check(err, ErrorMatches, `cannot find matching image for ubuntu-22.04-64`)
}

func (s *openstackFindImageSuite) TestOpenstackFindImageErrors(c *C) {
	s.fakeImageClient.err = fmt.Errorf("boom")

	_, err := s.opst.FindImage("ubuntu-22.04-64")
	c.Check(err, ErrorMatches, `cannot retrieve images list: boom`)
}

func uuid() string {
	b, err := ioutil.ReadFile("/proc/sys/kernel/random/uuid")
	if err != nil {
		panic(err)
	}
	return string(b)
}

func makeGlanceImageDetails(imgs []string) []glance.ImageDetail {
	out := make([]glance.ImageDetail, len(imgs))
	for i, name := range imgs {
		// XXX: is this correct? the id is just a uuid?
		id := uuid()
		out[i] = glance.ImageDetail{Id: id, Name: name}
	}
	return out
}

func (s *openstackFindImageSuite) TestOpenstackFindImageHappy(c *C) {
	for _, tc := range []struct {
		imageName       string
		availableImages []string
		expectFind      bool
	}{
		{"ubuntu-22.04", []string{"fedora-35", "tumbleweed-1"}, false},
		{"ubuntu-22.04", []string{"fedora-35", "ubuntu-22.04"}, true},
	} {
		s.fakeImageClient.res = makeGlanceImageDetails(tc.availableImages)
		idt, err := s.opst.FindImage(tc.imageName)
		if tc.expectFind {
			c.Check(err, IsNil, Commentf("%s", tc))
			c.Check(idt.Name, Equals, tc.imageName)
		} else {

			c.Check(err, ErrorMatches, `cannot find matching image for `+tc.imageName, Commentf("%s", tc))
		}
	}
}
