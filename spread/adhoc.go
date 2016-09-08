package spread

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v2"
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
	d adhocServerData
}

type adhocServerData struct {
	Backend string
	System  string
	Address string
}

func (s *adhocServer) String() string {
	return fmt.Sprintf("%s:%s", s.p.backend.Name, s.d.System)
}

func (s *adhocServer) Provider() Provider {
	return s.p
}

func (s *adhocServer) Address() string {
	return s.d.Address
}

func (s *adhocServer) System() *System {
	system := s.p.backend.Systems[s.d.System]
	if system == nil {
		return removedSystem(s.p.backend, s.d.System)
	}
	return system
}

func (s *adhocServer) ReuseData() []byte {
	data, err := yaml.Marshal(&s.d)
	if err != nil {
		panic(err)
	}
	return data
}

func (s *adhocServer) Discard() error {
	_, err := s.p.run(s.p.backend.Discard, s.System(), s.Address())
	if err != nil {
		return err
	}
	return nil
}

func (p *adhocProvider) Backend() *Backend {
	return p.backend
}

func (p *adhocProvider) Reuse(data []byte) (Server, error) {
	s := &adhocServer{}
	err := yaml.Unmarshal(data, &s.d)
	if err != nil {
		return nil, fmt.Errorf("cannot unmarshal adhoc reuse data: %v", err)
	}
	s.p = p
	return s, nil
}

func (p *adhocProvider) Allocate(system *System) (Server, error) {
	result, err := p.run(p.backend.Allocate, system, "")
	if err != nil {
		return nil, err
	}
	addr := result["ADDRESS"]
	if addr == "" || strings.Contains(addr, " ") {
		return nil, fmt.Errorf("%s allocate must print ADDRESS=<SSH address> to stdout, got: %q", p.backend, addr)
	}

	s := &adhocServer{
		p: p,
		d: adhocServerData{
			System:  system.Name,
			Address: addr,
			Backend: p.backend.Name,
		},
	}

	printf("Waiting for %s to make SSH available at %s...", system, addr)
	if err := waitPortUp(system, s.Address()); err != nil {
		s.Discard()
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
	output, _, err := runScript(traceOutput, script, p.project.Path, env, p.backend.WarnTimeout.Duration, p.backend.KillTimeout.Duration)
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

	logf("%s allocation results: %# v", system, result)

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
