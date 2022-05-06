package spread

import (
	"encoding/xml"
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
	suite := NewTestSuite(suiteName)

	tss.addSuite(suite)	
	return suite
}

type XUnitTestSuite struct {
	XMLName     xml.Name         `xml:"testsuite"`
	Successfull int              `xml:"successfull,attr"`
	Failed      int              `xml:"failed,attr"`
	Aborted     int              `xml:"aborted,attr"`
	Total       int              `xml:"total,attr"`
	Time        string           `xml:"time,attr"`
	Name        string           `xml:"name,attr"`
	TestCases   []*XUnitTestCase
}

func NewTestSuite(suiteName string) *XUnitTestSuite {
	return &XUnitTestSuite{
		Successfull: 0,
		Failed: 0,
		Aborted: 0,
		Total: 0,
		Time: "",
		Name: suiteName,
		TestCases: []*XUnitTestCase{},
	}
}

func (ts *XUnitTestSuite) addTest(test *XUnitTestCase) {
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

type XUnitTestCase struct {
	XMLName     xml.Name          `xml:"testcase"`
	Backend     string            `xml:"backend,attr"`
	System      string            `xml:"system,attr"`
	Name        string            `xml:"name,attr"`
	Suite       string            `xml:"suite,attr"`
	Details     []*XUnitDetail    `xml:"details,omitempty"`
}

func NewTestCase(testName string, backend string, system string, suitename string) *XUnitTestCase {
	return &XUnitTestCase{
				Backend: backend,
				System:  system,
				Name:    testName,
				Suite:   suitename,
				Details:  []*XUnitDetail{},
			}	
}

type XUnitDetail struct {
	Type     string `xml:"type,attr"`
	Info     string `xml:"info,attr"`
	Message  string `xml:"message,attr"`
}

type XUnitSkipMessage struct {
	Message string `xml:"message,attr"`
}

type XUnitFailure struct {
	Message  string `xml:"message,attr"`
	Type     string `xml:"type,attr"`
}

type XUnitAbort struct {
	Message  string `xml:"message,attr"`
	Type     string `xml:"type,attr"`
}

type XUnitReport struct {
	FileName string
	TestSuites *XUnitTestSuites
}

func NewXUnitReport(name string) XUnitReport {
	report := XUnitReport{}
	report.FileName = name
	report.TestSuites = &XUnitTestSuites{}
	return report 
}

func (r XUnitReport) finish() error {
	bytes, err := xml.MarshalIndent(r.TestSuites, "", "\t")
	check(err)

	err = ioutil.WriteFile(r.FileName, bytes, 0644)
    check(err)

	return nil
}

func (r XUnitReport) addTest(test *XUnitTestCase) {
	suite := r.TestSuites.getSuite(test.Suite)
	suite.addTest(test)
}

func (r XUnitReport) addFailedTest(suiteName string, backend string, system string, testName string, verb string) {
	testcase := NewTestCase(testName, backend, system, suiteName)
	detail := &XUnitDetail{
				Type:     failed,
				Info:     verb,
				Message:  "",
			}
	testcase.Details = append(testcase.Details, detail)
	r.addTest(testcase)
}

func (r XUnitReport) addAbortedTest(suiteName string, backend string, system string, testName string) {
	testcase := NewTestCase(testName, backend, system, suiteName)
	detail := &XUnitDetail{
				Type:     aborted,
				Info:     "",
				Message:  "",
			}
	testcase.Details = append(testcase.Details, detail)
	r.addTest(testcase)
}

func (r XUnitReport) addSuccessfullTest(suiteName string, backend string, system string, testName string) {
	testcase := NewTestCase(testName, backend, system, suiteName)
	r.addTest(testcase)
}

func check(e error) {
    if e != nil {
        panic(e)
    }
}
