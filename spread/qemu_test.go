package spread_test

import (
	"github.com/snapcore/spread/spread"

	. "gopkg.in/check.v1"
)

type qemuSuite struct{}

var _ = Suite(&qemuSuite{})

func (s *qemuSuite) TestQemuCmdDefaults(c *C) {
	prj := &spread.Project{}
	backend := &spread.Backend{}
	options := &spread.Options{}
	qemu := spread.QEMU(prj, backend, options).(*spread.QemuProvider)
	c.Check(qemu, NotNil)
	cmd := spread.QemuProviderQemuCmd(qemu, "/path/to/img", 1234)
	c.Check(cmd.Args, DeepEquals, []string{
		"qemu-system-x86_64",
		"-nographic",
		"-enable-kvm",
		"-snapshot",
		"-m", "1500",
		"-net", "nic",
		"-net", "user,hostfwd=tcp:127.0.0.1:1234-:22",
		"-serial", "telnet:127.0.0.1:1334,server,nowait",
		"-monitor", "telnet:127.0.0.1:1434,server,nowait",
		"/path/to/img",
	})
}

func (s *qemuSuite) TestQemuCmdAdjustMemCpu(c *C) {
	prj := &spread.Project{}
	backend := &spread.Backend{
		CpuCount: 4,
		Memory:   4 * 1024 * 1024 * 1024,
	}
	options := &spread.Options{}
	qemu := spread.QEMU(prj, backend, options).(*spread.QemuProvider)
	c.Check(qemu, NotNil)
	cmd := spread.QemuProviderQemuCmd(qemu, "/path/to/img", 1234)
	c.Check(cmd.Args, DeepEquals, []string{
		"qemu-system-x86_64",
		"-smp", "4",
		"-nographic",
		"-enable-kvm",
		"-snapshot",
		"-m", "4096",
		"-net", "nic",
		"-net", "user,hostfwd=tcp:127.0.0.1:1234-:22",
		"-serial", "telnet:127.0.0.1:1334,server,nowait",
		"-monitor", "telnet:127.0.0.1:1434,server,nowait",
		"/path/to/img",
	})
}
