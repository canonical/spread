package spread_test

import (
	"context"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/snapcore/spread/spread"

	. "gopkg.in/check.v1"
)

type qemuSuite struct{}

var _ = Suite(&qemuSuite{})

func makeMockQemuImg(c *C, mockSystemName string) (restore func()) {
	tmpdir := c.MkDir()
	realHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpdir)

	mockQemuDir := filepath.Join(tmpdir, ".spread/qemu")
	err := os.MkdirAll(mockQemuDir, 0755)
	c.Assert(err, IsNil)

	err = ioutil.WriteFile(filepath.Join(mockQemuDir, mockSystemName+".img"), nil, 0644)
	c.Assert(err, IsNil)

	return func() {
		os.Setenv("HOME", realHome)
	}
}

func (s *qemuSuite) TestQemuAllocateDetectsFailingQemu(c *C) {
	mockImageName := "some-system"

	restore := spread.MockQemuBinary("false")
	defer restore()
	restore = makeMockQemuImg(c, mockImageName)
	defer restore()

	ctx := context.Background()
	ms := &spread.System{
		Name:    mockImageName,
		Image:   mockImageName,
		Backend: "qemu",
	}
	q := spread.QEMU(&spread.Project{}, &spread.Backend{}, &spread.Options{})
	_, err := q.Allocate(ctx, ms)
	c.Assert(err, ErrorMatches, "cannot connect to qemu:some-system: interrupted")
}
