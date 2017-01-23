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

type ProjectSuite struct{}

var _ = Suite(&ProjectSuite{})

func makeProject() *spread.Project {
	prj := &spread.Project{
		Name:       "my-project",
		RemotePath: "/remote-path",
		Backends: map[string]*spread.Backend{
			"backend-1": &spread.Backend{
				Name:        "my-backend",
				Environment: spread.NewEnvironment(),
				Systems: spread.SystemsMap{
					"system-1": &spread.System{
						Backend: "backend-1",
						Name:    "system-1",
					},
					"system-2": &spread.System{
						Backend: "backend-1",
						Name:    "system-2",
					},
				},
			},
		},
		Suites: map[string]*spread.Suite{
			"my-suite": &spread.Suite{
				Summary:  "my-suite",
				Systems:  []string{"system-1", "system-2"},
				Backends: []string{"backend-1"},
				Tasks: map[string]*spread.Task{
					"t1": &spread.Task{
						Suite: "my-suite",
						Name:  "t1",
					},
					"t2": &spread.Task{
						Suite: "my-suite",
						Name:  "t2",
					},
				},
			},
		},
	}
	return prj
}

func (s *ProjectSuite) TestProjectJobsWildcards(c *C) {
	for _, t := range []struct {
		taskSystems map[string]string
		jobs        []string
	}{
		{
			// default, all combinations
			map[string]string{},
			[]string{"my-backend:system-1:t1", "my-backend:system-1:t2", "my-backend:system-2:t1", "my-backend:system-2:t2"},
		},
		{
			// select specific backend
			map[string]string{"t1": "system-1", "t2": "system-2"},
			[]string{"my-backend:system-1:t1", "my-backend:system-2:t2"},
		},
		{
			// exclude specific
			map[string]string{"t1": "-system-1", "t2": "system-2"},
			[]string{"my-backend:system-2:t1", "my-backend:system-2:t2"},
		},
		{
			// exclude wildcard
			map[string]string{"t1": "-system-*", "t2": "system-2"},
			[]string{"my-backend:system-2:t2"},
		},
		{
			// include wildcard
			map[string]string{"t1": "+system-*", "t2": "system-2"},
			[]string{"my-backend:system-1:t1", "my-backend:system-2:t1", "my-backend:system-2:t2"},
		},
	} {
		proj := makeProject()
		for t, sys := range t.taskSystems {
			proj.Suites["my-suite"].Tasks[t].Systems = []string{sys}
		}
		allJobs, err := proj.Jobs(&spread.Options{})
		c.Check(err, IsNil)

		jobs := make([]string, len(allJobs))
		for i, s := range allJobs {
			jobs[i] = s.String()
		}
		sort.Strings(jobs)

		c.Check(jobs, DeepEquals, t.jobs, Commentf("unexpected matches for %v: %v", t.taskSystems, jobs))
	}
}
