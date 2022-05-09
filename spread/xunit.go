package spread

import (
	"encoding/xml"
	"fmt"
	"io/ioutil"
)

type XUnitTestSuites struct {
	XMLName xml.Name `xml:"testsuites"`
	Suites  []*XUnitTestSuite
}

func (tss *XUnitTestSuites) addSuite(suite *XUnitTestSuite) {
	tss.Suites = append(tss.Suites, suite)
}

func (tss *XUnitTestSuites) getSuite(suiteName string) *XUnitTestSuite {
	for _, s := range tss.Suites {
		if s.Name == suiteName {
			return s
		}
	}
	suite := NewXUnitTestSuite(suiteName)

	tss.addSuite(suite)
	return suite
}

type XUnitTestSuite struct {
	XMLName   xml.Name `xml:"testsuite"`
	Passed    int      `xml:"passed,attr"`
	Failed    int      `xml:"failed,attr"`
	Aborted   int      `xml:"aborted,attr"`
	Total     int      `xml:"total,attr"`
	Name      string   `xml:"name,attr"`
	TestCases []*XUnitTestCase
}

func NewXUnitTestSuite(suiteName string) *XUnitTestSuite {
	return &XUnitTestSuite{
		Passed:    0,
		Failed:    0,
		Aborted:   0,
		Total:     0,
		Name:      suiteName,
		TestCases: []*XUnitTestCase{},
	}
}

func (ts *XUnitTestSuite) addTest(test *XUnitTestCase) {
	for _, t := range ts.TestCases {
		if test.Backend == t.Backend && test.System == t.System && test.Name == t.Name {
			if len(test.Details) > 0 {
				if test.Details[0].Type == aborted {
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
		if test.Details[0].Type == failed {
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
	ts.TestCases = append(ts.TestCases, test)
}

type XUnitTestCase struct {
	XMLName   xml.Name       `xml:"testcase"`
	Status    string         `xml:"status,attr"`
	Classname string         `xml:"classname,attr"`
	Name      string         `xml:"name,attr"`
	Details   []*XUnitDetail `xml:"details,omitempty"`
	Backend   string         `xml:"backend,omitempty"`
	System    string         `xml:"system,omitempty"`
	Suite     string         `xml:"suite,omitempty"`
}

func NewXUnitTestCase(testName string, backend string, system string, suiteName string) *XUnitTestCase {
	classname := backend + ":" + system + ":" + suiteName + "/"
	return &XUnitTestCase{
		Classname: classname,
		Backend:   backend,
		System:    system,
		Name:      testName,
		Suite:     suiteName,
		Details:   []*XUnitDetail{},
	}
}

type XUnitDetail struct {
	XMLName xml.Name `xml:"failure"`
	Type    string   `xml:"type,attr"`
	Info    string   `xml:"info,omitempty"`
	Message string   `xml:"message,omitempty"`
}

type XUnitReport struct {
	FileName   string
	TestSuites *XUnitTestSuites
}

func NewXUnitReport(name string) Report {
	report := XUnitReport{}
	report.FileName = name
	report.TestSuites = &XUnitTestSuites{}
	return report
}

func (r XUnitReport) cleanTestCases() {
	for _, ts := range r.TestSuites.Suites {
		for _, t := range ts.TestCases {
			t.Backend = ""
			t.System = ""
			t.Suite = ""
		}
	}
}

func (r XUnitReport) finish() error {
	r.cleanTestCases()
	bytes, err := xml.MarshalIndent(r.TestSuites, "", "\t")
	if err != nil {
		return fmt.Errorf("cannot indent the XUnit report: %v", err)
	}

	err = ioutil.WriteFile(r.FileName, bytes, 0644)
	if err != nil {
		return fmt.Errorf("cannot write XUnit report to %s file: %v", r.FileName, err)
	}

	return nil
}

func (r XUnitReport) addTest(test *XUnitTestCase) {
	suite := r.TestSuites.getSuite(test.Suite)
	suite.addTest(test)
}

func (r XUnitReport) addFailedTest(suiteName string, backend string, system string, testName string, verb string) {
	testcase := NewXUnitTestCase(testName, backend, system, suiteName)
	detail := &XUnitDetail{
		Type:    failed,
		Info:    verb,
		Message: "",
	}
	testcase.Details = append(testcase.Details, detail)
	r.addTest(testcase)
}

func (r XUnitReport) addAbortedTest(suiteName string, backend string, system string, testName string) {
	testcase := NewXUnitTestCase(testName, backend, system, suiteName)
	detail := &XUnitDetail{
		Type:    aborted,
		Info:    "",
		Message: "",
	}
	testcase.Details = append(testcase.Details, detail)
	r.addTest(testcase)
}

func (r XUnitReport) addPassedTest(suiteName string, backend string, system string, testName string) {
	testcase := NewXUnitTestCase(testName, backend, system, suiteName)
	r.addTest(testcase)
}
