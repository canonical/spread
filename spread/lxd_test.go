package spread_test

import (
	"errors"

	. "gopkg.in/check.v1"

	"github.com/snapcore/spread/spread"
)

type lxdSuite struct{}

var _ = Suite(&lxdSuite{})

type serverJSONTest struct {
	insList    string // The instances list from lxc list in JSON format
	insListErr error  // The error returned when listing instances
	name       string // The server name to retrieve information from
	errMsg     string // The expected error message
}

var serverJSONTests = []serverJSONTest{{
	insList:    "",
	insListErr: errors.New("lxc list error"),
	name:       "foo",
	errMsg:     "lxc list error",
}, {
	insList:    "",
	insListErr: nil,
	name:       "foo",
	errMsg:     "cannot unmarshal lxd list output: unexpected end of JSON input",
}, {
	insList:    `[{"name": "foo", "state": {"network": {}}}]`,
	insListErr: nil,
	name:       "foo",
	errMsg:     "",
}, {
	insList:    `[{"name": "foo", "state": {"network": {}}}]`,
	insListErr: nil,
	name:       "bar",
	errMsg:     `cannot find lxd server "bar"`,
}, {
	insList:    `[{"name": "foo-2", "state": {"network": {}}}, {"name": "foo", "state": {"network": {}}}]`,
	insListErr: nil,
	name:       "foo",
	errMsg:     "",
}}

func (s *lxdSuite) TestServerJSON(c *C) {
	for _, tc := range serverJSONTests {
		restore := spread.FakeLXDList(func(name string) ([]byte, error) { return []byte(tc.insList), tc.insListErr })
		defer restore()

		lxd := spread.LXD(nil, nil, nil)
		sjson, err := spread.LXDProviderServerJSON(lxd, tc.name)
		if tc.errMsg == "" {
			c.Assert(err, IsNil)
			c.Check(sjson.Name, Equals, tc.name)
		} else {
			c.Assert(err, ErrorMatches, tc.errMsg)
		}
	}
}
