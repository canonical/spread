package spread

import (
	"fmt"
	"math/rand"
	"time"

	"golang.org/x/net/context"
)

var rnd = rand.New(rand.NewSource(time.Now().UnixNano()))

type Provider interface {
	Backend() *Backend
	Allocate(ctx context.Context, system *System) (Server, error)
	Reuse(ctx context.Context, rsystem *ReuseSystem, system *System) (Server, error)
}

type Server interface {
	Provider() Provider
	Address() string
	Discard(ctx context.Context) error
	System() *System
	ReuseData() interface{}
	String() string
}

// FatalError represents an error that cannot be fixed by just retrying.
type FatalError struct{ error }

func SystemLabel(system *System, note string) string {
	if note != "" {
		note = " (" + note + ")"
	}
	tstr := time.Now().UTC().Format("15:04Jan2")
	return fmt.Sprintf("%s %s%s", system.Name, tstr, note)
}

type UnknownServer struct {
	Addr string
}

func removedSystem(backend *Backend, sysname string) *System {
	return &System{
		Backend: backend.Name,
		Name:    sysname,
		Image:   sysname,
	}
}
