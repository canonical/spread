package spread

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v2"
	"strings"
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
	output, err := p.run(p.backend.Allocate, system, "")
	if err != nil {
		return nil, err
	}

	lines := bytes.Split(bytes.TrimSpace(output), []byte{'\n'})
	addr := string(bytes.TrimSpace(lines[len(lines)-1]))
	if strings.Contains(addr, " ") {
		return nil, fmt.Errorf("%s allocate must print an SSH address as the last line, got: %s", p.backend, addr)
	}

	s := &adhocServer{
		p: p,
		d: adhocServerData{
			System:  system.Name,
			Address: addr,
			Backend: p.backend.Name,
		},
	}

	printf("Waiting for %s to make SSH available...", system)
	if err := waitPortUp(system, s.Address()); err != nil {
		s.Discard()
		return nil, fmt.Errorf("cannot connect to %s at %s: %s", s, s.Address(), err)
	}
	printf("Allocated %s.", s)
	return s, nil
}

func (p *adhocProvider) run(script string, system *System, address string) (output []byte, err error) {
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
	output, _, err = runScript(traceOutput, script, p.project.Path, env, p.backend.WarnTimeout.Duration, p.backend.KillTimeout.Duration)
	if err != nil {
		return nil, err
	}
	return output, nil
}
