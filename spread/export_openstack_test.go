package spread

import (
	"github.com/go-goose/goose/v5/glance"
)

var (
	OpenstackName = openstackName
)

type (
	OpenstackProvider = openstackProvider
	GlanceImageClient = glanceImageClient
)

func MockOpenstackImageClient(opst *OpenstackProvider, newIC glanceImageClient) (restore func()) {
	oldGlanceImageClient := opst.imageClient
	opst.imageClient = newIC
	return func() {
		opst.imageClient = oldGlanceImageClient
	}
}

func (opst *OpenstackProvider) FindImage(name string) (*glance.ImageDetail, error) {
	return opst.findImage(name)
}

func NewOpenstackError(gooseError error) error {
	return &openstackError{gooseError}
}
