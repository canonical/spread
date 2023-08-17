package spread_test

import (
	"time"

	"github.com/snapcore/spread/spread"

	. "gopkg.in/check.v1"
)

type openstackSuite struct{}

var _ = Suite(&openstackSuite{})

func (s *openstackSuite) TestTrivial(c *C) {
	prj := &spread.Project{}
	b := &spread.Backend{}
	opts := &spread.Options{}

	o := spread.Openstack(prj, b, opts)
	c.Check(o, NotNil)
}

func (s *openstackSuite) TestOpenstackName(c *C) {
	restore := spread.MockTimeNow(func() time.Time {
		return time.Date(2007, 8, 22, 11, 59, 58, 999, time.UTC)
	})
	defer restore()

	name := spread.OpenstackName()
	c.Check(name, Equals, "aug221159-000000")
}
