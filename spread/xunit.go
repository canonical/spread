package spread

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io/ioutil"
)

const (
	failed    = "failed"
	aborted   = "aborted"
	passed    = "passed"
)

type XUnitTestSuite struct {
	Name     string           `xml:"name,attr" json:"name,attr"`
	Passed   int              `xml:"passed,attr" json:"passed,attr"`
	Failed   int              `xml:"failed,attr" json:"failed,attr"`
	Aborted  int              `xml:"aborted,attr" json:"aborted,attr"`
	Total    int              `xml:"total,attr" json:"total,attr"`
	Tests    []*XUnitTestCase `xml:"test,omitempty" json:"tests,omitempty"`
}

func NewXUnitTestSuite(name string) *XUnitTestSuite {
	return &XUnitTestSuite{
		Name:    name,
		Passed:  0,
		Failed:  0,
		Aborted: 0,
		Total:   0,
		Tests:   []*XUnitTestCase{},
	}
}

func (ts *XUnitTestSuite) addTest(test *XUnitTestCase) {
	for _, t := range ts.Tests {
		// The test case has failed and now it is aborted
		// in this case it counts as aborted and not as failed
		// The total is not changed in this scenario
		if test.Name == t.Name {
			if len(test.Details) > 0 {
				if test.Details[0].Status == aborted {
					ts.Aborted += 1
					ts.Failed -= 1
					t.Status = aborted
				}
				t.Details = append(t.Details, test.Details[0])
			}
			return
		}		
	}

	if len(test.Details) > 0 {
		if test.Details[0].Status == failed {
			ts.Failed += 1
			test.Status = failed
		} else {
			ts.Aborted += 1
			test.Status = aborted
		}
	} else {
		ts.Passed += 1
		test.Status = passed
	}
	ts.Total += 1
	ts.Tests = append(ts.Tests, test)
}

type XUnitTestCase struct {
	Name      string          `xml:"name,attr" json:"name,attr"`
	suite     string
	Status    string          `xml:"status,attr" json:"status,attr"`
	Details   []*XUnitDetail  `xml:"detail,omitempty" json:"detail,omitempty"`
}

func NewXUnitTestCase(testName string, backend string, system string, suiteName string) *XUnitTestCase {
	name := fmt.Sprintf("%s:%s:%s/%s", backend, system, suiteName, testName)
	return &XUnitTestCase{
		Name:    name,
		suite:   suiteName, 
		Details: []*XUnitDetail{},
	}
}

type XUnitDetail struct {
	Info    string    `xml:"info" json:"info"`
	Status  string    `xml:"status" json:"status"`
}

func NewXUnitDetail(info string, status string) *XUnitDetail {
	return &XUnitDetail{
		Info:    info,
		Status:  status,
	}
}

type XUnitReport struct {
	Suites  []*XUnitTestSuite `xml:"testsuite,omitempty" json:"testsuites,omitempty"`
}

func NewXUnitReport() *XUnitReport {
	return &XUnitReport{
		Suites: []*XUnitTestSuite{},
	}
}

func (r *XUnitReport) getSuite(suiteName string) *XUnitTestSuite {
	for _, s := range r.Suites {
		if s.Name == suiteName {
			return s
		}
	}
	suite := NewXUnitTestSuite(suiteName)
	r.Suites = append(r.Suites, suite)
	return suite
}

func (r *XUnitReport) finishXML(filename string) error {
	bytes, err := xml.MarshalIndent(r, "", "\t")
	if err != nil {
		return fmt.Errorf("cannot indent the XUnit report: %v", err)
	}

	err = ioutil.WriteFile(filename, bytes, 0644)
	if err != nil {
		return fmt.Errorf("cannot write XUnit report to %s file: %v", filename, err)
	}

	return nil
}

func (r XUnitReport) finishJSON(filename string) error {
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

func (r *XUnitReport) addTest(test *XUnitTestCase) {
	suite := r.getSuite(test.suite)
	suite.addTest(test)
}

func (r *XUnitReport) addFailedTest(suiteName string, backend string, system string, testName string, stage string) {
	testcase := NewXUnitTestCase(testName, backend, system, suiteName)
	detail := NewXUnitDetail(stage, failed)
	testcase.Details = append(testcase.Details, detail)
	r.addTest(testcase)
}

func (r *XUnitReport) addAbortedTest(suiteName string, backend string, system string, testName string) {
	testcase := NewXUnitTestCase(testName, backend, system, suiteName)
	detail := NewXUnitDetail(executing, aborted)
	testcase.Details = append(testcase.Details, detail)
	r.addTest(testcase)
}

func (r *XUnitReport) addPassedTest(suiteName string, backend string, system string, testName string) {
	testcase := NewXUnitTestCase(testName, backend, system, suiteName)
	r.addTest(testcase)
}
