package spread

import (
	"fmt"
	"math/rand"
	"time"

	"golang.org/x/net/context"
	"regexp"
)

var rnd = rand.New(rand.NewSource(time.Now().UnixNano()))

type Provider interface {
	Backend() *Backend
	Allocate(ctx context.Context, system *System) (Server, error)
	Reuse(ctx context.Context, rsystem *ReuseSystem, system *System) (Server, error)
	GarbageCollect() error
}

type Server interface {
	Provider() Provider
	Address() string
	Discard(ctx context.Context) error
	ReuseData() interface{}
	System() *System
	Label() string
	String() string
}

// FatalError represents an error that cannot be fixed by just retrying.
type FatalError struct{ error }

var (
	labelTimeLayout = "15:04Jan2"
	labelTimeExp    = regexp.MustCompile("[0-9]{1,2}:[0-5][0-9][A-Z][a-z][a-z][0-9]{1,2}")
)

func SystemLabel(system *System, note string) string {
	if note != "" {
		note = " (" + note + ")"
	}
	tstr := time.Now().UTC().Format(labelTimeLayout)
	return fmt.Sprintf("%s %s%s", system.Name, tstr, note)
}

func ParseLabelTime(s string) (time.Time, error) {
	t, err := time.Parse(labelTimeLayout, labelTimeExp.FindString(s))
	if err != nil {
		return time.Time{}, fmt.Errorf("cannot find timestamp in label: %s", s)
	}

	now := time.Now()
	t = t.AddDate(now.Year(), 0, 0)
	if t.After(now) {
		t = t.AddDate(-1, 0, 0)
	}
	return t, nil
}

type UnknownServer struct {
	Addr string
}
