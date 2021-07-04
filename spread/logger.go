package spread

import (
	"bytes"
	"fmt"
	stdlog "log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/niemeyer/pretty"
)

// Logger defines the logger where messages should be sent to.
var Logger *stdlog.Logger

// Verbose defines whether to also deliver verbose messages to the log.
var Verbose bool

// Debug defines whether to also deliver debug messages to the log. Implies Verbose if set.
var Debug bool

func printf(format string, args ...interface{}) {
	if Logger != nil {
		writeLog(timePrefix() + pretty.Sprintf(format, args...))
	}
}

func logf(format string, args ...interface{}) {
	if (Verbose || Debug) && Logger != nil {
		writeLog(timePrefix() + pretty.Sprintf(format, args...))
	}
}

func debug(args ...interface{}) {
	if Debug && Logger != nil {
		writeLog(timePrefix() + pretty.Sprint(args...))
	}
}

func debugf(format string, args ...interface{}) {
	if Debug && Logger != nil {
		writeLog(timePrefix() + pretty.Sprintf(format, args...))
	}
}

func timePrefix() string {
	return time.Now().Format("2006-01-02 15:04:05 ")
}

var termMu sync.Mutex
var logMu sync.Mutex
var logCache bytes.Buffer
var logSaved stdlog.Logger

func writeLog(lines ...string) {
	logMu.Lock()
	defer logMu.Unlock()
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			Logger.Output(3, line)
		}
	}
}

type logFlags int

func (f logFlags) with(flag logFlags) bool { return f&flag != 0 }

const (
	startTime logFlags = 1
	startFold logFlags = 2
	endTime   logFlags = 4
	endFold   logFlags = 8
)

func printft(start time.Time, flags logFlags, format string, args ...interface{}) {
	if Logger != nil {
		writeLogt(start, flags, format, args...)
	}
}

func writeLogt(start time.Time, flags logFlags, format string, args ...interface{}) {
	var lines []string
	var content = strings.TrimSpace(pretty.Sprintf(format, args...))
	var multiline = strings.Contains(content, "\n")
	if flags.with(startFold) && (multiline || !flags.with(endFold)) {
		lines = append(lines, travisFoldStart(start))
	}
	if flags.with(startTime) {
		lines = append(lines, travisTimeStart(start))
	}
	if format != "" {
		lines = append(lines, timePrefix()+content)
	}
	if flags.with(endTime) {
		lines = append(lines, travisTimeEnd(start))
	}
	if flags.with(endFold) && (multiline || !flags.with(startFold)) {
		// Dot workarounds a visualization bug in Travis.
		lines = append(lines, travisFoldEnd(start), ".")
	}
	writeLog(lines...)
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

var onTravis = os.Getenv("TRAVIS") == "true"

func travisId(start time.Time) int64 {
	return start.UnixNano() & (1<<32 - 1)
}

func travisTimeStart(start time.Time) string {
	if onTravis {
		return fmt.Sprintf("travis_time:start:%x\n", travisId(start))
	}
	return ""
}

func travisTimeEnd(start time.Time) string {
	if onTravis {
		s := start.UnixNano()
		e := time.Now().UnixNano()
		return fmt.Sprintf("travis_time:end:%x:start=%d,finish=%d,duration=%d\n", travisId(start), s, e, e-s)
	}
	return ""
}

func travisFoldStart(start time.Time) string {
	if onTravis {
		return fmt.Sprintf("travis_fold:start:fold-%x\n", travisId(start))
	}
	return ""
}

func travisFoldEnd(start time.Time) string {
	if onTravis {
		return fmt.Sprintf("travis_fold:end:fold-%x\n", travisId(start))
	}
	return ""
}
