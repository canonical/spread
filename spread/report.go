package spread

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io/ioutil"
	"path/filepath"
)

const (
	failed  = "failed"
	aborted = "aborted"
	passed  = "passed"
)

type ResultsTestSuite struct {
	Name    string             `xml:"name,attr" json:"name,attr"`
	Passed  int                `xml:"passed,attr" json:"passed,attr"`
	Failed  int                `xml:"failed,attr" json:"failed,attr"`
	Aborted int                `xml:"aborted,attr" json:"aborted,attr"`
	Total   int                `xml:"total,attr" json:"total,attr"`
	Tests   []*ResultsTestCase `xml:"test,omitempty" json:"tests,omitempty"`
}

func NewResultsTestSuite(name string) *ResultsTestSuite {
	return &ResultsTestSuite{
		Name:    name,
		Passed:  0,
		Failed:  0,
		Aborted: 0,
		Total:   0,
		Tests:   []*ResultsTestCase{},
	}
}

func (ts *ResultsTestSuite) addTest(test *ResultsTestCase) {
	for _, t := range ts.Tests {
		// The test case has failed and now it is aborted
		// in this case it counts as aborted and not as failed
		// The total is not changed in this scenario
		if test.Name == t.Name {
			if len(test.Failures) > 0 {
				if test.Failures[0].Result == aborted {
					ts.Aborted += 1
					ts.Failed -= 1
					t.Result = aborted
				}
				t.Failures = append(t.Failures, test.Failures[0])
			}
			return
		}
	}

	if len(test.Failures) > 0 {
		if test.Failures[0].Result == failed {
			ts.Failed += 1
			test.Result = failed
		} else {
			ts.Aborted += 1
			test.Result = aborted
		}
	} else {
		ts.Passed += 1
		test.Result = passed
	}
	ts.Total += 1
	ts.Tests = append(ts.Tests, test)
}

type ResultsTestCase struct {
	Name     string `xml:"name,attr" json:"name,attr"`
	suite    string
	Result   string            `xml:"result,attr" json:"result,attr"`
	Failures []*ResultsFailure `xml:"failure,omitempty" json:"failure,omitempty"`
}

func NewResultsTestCase(testName string, backend string, system string, suiteName string) *ResultsTestCase {
	name := fmt.Sprintf("%s:%s:%s/%s", backend, system, suiteName, testName)
	return &ResultsTestCase{
		Name:     name,
		suite:    suiteName,
		Failures: []*ResultsFailure{},
	}
}

type ResultsFailure struct {
	Info   string `xml:"info" json:"info"`
	Result string `xml:"result" json:"result"`
}

func NewResultsFailure(info string, Result string) *ResultsFailure {
	return &ResultsFailure{
		Info:   info,
		Result: Result,
	}
}

type ResultsReport struct {
	Passed  int                 `xml:"passed,attr" json:"passed,attr"`
	Failed  int                 `xml:"failed,attr" json:"failed,attr"`
	Aborted int                 `xml:"aborted,attr" json:"aborted,attr"`
	Total   int                 `xml:"total,attr" json:"total,attr"`
	Suites  []*ResultsTestSuite `xml:"testsuite,omitempty" json:"testsuites,omitempty"`
}

func CreateResultsReport(results stats) error {
	report := &ResultsReport{
		Suites: []*ResultsTestSuite{},
	}
	report.complete(results)
	return report.write()
}

func (r *ResultsReport) addTests(testsList []*Job, result string, stage string) {
	for _, job := range testsList {
		// task names look like paths (eg suite/foo/bar/job)
		name := taskName(job)
		suiteName := filepath.Dir(name)
		testName := filepath.Base(name)

		if result == failed {
			r.addFailedTest(suiteName, job.Backend.Name, job.System.Name, testName, stage)
		} else if result == aborted {
			r.addAbortedTest(suiteName, job.Backend.Name, job.System.Name, testName)
		} else {
			r.addPassedTest(suiteName, job.Backend.Name, job.System.Name, testName)
		}
	}
}

func (r *ResultsReport) complete(results stats) {
	r.addTests(results.TaskPrepareError, failed, preparing)
	r.addTests(results.TaskError, failed, executing)
	r.addTests(results.TaskRestoreError, failed, restoring)
	r.addTests(results.TaskAbort, aborted, "")
	r.addTests(results.TaskDone, passed, "")

	for _, s := range r.Suites {
		r.Total += s.Total
		r.Aborted += s.Aborted
		r.Failed += s.Failed
		r.Passed += s.Passed
	}
}

func (r *ResultsReport) getSuite(suiteName string) *ResultsTestSuite {
	for _, s := range r.Suites {
		if s.Name == suiteName {
			return s
		}
	}
	suite := NewResultsTestSuite(suiteName)
	r.Suites = append(r.Suites, suite)
	return suite
}

func (r ResultsReport) write() error {
	err := r.writeXML("spread_report.xml")
	if err != nil {
		return err
	}
	err = r.writeJSON("spread_report.json")
	if err != nil {
		return err
	}
	return nil
}

func (r *ResultsReport) writeXML(filename string) error {
	bytes, err := xml.MarshalIndent(r, "", "\t")
	if err != nil {
		return fmt.Errorf("cannot indent the Results report: %v", err)
	}

	err = ioutil.WriteFile(filename, bytes, 0644)
	if err != nil {
		return fmt.Errorf("cannot write Results report to %s file: %v", filename, err)
	}

	return nil
}

func (r ResultsReport) writeJSON(filename string) error {
	bytes, err := json.MarshalIndent(r, "", "    ")
	if err != nil {
		return fmt.Errorf("cannot indent the JSONUnit report: %v", err)
	}

	err = ioutil.WriteFile(filename, bytes, 0644)
	if err != nil {
		return fmt.Errorf("cannot write JSONUnit report to %s file: %v", filename, err)
	}

	return nil
}

func (r *ResultsReport) addTest(test *ResultsTestCase) {
	suite := r.getSuite(test.suite)
	suite.addTest(test)
}

func (r *ResultsReport) addFailedTest(suiteName string, backend string, system string, testName string, stage string) {
	testcase := NewResultsTestCase(testName, backend, system, suiteName)
	failure := NewResultsFailure(stage, failed)
	testcase.Failures = append(testcase.Failures, failure)
	r.addTest(testcase)
}

func (r *ResultsReport) addAbortedTest(suiteName string, backend string, system string, testName string) {
	testcase := NewResultsTestCase(testName, backend, system, suiteName)
	failure := NewResultsFailure(executing, aborted)
	testcase.Failures = append(testcase.Failures, failure)
	r.addTest(testcase)
}

func (r *ResultsReport) addPassedTest(suiteName string, backend string, system string, testName string) {
	testcase := NewResultsTestCase(testName, backend, system, suiteName)
	r.addTest(testcase)
}
