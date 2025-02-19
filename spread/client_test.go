package spread_test

import (
	"fmt"
	"time"

	"golang.org/x/crypto/ssh"

	. "gopkg.in/check.v1"

	"github.com/snapcore/spread/spread"
)

type clientSuite struct{}

var _ = Suite(&clientSuite{})

func (s *clientSuite) TestDialOnReboot(c *C) {
	restore := spread.FakeSshDial(func(network, addr string, config *ssh.ClientConfig) (*ssh.Client, error) {
		time.Sleep(1 * time.Second)
		return nil, fmt.Errorf("cannot connect")
	})
	defer restore()

	cli := spread.FakeClient()
	spread.SetWarnTimeout(cli, 50*time.Millisecond)
	spread.SetKillTimeout(cli, 100*time.Millisecond)

	err := spread.DialOnReboot(cli, "")
	c.Check(err, ErrorMatches, "kill-timeout reached after mock-job reboot request")
}
