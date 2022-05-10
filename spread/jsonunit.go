package spread

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
)

type JSONUnitTestSet struct {
	Passed    int `json:"passed,attr"`
	Failed    int `json:"failed,attr"`
	Aborted   int `json:"aborted,attr"`
	Total     int `json:"total,attr"`
	TestCases []*JSONUnitTestCase
}

func NewJSONUnitTestSet() *JSONUnitTestSet {
	return &JSONUnitTestSet{}
}

func (tc *JSONUnitTestSet) addTest(test *JSONUnitTestCase) {
	for _, t := range tc.TestCases {
		if test.Backend == t.Backend && test.System == t.System && test.Suite == t.Suite && test.Name == t.Name {
			if len(test.Details) > 0 {
				if test.Details[0].Type == aborted {
					tc.Aborted += 1
					tc.Failed -= 1
					t.Status = aborted
				}
				t.Details = append(t.Details, test.Details[0])
			}
			return
		}
	}

	if len(test.Details) > 0 {
		if test.Details[0].Type == failed {
			tc.Failed += 1
			test.Status = failed
		} else {
			tc.Aborted += 1
			test.Status = aborted
		}
	} else {
		tc.Passed += 1
		test.Status = passed
	}
	tc.Total += 1
	tc.TestCases = append(tc.TestCases, test)
}

type JSONUnitTestCase struct {
	Status   string            `json:"status,attr"`
	Name     string            `json:"name,attr"`
	FullName string            `json:"fullname,attr"`
	Details  []*JSONUnitDetail `json:"details,omitempty"`
	Backend  string            `json:"backend,omitempty"`
	System   string            `json:"system,omitempty"`
	Suite    string            `json:"suite,omitempty"`
}

func NewJSONUnitTestCase(testName string, backend string, system string, suiteName string) *JSONUnitTestCase {
	fullname := fmt.Sprintf("%s:%s:%s/%s", backend, system, suiteName, testName)
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
	Type    string `json:"type,attr"`
	Info    string `json:"info,omitempty"`
	Message string `json:"message,omitempty"`
}

type JSONUnitReport struct {
	FileName string
	TestSet  *JSONUnitTestSet
}

func NewJSONUnitReport(name string) Report {
	report := JSONUnitReport{}
	report.FileName = name
	report.TestSet = NewJSONUnitTestSet()
	return report
}

func (r JSONUnitReport) cleanTestCases() {
	for _, t := range r.TestSet.TestCases {
		t.Backend = ""
		t.System = ""
		t.Suite = ""
	}
}

func (r JSONUnitReport) finish() error {
	r.cleanTestCases()
	bytes, err := json.MarshalIndent(r.TestSet, "", "    ")
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
	r.TestSet.addTest(test)
}

func (r JSONUnitReport) addFailedTest(suiteName string, backend string, system string, testName string, stage string) {
	testcase := NewJSONUnitTestCase(testName, backend, system, suiteName)
	detail := &JSONUnitDetail{
		Type:    failed,
		Info:    stage,
		Message: "",
	}
	testcase.Details = append(testcase.Details, detail)
	r.addTest(testcase)
}

func (r JSONUnitReport) addAbortedTest(suiteName string, backend string, system string, testName string) {
	testcase := NewJSONUnitTestCase(testName, backend, system, suiteName)
	detail := &JSONUnitDetail{
		Type:    aborted,
		Info:    "",
		Message: "",
	}
	testcase.Details = append(testcase.Details, detail)
	r.addTest(testcase)
}

func (r JSONUnitReport) addPassedTest(suiteName string, backend string, system string, testName string) {
	testcase := NewJSONUnitTestCase(testName, backend, system, suiteName)
	r.addTest(testcase)
}
