package spread

import (
	"net/http"
	"time"

	"golang.org/x/crypto/ssh"
)

func FakeClient() *Client {
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

func FakeSshDial(f func(network, addr string, config *ssh.ClientConfig) (*ssh.Client, error)) (restore func()) {
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

type LXDServerJSON = lxdServerJSON

func LXDProviderServerJSON(provider Provider, name string) (*LXDServerJSON, error) {
	return provider.(*lxdProvider).serverJSON(name)
}

func FakeLXDList(f func(name string) ([]byte, error)) (restore func()) {
	oldLxdList := lxdList
	lxdList = f
	return func() {
		lxdList = oldLxdList
	}
}

var QemuCmd = qemuCmd

func FakeGoogleProvider(mockApiURL string, p *Project, b *Backend, o *Options) Provider {
	provider := Google(p, b, o)
	ggl := provider.(*googleProvider)
	ggl.apiURL = mockApiURL
	ggl.keyChecked = true
	ggl.client = &http.Client{}

	return provider
}

func ProjectImages(provider Provider, project string) ([]googleImage, error) {
	return provider.(*googleProvider).projectImages(project)
}

type GoogleImage = googleImage
