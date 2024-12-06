package spread

import (
	"time"

	"golang.org/x/crypto/ssh"
)

func MockClient() *Client {
	config := &ssh.ClientConfig{
		User:    "mock",
		Timeout: 10 * time.Second,
	}
	return &Client{
		config: config,
		job:    "mock-job",
	}
}

func DialOnReboot(cli *Client, prevBootID string) error {
	return cli.dialOnReboot(prevBootID)
}

func SetKillTimeout(cli *Client, killTimeout time.Duration) {
	cli.killTimeout = killTimeout
}

func SetWarnTimeout(cli *Client, warnTimeout time.Duration) {
	cli.warnTimeout = warnTimeout
}

func MockSshDial(f func(network, addr string, config *ssh.ClientConfig) (*ssh.Client, error)) (restore func()) {
	oldSshDial := sshDial
	sshDial = f
	return func() {
		sshDial = oldSshDial
	}
}

func MockTimeNow(f func() time.Time) (restore func()) {
	oldTimeNow := timeNow
	timeNow = f
	return func() {
		timeNow = oldTimeNow
	}
}

var QemuCmd = qemuCmd
