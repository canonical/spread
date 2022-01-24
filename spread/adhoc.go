package spread

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/net/context"
)

func AdHoc(p *Project, b *Backend, o *Options) Provider {
	return &adhocProvider{p, b, o}
}

type adhocProvider struct {
	project *Project
	backend *Backend
	options *Options
}

type adhocServer struct {
	p *adhocProvider

	system  *System
	address string
}

func (s *adhocServer) String() string {
	return s.system.String()
}

func (s *adhocServer) Label() string {
	return s.system.String()
}

func (s *adhocServer) Provider() Provider {
	return s.p
}

func (s *adhocServer) Address() string {
	return s.address
}

func (s *adhocServer) System() *System {
	return s.system
}

func (s *adhocServer) ReuseData() interface{} {
	return nil
}

func (s *adhocServer) Discard(ctx context.Context) error {
	_, err := s.p.run(s.p.backend.Discard, s.system, s.address)
	if err != nil {
		return err
	}
	return nil
}

func (p *adhocProvider) Backend() *Backend {
	return p.backend
}

func (p *adhocProvider) GarbageCollect() error {
	return nil
}

func (p *adhocProvider) Reuse(ctx context.Context, rsystem *ReuseSystem, system *System) (Server, error) {
	s := &adhocServer{
		p:       p,
		system:  system,
		address: rsystem.Address,
	}
	return s, nil
}

func (p *adhocProvider) Allocate(ctx context.Context, system *System, id int) (Server, error) {
	result, err := p.run(p.backend.Allocate, system, "")
	if err != nil {
		return nil, err
	}
	addr := result["ADDRESS"]
	if len(strings.TrimSpace(addr)) == 0 {
		return nil, fmt.Errorf("%s allocate must print ADDRESS=<SSH address> to stdout, got: %q", p.backend, addr)
	}

	allAddr := strings.Fields(addr)
	if id >= len(allAddr) {
		return nil, fmt.Errorf("not enough addresses defined for the required workers")
	}
	addr = allAddr[id]

	s := &adhocServer{
		p:       p,
		system:  system,
		address: addr,
	}

	printf("Waiting for %s to make SSH available at %s...", system, addr)
	if err := waitPortUp(ctx, system, s.address); err != nil {
		s.Discard(ctx)
		return nil, fmt.Errorf("cannot connect to %s at %s: %s", s, s.Address(), err)
	}
	printf("Allocated %s.", s)
	return s, nil
}

var resultExp = regexp.MustCompile("(?m)^([A-Z_]+)=(.*)$")

func (p *adhocProvider) run(script string, system *System, address string) (result map[string]string, err error) {
	env := NewEnvironment(
		"SPREAD_BACKEND", p.backend.Name,
		"SPREAD_SYSTEM", system.Name,
		"SPREAD_SYSTEM_USERNAME", system.Username,
		"SPREAD_SYSTEM_PASSWORD", system.Password,
		"SPREAD_SYSTEM_ADDRESS", address,
	)
	if system.Password == "" {
		env.Set("SPREAD_PASSWORD", p.options.Password)
	}
	lscript := localScript{
		script:      script,
		dir:         p.project.Path,
		env:         env,
		warnTimeout: p.backend.WarnTimeout.Duration,
		killTimeout: p.backend.KillTimeout.Duration,
		mode:        traceOutput,
	}
	output, _, err := lscript.run()
	if err != nil {
		return nil, err
	}

	result = make(map[string]string)
	for _, line := range bytes.Split(bytes.TrimSpace(output), []byte{'\n'}) {
		m := commandExp.FindStringSubmatch(string(bytes.TrimSpace(line)))
		if m != nil {
			result[m[1]] = m[2]
		}
	}

	debugf("Allocation results of %s: %# v", system, result)

	fatal := result["FATAL"]
	if fatal != "" {
		return nil, &FatalError{fmt.Errorf("%s", fatal)}
	}
	error := result["ERROR"]
	if error != "" {
		return nil, fmt.Errorf("%s", error)
	}
	return result, nil
}
