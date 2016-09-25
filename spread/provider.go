package spread

import (
	"fmt"
	"math/rand"
	"time"
)

var rnd = rand.New(rand.NewSource(time.Now().UnixNano()))

type Provider interface {
	Backend() *Backend
	Allocate(system *System) (Server, error)
	Reuse(rsystem *ReuseSystem, system *System) (Server, error)
}

type Server interface {
	Provider() Provider
	Address() string
	Discard() error
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

func (s *UnknownServer) String() string         { return "server " + s.Addr }
func (s *UnknownServer) Provider() Provider     { return nil }
func (s *UnknownServer) Address() string        { return s.Addr }
func (s *UnknownServer) Discard() error         { return nil }
func (s *UnknownServer) ReuseData() interface{} { return nil }
func (s *UnknownServer) System() *System        { return nil }
