package spread

import (
	"time"

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

func MockOpenstackBuildingTimeout(timeout, retry time.Duration) (restore func()) {
	oldTimeout := openstackBuildingTimeout
	oldRetry := openstackBuildingRetry
	openstackBuildingTimeout = timeout
	openstackBuildingRetry = retry
	return func() {
		openstackBuildingTimeout = oldTimeout
		openstackBuildingRetry = oldRetry
	}
}

func MockOpenstackSetupTimeout(timeout, retry time.Duration) (restore func()) {
	oldTimeout := openstackSetupTimeout
	oldRetry := openstackSetupRetry
	openstackSetupTimeout = timeout
	openstackSetupRetry = retry
	return func() {
		openstackSetupTimeout = oldTimeout
		openstackSetupRetry = oldRetry
	}
}

func (opst *OpenstackProvider) FindImage(name string) (*glance.ImageDetail, error) {
	return opst.findImage(name)
}

func (opst *openstackProvider) WaitServerCompleteBuilding(serverData OpenstackServerData) error {
	server := &openstackServer{
		p: opst,
		d: serverData,
	}
	return opst.waitServerCompleteBuilding(server)
}

func (opst *openstackProvider) WaitServerCompleteSetup(serverData OpenstackServerData) error {
	server := &openstackServer{
		p: opst,
		d: serverData,
	}
	return opst.waitServerCompleteSetup(server)
}

func NewOpenstackError(gooseError error) error {
	return &openstackError{gooseError}
}
