package spread

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
)

type JSONUnitTestCases struct {
	Successfull int              `json:"successfull,attr"`
	Failed      int              `json:"failed,attr"`
	Aborted     int              `json:"aborted,attr"`
	Total       int              `json:"total,attr"`
	TestCases   []*JSONUnitTestCase
}

func NewJSONUnitTestCases() *JSONUnitTestCases {
	return &JSONUnitTestCases{
		Successfull: 0,
		Failed: 0,
		Aborted: 0,
		Total: 0,
		TestCases: []*JSONUnitTestCase{},
	}
}

func (tc *JSONUnitTestCases) addTest(test *JSONUnitTestCase) {
	for _, t := range tc.TestCases {
    	if test.Backend == t.Backend && test.System == t.System && test.Name == t.Name {
    		if len(test.Details) > 0 {
    			if test.Details[0].Type == aborted {
    				tc.Aborted += 1
    				tc.Failed -= 1
    			} 
    			t.Details = append(t.Details, test.Details[0])
    		}
    		return
    	}
	}

	if len(test.Details) > 0 {
		if test.Details[0].Type == failed {
			tc.Failed += 1
		} else {
			tc.Aborted += 1
		}
	} else {
		tc.Successfull += 1
	}
	tc.Total += 1
	tc.TestCases = append(tc.TestCases, test)
}

type JSONUnitTestCase struct {
	Backend     string            `json:"backend,attr"`
	System      string            `json:"system,attr"`
	Name        string            `json:"name,attr"`
	Suite       string            `json:"suite,attr"`
	FullName    string            `json:"fullname,attr"`
	Details     []*JSONUnitDetail `json:"details,omitempty"`
}

func NewJSONUnitTestCase(testName string, backend string, system string, suiteName string) *JSONUnitTestCase {
	fullname := backend + ":" + system + ":" + suiteName + "/" + testName 
	return &JSONUnitTestCase{
				Backend:  backend,
				System:   system,
				Name:     testName,
				Suite:    suiteName,
				FullName: fullname,
				Details:  []*JSONUnitDetail{},
			}	
}

type JSONUnitDetail struct {
	Type     string `json:"type,attr"`
	Info     string `json:"info,attr"`
	Message  string `json:"message,attr"`
}

type JSONUnitReport struct {
	FileName string
	TestCases *JSONUnitTestCases
}

func NewJSONUnitReport(name string) Report {
	report := JSONUnitReport{}
	report.FileName = name
	report.TestCases = NewJSONUnitTestCases()
	return report 
}

func (r JSONUnitReport) finish() error {
	bytes, err := json.MarshalIndent(r.TestCases, "", "    ")
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
	r.TestCases.addTest(test)
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
