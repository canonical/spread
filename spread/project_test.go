package spread_test

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/snapcore/spread/spread"

	. "gopkg.in/check.v1"
)

func Test(t *testing.T) { TestingT(t) }

type filterSuite struct{}

var _ = Suite(&filterSuite{})

func (s *filterSuite) TestFilter(c *C) {
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

type projectSuite struct{}

var _ = Suite(&projectSuite{})

func makeMockProject(c *C, content []byte, tasks map[string]string) *spread.Project {
	root := c.MkDir()
	mockSpreadYaml := filepath.Join(root, "spread.yaml")
	err := ioutil.WriteFile(mockSpreadYaml, content, 0644)
	c.Assert(err, IsNil)
	for name, taskContent := range tasks {
		mockTaskYaml := filepath.Join(root, name, "task.yaml")
		err := os.MkdirAll(filepath.Dir(mockTaskYaml), 0755)
		c.Assert(err, IsNil)
		err = ioutil.WriteFile(mockTaskYaml, []byte(taskContent), 0644)
		c.Assert(err, IsNil)
	}
	prj, err := spread.Load(root)
	c.Assert(err, IsNil)
	return prj
}

func (s *projectSuite) TestProjectJobsWildcards(c *C) {
	for _, t := range []struct {
		taskSystems map[string]string
		jobs        []string
	}{
		{
			// default, all combinations
			map[string]string{},
			[]string{"qemu:system-1:tests/t1", "qemu:system-1:tests/t2", "qemu:system-2:tests/t1", "qemu:system-2:tests/t2"},
		},
		{
			// select specific backend
			map[string]string{"t1": "system-1", "t2": "system-2"},
			[]string{"qemu:system-1:tests/t1", "qemu:system-2:tests/t2"},
		},
		{
			// exclude specific
			map[string]string{"t1": "-system-1", "t2": "system-2"},
			[]string{"qemu:system-2:tests/t1", "qemu:system-2:tests/t2"},
		},
		{
			// exclude wildcard
			map[string]string{"t1": "-system-*", "t2": "system-2"},
			[]string{"qemu:system-2:tests/t2"},
		},
		{
			// include wildcard
			map[string]string{"t1": "+system-*", "t2": "system-2"},
			[]string{"qemu:system-1:tests/t1", "qemu:system-2:tests/t1", "qemu:system-2:tests/t2"},
		},
	} {
		projectYaml := []byte(`
project: my-project
path: /remote-path
backends:
  qemu:
    systems: [system-1, system-2]
suites:
  tests/:
    summary: mock
`)
		proj := makeMockProject(c, projectYaml, map[string]string{
			"tests/t1": "summary: t1",
			"tests/t2": "summary: t2",
		})
		for t, sys := range t.taskSystems {
			proj.Suites["tests/"].Tasks[t].Systems = []string{sys}
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
