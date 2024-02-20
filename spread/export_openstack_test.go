package spread

import (
	"context"
	"time"

	gooseClient "github.com/go-goose/goose/v5/client"
	"github.com/go-goose/goose/v5/glance"
)

var (
	OpenstackName = openstackName
)

type (
	OpenstackProvider   = openstackProvider
	GlanceImageClient   = glanceImageClient
	OpenstackServerData = openstackServerData
)

func MockOpenstackImageClient(opst *OpenstackProvider, newIC glanceImageClient) (restore func()) {
	oldGlanceImageClient := opst.imageClient
	opst.imageClient = newIC
	return func() {
		opst.imageClient = oldGlanceImageClient
	}
}

func MockOpenstackComputeClient(opst *OpenstackProvider, newIC novaComputeClient) (restore func()) {
	oldNovaImageClient := opst.computeClient
	opst.computeClient = newIC
	return func() {
		opst.computeClient = oldNovaImageClient
	}
}

func MockOpenstackGooseClient(opst *OpenstackProvider, newC gooseClient.Client) (restore func()) {
	oldOsClient := opst.osClient
	opst.osClient = newC
	return func() {
		opst.osClient = oldOsClient
	}
}

func MockOpenstackProvisionTimeout(timeout, retry time.Duration) (restore func()) {
	oldTimeout := openstackProvisionTimeout
	oldRetry := openstackProvisionRetry
	openstackProvisionTimeout = timeout
	openstackProvisionRetry = retry
	return func() {
		openstackProvisionTimeout = oldTimeout
		openstackProvisionRetry = oldRetry
	}
}

func MockOpenstackServerBootTimeout(timeout, retry time.Duration) (restore func()) {
	oldTimeout := openstackServerBootTimeout
	oldRetry := openstackServerBootRetry
	openstackServerBootTimeout = timeout
	openstackServerBootRetry = retry
	return func() {
		openstackServerBootTimeout = oldTimeout
		openstackServerBootRetry = oldRetry
	}
}

func (opst *OpenstackProvider) FindImage(name string) (*glance.ImageDetail, error) {
	return opst.findImage(name)
}

func (opst *openstackProvider) WaitProvision(ctx context.Context, serverData OpenstackServerData) error {
	server := &openstackServer{
		p: opst,
		d: serverData,
	}
	return opst.waitProvision(ctx, server)
}

func (opst *openstackProvider) WaitServerBoot(ctx context.Context, serverData OpenstackServerData) error {
	server := &openstackServer{
		p: opst,
		d: serverData,
	}
	return opst.waitServerBoot(ctx, server)
}

func NewOpenstackError(gooseError error) error {
	return &openstackError{gooseError}
}
