package spread

type Report interface {
	addFailedTest(suiteName string, backend string, system string, testName string, verb string)
	addAbortedTest(suiteName string, backend string, system string, testName string)
	addSuccessfullTest(suiteName string, backend string, system string, testName string)
	finish() error
}

const (
	failed      = "failed"
	aborted     = "aborted"
	successfull = "successfull"
)
