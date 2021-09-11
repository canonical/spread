package spread_test

import (
	"context"
	"fmt"
	"net"
	"time"

	"golang.org/x/crypto/ssh"

	. "gopkg.in/check.v1"

	"github.com/snapcore/spread/spread"
)

type mockSystem string

func (ms mockSystem) String() string { return string(ms) }

type clientSuite struct {
	ctx    context.Context
	system mockSystem
}

var _ = Suite(&clientSuite{})

func (s *clientSuite) SetUpTest(c *C) {
	s.ctx = context.Background()
	s.system = mockSystem("some-system")
}

func (s *clientSuite) TestDialOnReboot(c *C) {
	restore := spread.MockSshDial(func(network, addr string, config *ssh.ClientConfig) (*ssh.Client, error) {
		time.Sleep(1 * time.Second)
		return nil, fmt.Errorf("cannot connect")
	})
	defer restore()

	cli := spread.MockClient()
	spread.SetWarnTimeout(cli, 50*time.Millisecond)
	spread.SetKillTimeout(cli, 100*time.Millisecond)

	err := spread.DialOnReboot(cli, time.Time{})
	c.Check(err, ErrorMatches, "kill-timeout reached after mock-job reboot request")
}

func (s *clientSuite) TestWaitPortUpHappyNoCmd(c *C) {
	ln, err := net.Listen("tcp", "localhost:0")
	c.Assert(err, IsNil)
	go func() {
		conn, err := ln.Accept()
		c.Assert(err, IsNil)
		conn.Close()
	}()

	err = spread.WaitPortUp(s.ctx, s.system, ln.Addr().String())
	c.Assert(err, IsNil)
}

func (s *clientSuite) TestWaitPortCanBeCanceled(c *C) {
	ctx, cancelFn := context.WithCancel(s.ctx)
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancelFn()
	}()

	err := spread.WaitPortUp(ctx, s.system, "localhost:0")
	c.Assert(err, ErrorMatches, "cannot connect to some-system: interrupted")

}
