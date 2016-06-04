package spread

import (
	"github.com/kr/pretty"
)

// Logger defines the logger where messages should be sent to.
// The standard *log.Logger type implements this interface.
var Logger interface {
	Output(calldepth int, s string) error
}

// Verbose defines whether to also deliver verbose messages to the log.
var Verbose bool

// Debug defines whether to also deliver debug messages to the log. Implies Verbose if set.
var Debug bool

func print(v ...interface{}) {
	if Logger != nil {
		Logger.Output(2, pretty.Sprint(v...))
	}
}

func printf(format string, v ...interface{}) {
	if Logger != nil {
		Logger.Output(2, pretty.Sprintf(format, v...))
	}
}

func log(v ...interface{}) {
	if (Verbose || Debug) && Logger != nil {
		Logger.Output(2, pretty.Sprint(v...))
	}
}

func logf(format string, v ...interface{}) {
	if (Verbose || Debug) && Logger != nil {
		Logger.Output(2, pretty.Sprintf(format, v...))
	}
}

func debug(v ...interface{}) {
	if Debug && Logger != nil {
		Logger.Output(2, pretty.Sprint(v...))
	}
}

func debugf(format string, v ...interface{}) {
	if Debug && Logger != nil {
		Logger.Output(2, pretty.Sprintf(format, v...))
	}
}

func nth(n int, word0 string, wordN ...string) string {
	if n == 0 || len(wordN) == 0 {
		return word0
	}
	if n >= len(wordN) {
		return wordN[len(wordN)-1]
	}
	return wordN[n-1]
}
