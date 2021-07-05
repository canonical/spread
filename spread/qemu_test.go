package spread_test

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/snapcore/spread/spread"

	. "gopkg.in/check.v1"
)

type qemuSuite struct{}

func makeMockQemuImg(c *C, mockSystemName string) (restore func()) {
	tmpdir := c.MkDir()

	mockQemuDir := filepath.Join(tmpdir, ".spread/qemu")
	err := os.MkdirAll(mockQemuDir, 0755)
	c.Assert(err, IsNil)

	err = ioutil.WriteFile(filepath.Join(mockQemuDir, mockSystemName+".img"), nil, 0644)
	c.Assert(err, IsNil)

	realHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpdir)
	return func() {
		os.Setenv("HOME", realHome)
	}
}

var _ = Suite(&qemuSuite{})

func (s *qemuSuite) TestQemuCmdWithEfi(c *C) {
	imageName := "ubuntu-20.06-64"

	restore := makeMockQemuImg(c, imageName)
	defer restore()

	tests := []struct {
		BiosSetting       string
		UseBiosQemuOption bool
	}{
		{"uefi", true},
		{"legacy", false},
	}

	for _, tc := range tests {
		ms := &spread.System{
			Name:    "some-name",
			Image:   imageName,
			Backend: "qemu",
			Bios:    tc.BiosSetting,
		}
		cmd, err := spread.QemuCmd(ms, "/path/to/image", 512, 9999)
		c.Assert(err, IsNil)

		// XXX: reuse testutil.Contains from snapd?
		s := strings.Join(cmd.Args, ":")
		c.Check(strings.Contains(s, ":-bios:/usr/share/OVMF/OVMF_CODE.fd:"), Equals, tc.UseBiosQemuOption)
	}
}
