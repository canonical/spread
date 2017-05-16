package spread

import (
	"strconv"
	"time"

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
	XMLName    xml.Name        `xml:"testsuite"`
	Tests      int             `xml:"tests,attr"`
	Failures   int             `xml:"failures,attr"`
	Time       string          `xml:"time,attr"`
	Name       string          `xml:"name,attr"`
	Properties []*XUnitProperty `xml:"properties>property,omitempty"`
	TestCases  []*XUnitTestCase
}

func NewTestSuite(suiteName string) *XUnitTestSuite {
	return &XUnitTestSuite{
		Tests: 0,
		Failures: 0,
		Time: "",
		Name: suiteName,
		Properties: []*XUnitProperty{},
		TestCases: []*XUnitTestCase{},
	}
}

func (ts *XUnitTestSuite) addTest(test *XUnitTestCase) {
	if test.Failure != nil {
		ts.Failures += 1
	}
	ts.Tests += 1
	ts.TestCases = append(ts.TestCases, test)
}

type XUnitTestCase struct {
	XMLName     xml.Name          `xml:"testcase"`
	Classname   string            `xml:"classname,attr"`
	Name        string            `xml:"name,attr"`
	Time        string            `xml:"time,attr"`
	SkipMessage *XUnitSkipMessage `xml:"skipped,omitempty"`
	Failure     *XUnitFailure     `xml:"failure,omitempty"`
}

func NewTestCase(testName string, className string, duration time.Duration) *XUnitTestCase {
	return &XUnitTestCase{
				Classname: className,
				Name:      testName,
				Time:      strconv.FormatInt(int64(duration/time.Second), 10),
				Failure:   nil,
			}	
}

type XUnitSkipMessage struct {
	Message string `xml:"message,attr"`
}

type XUnitProperty struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

type XUnitFailure struct {
	Message  string `xml:"message,attr"`
	Type     string `xml:"type,attr"`
	Contents string `xml:",chardata"`
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

func (r XUnitReport) addTest(suiteName string, test *XUnitTestCase) {
	suite := r.TestSuites.getSuite(suiteName)
	suite.addTest(test)
}

func (r XUnitReport) addFailedTest(suiteName string, className string, testName string, duration time.Duration) {
	testcase := NewTestCase(testName, className, duration)
	testcase.Failure = &XUnitFailure{
					Message:  "Failed",
					Type:     "",
					Contents: "",
				}
	r.addTest(suiteName, testcase)
}

func (r XUnitReport) addPassedTest(suiteName string, className string, testName string, duration time.Duration) {
	testcase := NewTestCase(testName, className, duration)
	r.addTest(suiteName, testcase)
}

func check(e error) {
    if e != nil {
        panic(e)
    }
}
