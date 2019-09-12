package spread

import (
	"net/http"
)

func NewGoogleProviderForTesting(mockApiURL string, p *Project, b *Backend, o *Options) *googleProvider {
	provider := Google(p, b, o)
	ggl := provider.(*googleProvider)
	ggl.apiURL = mockApiURL
	ggl.keyChecked = true
	ggl.client = &http.Client{}

	return ggl
}

func (p *googleProvider) ProjectImages(project string) ([]googleImage, error) {
	return p.projectImages(project)
}
