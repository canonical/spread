package spread

import (
	"fmt"
	"math/rand"
	"strings"
	"time"
)

var rnd = rand.New(rand.NewSource(time.Now().UnixNano()))

type Provider interface {
	Backend() *Backend
	Allocate(image ImageID, password string) (Server, error)
	Reuse(data []byte, password string) (Server, error)
	DiscardSnapshot(img ImageID) error
}

type Server interface {
	Provider() Provider
	Address() string
	Discard() error
	Image() ImageID
	Snapshot() (ImageID, error)
	ReuseData() []byte
}

type ImageID string

func (img ImageID) SystemID() ImageID {
	if i := strings.Index(string(img), ":"); i >= 0 {
		return img[:i]
	}
	return img
}

func (img ImageID) SnapshotID() string {
	if i := strings.Index(string(img), ":"); i >= 0 {
		return string(img[i+1:])
	}
	return ""
}

func (img ImageID) Snapshot(snapshotID string) ImageID {
	return img.SystemID() + ":" + ImageID(snapshotID)
}

func (img ImageID) Label(note string) string {
	if note != "" {
		note = " (" + note + ")"
	}
	tstr := time.Now().UTC().Format("2006-01-02 15:04")
	return fmt.Sprintf("Test with %s on %s%s", img, tstr, note)
}

type UnknownServer struct {
	Addr string
}

func (s *UnknownServer) String() string             { return "server " + s.Addr }
func (s *UnknownServer) Provider() Provider         { return nil }
func (s *UnknownServer) Address() string            { return s.Addr }
func (s *UnknownServer) Discard() error             { return nil }
func (s *UnknownServer) ReuseData() []byte          { return nil }
func (s *UnknownServer) Image() ImageID             { return "" }
func (s *UnknownServer) Snapshot() (ImageID, error) { return "", nil }
