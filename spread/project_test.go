package spread_test

import (
	"sort"
	"testing"

	"github.com/snapcore/spread/spread"

	. "gopkg.in/check.v1"
)

func Test(t *testing.T) { TestingT(t) }

type FilterSuite struct{}

var _ = Suite(&FilterSuite{})

func (s *FilterSuite) TestFilter(c *C) {
	job := &spread.Job{Name: "backend:image:suite/test:variant"}

	pass := []string{
		"backend",
		"backend:",
		"image",
		":image:",
		"suite/test",
		"suit...est",
		"suite/",
		"/test",
		":variant",
		"...",
		"im...",
		"...ge",
	}

	block := []string{
		"nothing",
		"noth...",
		"...hing",
		":backend",
		"suite",
		"test",
	}

	for _, s := range pass {
		f, err := spread.NewFilter([]string{s})
		c.Assert(err, IsNil)
		c.Assert(f.Pass(job), Equals, true, Commentf("Filter: %q", s))
	}

	for _, s := range block {
		f, err := spread.NewFilter([]string{s})
		c.Assert(err, IsNil)
		c.Assert(f.Pass(job), Equals, false, Commentf("Filter: %q", s))
	}
}

func (s *FilterSuite) TestEvalstr(c *C) {
	type goodTable struct {
		desc                   string
		root, sub1, sub2, want []string
	}
	for _, t := range []goodTable{
		{
			desc: "all absolute",
			root: []string{"1", "2"},
			sub1: []string{"1-1", "1-2"},
			sub2: []string{"2-1", "2-2"},
			want: []string{"2-1", "2-2"},
		}, {
			desc: "leaf relative",
			root: []string{"1", "2"},
			sub1: []string{"1-1", "1-2"},
			sub2: []string{"+2-1", "-1-2"},
			want: []string{"1-1", "2-1"},
		}, {
			desc: "most relative",
			root: []string{"1", "2"},
			sub1: []string{"+1-1", "+1-2", "-2"},
			sub2: []string{"+2-1", "-1-2"},
			want: []string{"1", "1-1", "2-1"},
		}, {
			desc: "leaf absolute",
			root: []string{"1", "2"},
			sub1: []string{"-2", "+1-1"},
			sub2: []string{"2-1", "2-2"},
			want: []string{"2-1", "2-2"},
		}, {
			desc: "leaf empty",
			root: []string{"1", "2"},
			sub1: []string{"1-1", "1-2"},
			sub2: []string{},
			want: []string{"1-1", "1-2"},
		}, {
			desc: "leaf glob",
			root: []string{"1", "2"},
			sub1: []string{"1-1", "1-2"},
			sub2: []string{"*"},
			want: []string{"*", "1", "1-1", "1-2", "2"}, // XXX: is this expected
		}, {
			desc: "mixed absolute-relative",
			root: []string{"1-1", "1-2", "1-3"},
			sub1: []string{"+2-1", "+2-2", "+2-3"},
			sub2: []string{"1-*", "-*-3"},
			want: []string{"1-*", "1-1", "1-2"}, // interesting that the glob isn't pruned
		},
	} {
		root := spread.Strmap("root", t.root)
		sub1 := spread.Strmap("sub1", t.sub1)
		sub2 := spread.Strmap("sub2", t.sub2)

		strs, err := spread.Evalstr(t.desc, root, sub1, sub2)
		c.Check(err, IsNil, Commentf("%s", t.desc))
		sort.Strings(strs)
		c.Check(strs, DeepEquals, t.want, Commentf("%s", t.desc))
	}

	type badTable struct {
		desc string
		root []string
		leaf []string
		want string
	}

	for _, t := range []badTable{
		{desc: "delta root[0] (impossible)", root: []string{"-0"}, want: "root specifies attr in delta format"},
		{desc: "delta root[1] (unnecessary)", root: []string{"0", "+1"}, want: "root specifies attr in delta format"},
		{desc: "bad leaf mix", leaf: []string{"0", "+1", "2"}, want: "leaf specifies attr using plain format after delta"},
	} {
		_, err := spread.Evalstr("attr", spread.Strmap("root", t.root), spread.Strmap("leaf", t.leaf))
		c.Check(err, ErrorMatches, t.want)
	}
}
