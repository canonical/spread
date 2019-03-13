package spread_test

import (
	"testing"

	"github.com/snapcore/spread/spread"

	. "gopkg.in/check.v1"
)

func Test(t *testing.T) { TestingT(t) }

type FilterSuite struct{}

var _ = Suite(&FilterSuite{})

func (s *FilterSuite) TestFilter(c *C) {
	job := &spread.Job{Name: "backend:image:suite/test:variant"}

	pass := []string{
		"backend",
		"backend:",
		"image",
		":image:",
		"suite/test",
		"suit...est",
		"suite/",
		"/test",
		":variant",
		"...",
		"im...",
		"...ge",
	}

	block := []string{
		"nothing",
		"north...",
		"...hing",
		":backend",
		"suite",
		"test",
	}

	for _, s := range pass {
		f, err := spread.NewFilter([]string{s})
		c.Assert(err, IsNil)
		c.Assert(f.Pass(job), Equals, true, Commentf("Filter: %q", s))
	}

	for _, s := range block {
		f, err := spread.NewFilter([]string{s})
		c.Assert(err, IsNil)
		c.Assert(f.Pass(job), Equals, false, Commentf("Filter: %q", s))
	}
}
