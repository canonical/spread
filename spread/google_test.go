package spread_test

import (
	"net/http"
	"net/http/httptest"

	"github.com/snapcore/spread/spread"

	. "gopkg.in/check.v1"
)

type googleSuite struct{}

var _ = Suite(&googleSuite{})

func (s *googleSuite) TestPagination(c *C) {
	n := 0
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch n {
		case 0:
			c.Check(r.ParseForm(), IsNil)
			c.Check(r.Form.Get("pageToken"), Equals, "")
			w.Write([]byte(`
                {
                    "items": [{"status":"READY","name":"ubuntu-1910-64-v20190826", "description":"ubuntu-19.10-64"}, {"status":"READY","name":"ubuntu-1904-64-v20190726", "description":"ubuntu-19.04-64"}],
                    "nextpagetoken": "token-1"
                }
                `))
		case 1:
			c.Check(r.ParseForm(), IsNil)
			c.Check(r.Form.Get("pageToken"), Equals, "token-1")
			w.Write([]byte(`
                {
                    "items": [{"status":"READY","name":"opensuse-leap-42-3-v20190227", "description":"opensuse-leap-42-3-64"}, {"status":"READY","name":"debian-9-v20190901", "description":"debian-9-64"}],
                    "nextpagetoken": "token-2"
                }
                `))
		case 2:
			c.Check(r.ParseForm(), IsNil)
			c.Check(r.Form.Get("pageToken"), Equals, "token-2")
			w.Write([]byte(`
                {
                    "items": [{"status":"READY","name":"fedora-28-64-v20181210", "description":"fedora-28-64"}],
                    "nextpagetoken": ""
                }
                `))
		}
		n++
	}))
	defer mockServer.Close()

	g := spread.NewGoogleProviderForTesting(mockServer.URL, nil, nil, nil)
	c.Assert(g, NotNil)

	images, err := g.ProjectImages("snapd")
	c.Assert(err, IsNil)
	// XXX: a `c.Check(images, DeepEquals, []spread.googleImage{...}`
	// would be nice here but "googleImage" is not exported so we can't
	// access it here. So we test a bit more indirect.
	c.Assert(images, HasLen, 5)
	i := 0
	c.Check(images[i].Project, Equals, "snapd")
	c.Check(images[i].Name, Equals, "ubuntu-1910-64-v20190826")
	c.Check(images[i].Terms, DeepEquals, []string{"ubuntu", "19.10", "64"})
	i++
	c.Check(images[i].Project, Equals, "snapd")
	c.Check(images[i].Name, Equals, "ubuntu-1904-64-v20190726")
	c.Check(images[i].Terms, DeepEquals, []string{"ubuntu", "19.04", "64"})
	i++
	c.Check(images[i].Project, Equals, "snapd")
	c.Check(images[i].Name, Equals, "opensuse-leap-42-3-v20190227")
	c.Check(images[i].Terms, DeepEquals, []string{"opensuse", "leap", "42", "3", "64"})
	i++
	c.Check(images[i].Project, Equals, "snapd")
	c.Check(images[i].Name, Equals, "debian-9-v20190901")
	c.Check(images[i].Terms, DeepEquals, []string{"debian", "9", "64"})
	i++
	c.Check(images[i].Project, Equals, "snapd")
	c.Check(images[i].Name, Equals, "fedora-28-64-v20181210")
	c.Check(images[i].Terms, DeepEquals, []string{"fedora", "28", "64"})
}
