package spread

import (
	"bytes"
	stdlog "log"
	"github.com/kr/pretty"
	"sync"
)

// Logger defines the logger where messages should be sent to.
var Logger *stdlog.Logger

// Verbose defines whether to also deliver verbose messages to the log.
var Verbose bool

// Debug defines whether to also deliver debug messages to the log. Implies Verbose if set.
var Debug bool

func print(args ...interface{}) {
	if Logger != nil {
		writeLog(pretty.Sprint(args...))
	}
}

func printf(format string, args ...interface{}) {
	if Logger != nil {
		writeLog(pretty.Sprintf(format, args...))
	}
}

func log(args ...interface{}) {
	if (Verbose || Debug) && Logger != nil {
		writeLog(pretty.Sprint(args...))
	}
}

func logf(format string, args ...interface{}) {
	if (Verbose || Debug) && Logger != nil {
		writeLog(pretty.Sprintf(format, args...))
	}
}

func debug(args ...interface{}) {
	if Debug && Logger != nil {
		writeLog(pretty.Sprint(args...))
	}
}

func debugf(format string, args ...interface{}) {
	if Debug && Logger != nil {
		writeLog(pretty.Sprintf(format, args...))
	}
}

var termMu sync.Mutex
var logMu sync.Mutex
var logCache bytes.Buffer
var logSaved stdlog.Logger

func writeLog(line string) {
	logMu.Lock()
	defer logMu.Unlock()
	Logger.Output(3, line)
}

func termLock() {
	termMu.Lock()
	logMu.Lock()
	logSaved = *Logger
	Logger.SetOutput(&logCache)
	logMu.Unlock()
}

func termUnlock() {
	logMu.Lock()
	*Logger = logSaved
	Logger.SetFlags(0)
	Logger.SetPrefix("")
	Logger.Output(0, logCache.String())
	*Logger = logSaved
	logCache.Truncate(0)
	logMu.Unlock()
	termMu.Unlock()
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
