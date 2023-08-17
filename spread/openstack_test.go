package spread_test

import (
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
