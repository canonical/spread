package spread_test

import (
	"errors"
	"fmt"
	"io/ioutil"
	"regexp"
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
		idt, err := s.opst.FindImage(tc.imageName)
		if tc.expectFind {
			c.Check(err, IsNil, Commentf("%s", tc))
			c.Check(idt.Name, Equals, tc.imageName)
		} else {

			c.Check(err, ErrorMatches, `cannot find matching image for `+tc.imageName, Commentf("%s", tc))
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
		idt, err := s.opst.FindImage(tc.imageName)
		if tc.expectedImageName != "" {
			c.Check(err, IsNil, Commentf("%s", tc))
			c.Check(idt.Name, Equals, tc.expectedImageName)
		} else {

			c.Check(err, ErrorMatches, `cannot find matching image for `+tc.imageName, Commentf("%s", tc))
		}
	}
}
