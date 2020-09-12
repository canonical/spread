package spread

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/context"

	"github.com/niemeyer/pretty"
)

func Humbox(p *Project, b *Backend, o *Options) Provider {
	return &humboxProvider{
		project: p,
		backend: b,
		options: o,
	}
}

type humboxProvider struct {
	project *Project
	backend *Backend
	options *Options

	account string
	url     string

	mu sync.Mutex

	keyChecked bool
	keyErr     error
}

type humboxServer struct {
	p *humboxProvider
	d humboxServerData

	system  *System
	address string
}

type humboxServerData struct {
	Name  string
	Birth time.Time
	SSH   string
}

func (s *humboxServer) String() string {
	if s.system == nil {
		return s.d.Name
	}
	return fmt.Sprintf("%s (%s)", s.system, s.d.Name)
}

func (s *humboxServer) Label() string {
	return s.d.Name
}

func (s *humboxServer) Provider() Provider {
	return s.p
}

func (s *humboxServer) Address() string {
	return s.address
}

func (s *humboxServer) System() *System {
	return s.system
}

func (s *humboxServer) ReuseData() interface{} {
	return &s.d
}

func (p *humboxProvider) Backend() *Backend {
	return p.backend
}

func (p *humboxProvider) Reuse(ctx context.Context, rsystem *ReuseSystem, system *System) (Server, error) {
	s := &humboxServer{
		p:       p,
		address: rsystem.Address,
		system:  system,
	}
	err := rsystem.UnmarshalData(&s.d)
	if err != nil {
		return nil, fmt.Errorf("cannot unmarshal Humbox reuse data: %v", err)
	}
	return s, nil
}

func (p *humboxProvider) Allocate(ctx context.Context, system *System) (Server, error) {
	if err := p.checkKey(); err != nil {
		return nil, err
	}

	s, err := p.createMachine(ctx, system)
	if err != nil {
		return nil, err
	}

	printf("Allocated %s.", s)
	return s, nil
}

func (s *humboxServer) Discard(ctx context.Context) error {
	return s.p.removeMachine(ctx, s)
}

func (p *humboxProvider) GarbageCollect() error {
	return nil
}

func (p *humboxProvider) createMachine(ctx context.Context, system *System) (*humboxServer, error) {
	debugf("Creating new Humbox server for %s...", system.Name)

	storage := 10
	if p.backend.Storage > 0 {
		storage = int(p.backend.Storage / gb)
	}

	custom := humboxParams{
		"spread":   "true",
		"owner":    strings.ToLower(username()),
		"reuse":    strconv.FormatBool(p.options.Reuse),
		"password": p.options.Password,
	}

	job := os.Getenv("TRAVIS_JOB_ID")
	if job != "" {
		custom["travis"] = fmt.Sprintf(`https://travis-ci.org/%s/jobs/%s`, os.Getenv("TRAVIS_REPO_SLUG"), job)
	}

	params := humboxParams{
		"image":    system.Image,
		"password": p.options.Password,
		"storage":  storage,
		"custom":   custom,
	}

	var result struct {
		Server humboxServerData
	}
	err := p.do("POST", "/v1/servers", params, &result)
	if err != nil {
		return nil, &FatalError{fmt.Errorf("cannot allocate new Humbox server for %s: %v", system.Name, err)}
	}

	s := &humboxServer{
		p: p,
		d: result.Server,

		system:  system,
		address: result.Server.SSH,
	}

	username := system.Username
	password := system.Password
	sshkeyfile := system.SSHKeyFile
	if username == "" {
		username = "root"
	}
	if password == "" {
		password = p.options.Password
	}

	if err := waitServerUp(ctx, s, username, password, sshkeyfile); err != nil {
		if p.removeMachine(ctx, s) != nil {
			return nil, &FatalError{fmt.Errorf("cannot allocate or deallocate (!) new Humbox server %s: %v", s, err)}
		}
		return nil, &FatalError{err}
	}

	return s, nil
}

func (p *humboxProvider) removeMachine(ctx context.Context, s *humboxServer) error {
	err := p.do("DELETE", "/v1/servers/"+s.d.Name, nil, nil)
	if err != nil {
		return fmt.Errorf("cannot deallocate Humbox server %s: %v", s, err)
	}
	return err
}

