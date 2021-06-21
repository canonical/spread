package spread_test

import (
	"context"
	"fmt"
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

// XXX: use a better mocking library, e.g. reuse
// github.com/snapcore/snapd/testutil/exec.go
func makeMockQemuBinary(c *C) (callLogFile string, restore func()) {
	mockQemuBinary := filepath.Join(c.MkDir(), "mocked-qemu")
	callLogFile = mockQemuBinary + ".log"

	script := fmt.Sprintf(`#!/bin/sh -e
printf "%%s" "$(basename "$0")" >> %[1]q
printf '\0' >> %[1]q

for arg in "$@"; do
     printf "%%s" "$arg" >> %[1]q
     printf '\0'  >> %[1]q
done

# extract what port is expected from waitPortUp()
port=$(echo $@ | sed -E -n  's/^.*hostfwd=tcp:127.0.0.1:([0-9]+)-:22.*$/\1/p')
# this will exit after the connection of waitPortUp()
nc -l $port
`, callLogFile)
	err := ioutil.WriteFile(mockQemuBinary, []byte(script), 0755)
	c.Assert(err, IsNil)

	restore = spread.MockQemuBinary(mockQemuBinary)
	return callLogFile, restore
}

var _ = Suite(&qemuSuite{})

func (s *qemuSuite) TestQemuAllocateWithUefi(c *C) {
	imageName := "ubuntu-20.06-64"

	callLogFile, restore := makeMockQemuBinary(c)
	defer restore()
	restore = makeMockQemuImg(c, imageName)
	defer restore()

	ctx := context.Background()

	tests := []struct {
		BiosSetting    string
		ContainsNeedle bool
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
		q := spread.QEMU(&spread.Project{}, &spread.Backend{}, &spread.Options{})
		backend, err := q.Allocate(ctx, ms)
		c.Assert(err, IsNil)
		c.Check(backend, NotNil)

		qemuCalls, err := ioutil.ReadFile(callLogFile)
		c.Assert(err, IsNil)
		needle := "\000-bios\000/usr/share/OVMF/OVMF_CODE.fd\000"
		c.Check(strings.Contains(string(qemuCalls), needle), Equals, tc.ContainsNeedle)
		os.Remove(callLogFile)
	}
}
