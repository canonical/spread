package spread

import (
	"context"
	"time"

	gooseclient "github.com/go-goose/goose/v5/client"
	"github.com/go-goose/goose/v5/glance"
)

var (
	OpenStackName = openstackName
)

func FakeOpenStackImageClient(p Provider, imageClient glanceImageClient) (restore func()) {
	opst := p.(*openstackProvider)
	oldGlanceImageClient := opst.imageClient
	opst.imageClient = imageClient
	return func() {
		opst.imageClient = oldGlanceImageClient
	}
}

func FakeOpenStackComputeClient(p Provider, computeClient novaComputeClient) (restore func()) {
	opst := p.(*openstackProvider)
	oldNovaImageClient := opst.computeClient
	opst.computeClient = computeClient
	return func() {
		opst.computeClient = oldNovaImageClient
	}
}

func FakeOpenStackGooseClient(p Provider, gooseClient gooseclient.Client) (restore func()) {
	opst := p.(*openstackProvider)
	oldOsClient := opst.osClient
	opst.osClient = gooseClient
	return func() {
		opst.osClient = oldOsClient
	}
}

func FakeOpenStackProvisionTimeout(timeout, retry time.Duration) (restore func()) {
	oldTimeout := openstackProvisionTimeout
	oldRetry := openstackProvisionRetry
	openstackProvisionTimeout = timeout
	openstackProvisionRetry = retry
	return func() {
		openstackProvisionTimeout = oldTimeout
		openstackProvisionRetry = oldRetry
	}
}

func FakeOpenStackServerBootTimeout(timeout, retry time.Duration) (restore func()) {
	oldTimeout := openstackServerBootTimeout
	oldRetry := openstackServerBootRetry
	openstackServerBootTimeout = timeout
	openstackServerBootRetry = retry
	return func() {
		openstackServerBootTimeout = oldTimeout
		openstackServerBootRetry = oldRetry
	}
}

func FakeOpenStackSerialOutputTimeout(timeout time.Duration) (restore func()) {
	oldTimeout := openstackSerialOutputTimeout
	openstackSerialOutputTimeout = timeout
	return func() {
		openstackSerialOutputTimeout = oldTimeout
	}
}

func OpenStackFindImage(p Provider, name string) (*glance.ImageDetail, error) {
	opst := p.(*openstackProvider)
	return opst.findImage(name)
}

func OpenStackWaitProvision(p Provider, ctx context.Context, serverID, serverName string) error {
	opst := p.(*openstackProvider)
	server := &openstackServer{
		p: opst,
		d: openstackServerData{
			Id:   serverID,
			Name: serverName,
		},
	}
	return opst.waitProvision(ctx, server)
}

func OpenStackWaitServerBoot(p Provider, ctx context.Context, serverID, serverName string, serverNetworks []string) error {
	opst := p.(*openstackProvider)
	server := &openstackServer{
		p: opst,
		d: openstackServerData{
			Id:       serverID,
			Name:     serverName,
			Networks: serverNetworks,
		},
	}
	return opst.waitServerBoot(ctx, server)
}

func NewOpenStackError(gooseError error) error {
	return &openstackError{gooseError}
}