var humboxLocation = regexp.MustCompile(`^(https?://)([^@]+)@(.+:[0-9]+)$`)

func (p *humboxProvider) checkKey() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.keyChecked {
		return p.keyErr
	}

	var err error

	m := humboxLocation.FindStringSubmatch(p.backend.Location)
	if m == nil {
		err = fmt.Errorf("location for %q backend must use the http(s)://<user>@<hostname>:<port> format", p.backend.Name)
	}

	p.account = m[2]
	p.url = m[1] + m[3]

	err = p.dofl("GET", "/v1/servers", nil, nil, noCheckKey)
	if err != nil {
		err = &FatalError{err}
	}

	p.keyChecked = true
	p.keyErr = err
	return err
}

const (
	humboxMissingAccount = "MISSING-ACCOUNT"
	humboxMissingAddress = "MISSING-ADDRESS"
)

func (p *humboxProvider) haccount() string {
	if i := strings.Index(p.backend.Location, "@"); i > 0 {
		return p.backend.Location[:i]
	}
	return humboxMissingAccount
}

func (p *humboxProvider) haddress() string {
	if i := strings.Index(p.backend.Location, "@"); i > 0 && i+1 < len(p.backend.Location) {
		return p.backend.Location[i+1:]
	}
	return humboxMissingAddress
}

type humboxResult struct {
	Error string
}

func (r *humboxResult) err() error {
	if r.Error != "" {
		return fmt.Errorf("%s", r.Error)
	}
	return nil
}

type humboxParams map[string]interface{}

var humboxThrottle = throttle(time.Second / 10)

func (p *humboxProvider) do(method, subpath string, params interface{}, result interface{}) error {
	return p.dofl(method, subpath, params, result, 0)
}

func (p *humboxProvider) dofl(method, subpath string, params interface{}, result interface{}, flags doFlags) error {
	if flags&noCheckKey == 0 {
		if err := p.checkKey(); err != nil {
			return err
		}
	}

	log := flags&noLog == 0
	if log {
		debugf("Humbox request to %s %s with params: %# v\n", method, subpath, params)
	}

	var data []byte
	var err error

	if params != nil {
		data, err = json.Marshal(params)
		if err != nil {
			return fmt.Errorf("cannot marshal Humbox request parameters: %s", err)
		}
	}

	<-humboxThrottle

	// Repeat on 500s. Comes from Linode logic, not observed on Humbox so far.
	var resp *http.Response
	var req *http.Request
	var delays = rand.Perm(10)
	for i := 0; i < 10; i++ {
		req, err = http.NewRequest(method, p.url+subpath, bytes.NewBuffer(data))
		debugf("Humbox request URL: %s", req.URL)
		if err != nil {
			return &FatalError{fmt.Errorf("cannot create HTTP request: %v", err)}
		}
		req.Header.Set("Content-Type", "application/json")
		req.SetBasicAuth(p.account, p.backend.Key)
		resp, err = client.Do(req)
		if err == nil && 500 <= resp.StatusCode && resp.StatusCode < 600 {
			time.Sleep(time.Duration(delays[i]) * 250 * time.Millisecond)
			continue
		}
		break
	}
	if err != nil {
		return fmt.Errorf("cannot perform Humbox request: %v", err)
	}
	defer resp.Body.Close()

	data, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("cannot read Humbox response: %v", err)
	}

	if log && Debug {
		var r interface{}
		if err := json.Unmarshal(data, &r); err == nil {
			debugf("Humbox response: %# v\n", r)
		}
	}

	if result != nil {
		// Unmarshal even on errors, so the call site has a chance to inspect the data on errors.
		err = json.Unmarshal(data, result)
		if err != nil && resp.StatusCode == 404 {
			return humboxNotFound
		}
	}

	var eresult humboxResult
	if jerr := json.Unmarshal(data, &eresult); jerr == nil {
		if rerr := eresult.err(); rerr != nil {
			return rerr
		}
	}

	if err != nil {
		info := pretty.Sprintf("Request:\n-----\n%# v\n-----\nResponse:\n-----\n%s\n-----\n", params, data)
		return fmt.Errorf("cannot decode Humbox response (status %d): %s\n%s", resp.StatusCode, err, info)
	}

	return nil
}

var humboxNotFound = fmt.Errorf("not found")
