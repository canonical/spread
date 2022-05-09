package spread

type Report interface {
	addFailedTest(suiteName string, backend string, system string, testName string, verb string)
	addAbortedTest(suiteName string, backend string, system string, testName string)
	addPassedTest(suiteName string, backend string, system string, testName string)
	finish() error
}

const (
	failed  = "failed"
	aborted = "aborted"
	passed  = "passed"
)
