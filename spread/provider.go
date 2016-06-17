package spread

import (
	"fmt"
	"math/rand"
	"time"
)

var rnd = rand.New(rand.NewSource(time.Now().UnixNano()))

type Provider interface {
	Backend() *Backend
	Allocate(system string, password string, keep bool) (Server, error)
	Reuse(data []byte, password string) (Server, error)
}

type Server interface {
	Provider() Provider
	Address() string
	Discard() error
	System() string
	ReuseData() []byte
	String() string
}

// FatalError represents an error that cannot be fixed by just retrying.
type FatalError struct{ error }

func SystemLabel(system, note string) string {
	if note != "" {
		note = " (" + note + ")"
	}
	tstr := time.Now().UTC().Format("15:04Jan2")
	return fmt.Sprintf("%s %s%s", system, tstr, note)
}

type UnknownServer struct {
	Addr string
}

func (s *UnknownServer) String() string     { return "server " + s.Addr }
func (s *UnknownServer) Provider() Provider { return nil }
func (s *UnknownServer) Address() string    { return s.Addr }
func (s *UnknownServer) Discard() error     { return nil }
func (s *UnknownServer) ReuseData() []byte  { return nil }
func (s *UnknownServer) System() string     { return "" }
