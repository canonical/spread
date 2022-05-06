package spread

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
)

type JSONUnitTestSuites struct {
	Suites   []*JSONUnitTestSuite
}

func (tss *JSONUnitTestSuites) addSuite(suite *JSONUnitTestSuite) {
	tss.Suites = append(tss.Suites, suite)
}

func (tss *JSONUnitTestSuites) getSuite(suiteName string) *JSONUnitTestSuite {
    for _, s := range tss.Suites {
        if s.Name == suiteName {
            return s
        }
    }
	suite := NewJSONUnitTestSuite(suiteName)

	tss.addSuite(suite)	
	return suite
}

type JSONUnitTestSuite struct {
	Successfull int              `json:"successfull,attr"`
	Failed      int              `json:"failed,attr"`
	Aborted     int              `json:"aborted,attr"`
	Total       int              `json:"total,attr"`
	Time        string           `json:"time,attr"`
	Name        string           `json:"name,attr"`
	TestCases   []*JSONUnitTestCase
}

func NewJSONUnitTestSuite(suiteName string) *JSONUnitTestSuite {
	return &JSONUnitTestSuite{
		Successfull: 0,
		Failed: 0,
		Aborted: 0,
		Total: 0,
		Time: "",
		Name: suiteName,
		TestCases: []*JSONUnitTestCase{},
	}
}

func (ts *JSONUnitTestSuite) addTest(test *JSONUnitTestCase) {
	for _, t := range ts.TestCases {
    	if test.Backend == t.Backend && test.System == t.System && test.Name == t.Name {
    		if len(test.Details) > 0 {
    			t.Details = append(t.Details, test.Details[0])
    		}
    		return
    	}
	}

	if len(test.Details) > 0 {
		if test.Details[0].Type == failed {
			ts.Failed += 1
		} else {
			ts.Aborted += 1	
		}
	} else {
		ts.Successfull += 1
	}
	ts.Total += 1
	ts.TestCases = append(ts.TestCases, test)
}

type JSONUnitTestCase struct {
	Backend     string            `json:"backend,attr"`
	System      string            `json:"system,attr"`
	Name        string            `json:"name,attr"`
	Suite       string            `json:"suite,attr"`
	Details     []*JSONUnitDetail    `json:"details,omitempty"`
}

func NewJSONUnitTestCase(testName string, backend string, system string, suitename string) *JSONUnitTestCase {
	return &JSONUnitTestCase{
				Backend: backend,
				System:  system,
				Name:    testName,
				Suite:   suitename,
				Details:  []*JSONUnitDetail{},
			}	
}

type JSONUnitDetail struct {
	Type     string `json:"type,attr"`
	Info     string `json:"info,attr"`
	Message  string `json:"message,attr"`
}

type JSONUnitSkipMessage struct {
	Message string `json:"message,attr"`
}

type JSONUnitFailure struct {
	Message  string `json:"message,attr"`
	Type     string `json:"type,attr"`
}

type JSONUnitAbort struct {
	Message  string `json:"message,attr"`
	Type     string `json:"type,attr"`
}

type JSONUnitReport struct {
	FileName string
	TestSuites *JSONUnitTestSuites
}

func NewJSONUnitReport(name string) Report {
	report := JSONUnitReport{}
	report.FileName = name
	report.TestSuites = &JSONUnitTestSuites{}
	return report 
}

func (r JSONUnitReport) finish() error {
	bytes, err := json.MarshalIndent(r.TestSuites, "", "    ")
	if err != nil {
		return fmt.Errorf("cannot indent the JSONUnit report: %v", err)
	}

	err = ioutil.WriteFile(r.FileName, bytes, 0644)
    if err != nil {
		return fmt.Errorf("cannot write JSONUnit report to %s file: %v", r.FileName, err)
	}

	return nil
}

func (r JSONUnitReport) addTest(test *JSONUnitTestCase) {
	suite := r.TestSuites.getSuite(test.Suite)
	suite.addTest(test)
}

func (r JSONUnitReport) addFailedTest(suiteName string, backend string, system string, testName string, verb string) {
	testcase := NewJSONUnitTestCase(testName, backend, system, suiteName)
	detail := &JSONUnitDetail{
				Type:     failed,
				Info:     verb,
				Message:  "",
			}
	testcase.Details = append(testcase.Details, detail)
	r.addTest(testcase)
}

func (r JSONUnitReport) addAbortedTest(suiteName string, backend string, system string, testName string) {
	testcase := NewJSONUnitTestCase(testName, backend, system, suiteName)
	detail := &JSONUnitDetail{
				Type:     aborted,
				Info:     "",
				Message:  "",
			}
	testcase.Details = append(testcase.Details, detail)
	r.addTest(testcase)
}

func (r JSONUnitReport) addSuccessfullTest(suiteName string, backend string, system string, testName string) {
	testcase := NewJSONUnitTestCase(testName, backend, system, suiteName)
	r.addTest(testcase)
}
