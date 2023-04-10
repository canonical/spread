package spread

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/context"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"golang.org/x/oauth2/jwt"

	"github.com/niemeyer/pretty"
	"regexp"
	"strconv"
	"unicode"
)

func Google(p *Project, b *Backend, o *Options) Provider {
	return &googleProvider{
		project: p,
		backend: b,
		options: o,

		imagesCache: make(map[string]*googleImagesCache),
	}
}

type googleProvider struct {
	project *Project
	backend *Backend
	options *Options

	googleProject string
	googleZone    string

	client *http.Client

	mu sync.Mutex

	keyChecked bool
	keyErr     error

	imagesCache map[string]*googleImagesCache
}

type googleServer struct {
	p *googleProvider
	d googleServerData

	system  *System
	address string
}

type googleServerData struct {
	Name    string
	Plan    string    `json:"machineType"`
	Status  string    `yaml:"-"`
	Created time.Time `json:"creationTimestamp"`

	Labels map[string]string `yaml:"-"`
}

func (d *googleServerData) cleanup() {
	if i := strings.LastIndex(d.Plan, "/"); i >= 0 {
		d.Plan = d.Plan[i+1:]
	}
}

func (s *googleServer) String() string {
	if s.system == nil {
		return s.d.Name
	}
	return fmt.Sprintf("%s (%s)", s.system, s.d.Name)
}

func (s *googleServer) Label() string {
	return s.d.Name
}

func (s *googleServer) Provider() Provider {
	return s.p
}

func (s *googleServer) Address() string {
	return s.address
}

func (s *googleServer) System() *System {
	return s.system
}

func (s *googleServer) ReuseData() interface{} {
	return &s.d
}

const (
	googleStaging      = "STAGING"
	googleProvisioning = "PROVISIONING"
	googleRunning      = "RUNNING"
	googleStopping     = "STOPPING"
	googleStopped      = "STOPPED"
	googleSuspending   = "SUSPENDING"
	googleTerminating  = "TERMINATED"

	googlePending = "PENDING"
	googleDone    = "DONE"
)

func (p *googleProvider) Backend() *Backend {
	return p.backend
}

func (p *googleProvider) Reuse(ctx context.Context, rsystem *ReuseSystem, system *System) (Server, error) {
	s := &googleServer{
		p:       p,
		address: rsystem.Address,
		system:  system,
	}
	err := rsystem.UnmarshalData(&s.d)
	if err != nil {
		return nil, fmt.Errorf("cannot unmarshal Google reuse data: %v", err)
	}
	return s, nil
}

func (p *googleProvider) Allocate(ctx context.Context, system *System) (Server, error) {
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

func (s *googleServer) Discard(ctx context.Context) error {
	return s.p.removeMachine(ctx, s)
}

const googleStartupScript = `
echo root:%s | chpasswd

sed -i 's/^\s*#\?\s*\(PermitRootLogin\|PasswordAuthentication\)\>.*/\1 yes/' /etc/ssh/sshd_config
sed -i 's/^PermitRootLogin=/#PermitRootLogin=/g' /etc/ssh/sshd_config.d/* || true
sed -i 's/^PasswordAuthentication=/#PasswordAuthentication=/g' /etc/ssh/sshd_config.d/* || true
test -d /etc/ssh/sshd_config.d && echo -e 'PermitRootLogin=yes\nPasswordAuthentication=yes' > /etc/ssh/sshd_config.d/00-spread.conf

pkill -o -HUP sshd || true

echo '` + googleReadyMarker + `' > /dev/ttyS2
`

const googleReadyMarker = "MACHINE-IS-READY"
const googleNameLayout = "Jan021504.000000"
const googleDefaultPlan = "n1-standard-1"

func googleName() string {
	return strings.ToLower(strings.Replace(time.Now().UTC().Format(googleNameLayout), ".", "-", 1))
}

func googleParseName(name string) (time.Time, error) {
	t, err := time.Parse(googleNameLayout, strings.Replace(name, "-", ".", 1))
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid Google machine name for spread: %s", name)
	}
	return t, nil
}

var googleLabelExp = regexp.MustCompile("^[a-z0-9_-]+$")

func (p *googleProvider) validLabel(label string) bool {
	return len(label) < 64 && googleLabelExp.MatchString(label)
}

type googleInstanceInterfaces struct {
	NetworkInterfaces []struct {
		Network       string
		Subnetwork    string
		NetworkIP     string
		Name          string
		AccessConfigs []struct {
			Type  string
			Name  string
			NatIP string
		}
	}
}

func (p *googleProvider) address(s *googleServer) (string, error) {
	var result googleInstanceInterfaces
	err := p.doz("GET", "/instances/"+s.d.Name, nil, &result)
	if err != nil {
		return "", fmt.Errorf("cannot get IP address for Google server %s: %v", s, err)
	}
	for _, iface := range result.NetworkInterfaces {
		for _, access := range iface.AccessConfigs {
			if access.Type == "ONE_TO_ONE_NAT" {
				return access.NatIP, nil
			}
		}
	}
	return "", fmt.Errorf("cannot find IP address in Google server %s details", s)
}

var googleImageProjects = map[string]string{
	"ubuntu-":   "ubuntu-os-cloud",
	"centos-":   "centos-cloud",
	"debian-":   "debian-cloud",
	"opensuse-": "opensuse-cloud",
	"freebsd-":  "freebsd-org-cloud-dev",
}

type googleImage struct {
	Project string
	Name    string
	Family  string
	Terms   []string
}

var termExp = regexp.MustCompile("[a-z]+|[0-9](?:[0-9.]*[0-9])?")

func toTerms(s string) []string {
	return termExp.FindAllString(strings.ToLower(s), -1)
}

func containsTerms(superset, subset []string) bool {
	j := 0
Outer:
	for _, term := range subset {
		for ; j < len(superset); j++ {
			if term == superset[j] {
				continue Outer
			}
		}
		return false
	}
	return true
}

func (p *googleProvider) image(system *System) (image *googleImage, family bool, err error) {
	var lookupOrder []googleImage

	if i := strings.Index(system.Image, "/"); i >= 0 {
		// If a project was provided, respect it exactly.
		lookupOrder = append(lookupOrder, googleImage{
			Project: system.Image[:i],
			Name:    system.Image[i+1:],
			Family:  system.Image[i+1:],
			Terms:   toTerms(system.Image[i+1:]),
		})
	} else {
		// Otherwise lookup first in backend project.
		terms := toTerms(system.Image)
		lookupOrder = append(lookupOrder, googleImage{
			Project: p.gproject(),
			Name:    system.Image,
			Family:  system.Image,
			Terms:   terms,
		})

		// And then try to find the image on a known project.
		for prefix, project := range googleImageProjects {
			if strings.HasPrefix(system.Image, prefix) {
				lookupOrder = append(lookupOrder, googleImage{
					Project: project,
					Name:    system.Image,
					Family:  system.Image,
					Terms:   terms,
				})
			}
		}
	}

	for _, lookup := range lookupOrder {
		images, err := p.projectImages(lookup.Project)
		if err != nil {
			return nil, false, err
		}

		// First consider exact matches on name.
		for i := range images {
			image := &images[i]
			if image.Name == lookup.Name {
				debugf("Name match: %#v matches %#v", lookup, image)
				return image, false, nil
			}
		}

		// Next consider exact matches on family.
		for i := range images {
			image := &images[i]
			if image.Family == lookup.Family {
				debugf("Family match: %#v matches %#v", lookup, image)
				return image, true, nil
			}
		}

		// Otherwise use term matching.
		for i := range images {
			image := &images[i]
			if containsTerms(image.Terms, lookup.Terms) {
				debugf("Terms match: %#v matches %#v", lookup, image)
				return image, image.Family != "", nil
			}
		}
	}

	var projects []string
	for _, lookup := range lookupOrder {
		projects = append(projects, lookup.Project)
	}
	return nil, false, &FatalError{fmt.Errorf(`cannot find any Google image matching %q on project "%s"`, system.Image, strings.Join(projects, `" or "`))}
}

type googleImagesCache struct {
	mu     sync.Mutex
	ready  bool
	images []googleImage
	err    error
}

func (p *googleProvider) projectImages(project string) ([]googleImage, error) {
	p.mu.Lock()
	cache, ok := p.imagesCache[project]
	if !ok {
		cache = &googleImagesCache{}
		p.imagesCache[project] = cache
	}
	p.mu.Unlock()

	cache.mu.Lock()
	defer cache.mu.Unlock()

	if cache.ready {
		return cache.images, cache.err
	}

	var result struct {
		Items []struct {
			Description string
			Status      string
			Name        string
			Family      string
		}
	}

	err := p.dofl("GET", "/compute/v1/projects/"+project+"/global/images?orderBy=creationTimestamp+desc", nil, &result, noPathPrefix)
	if err == errGoogleNotFound {
	}
	if err != nil {
		return nil, &FatalError{fmt.Errorf("cannot retrieve Google images for project %q: %v", project, err)}
	}

	for _, item := range result.Items {
		cache.images = append(cache.images, googleImage{
			Project: project,
			Name:    item.Name,
			Family:  item.Family,
			Terms:   toTerms(item.Description),
		})
	}

	return cache.images, err
}

func (p *googleProvider) createMachine(ctx context.Context, system *System) (*googleServer, error) {
	debugf("Creating new Google server for %s...", system.Name)

	name := googleName()
	plan := googleDefaultPlan
	if system.Plan != "" {
		plan = system.Plan
	}

	image, family, err := p.image(system)
	if err != nil {
		return nil, err
	}
	var sourceImage string
	if image.Project != p.gproject() {
		sourceImage += "projects/" + image.Project + "/"
	}
	if family {
		sourceImage += "global/images/family/" + image.Family
	} else {
		sourceImage += "global/images/" + image.Name
	}
	debugf("System %s will use Google image %s", system, sourceImage)

	labels := googleParams{
		"spread": "true",
		"owner":  strings.ToLower(username()),
		"reuse":  strconv.FormatBool(p.options.Reuse),
	}
	if p.validLabel(p.options.Password) {
		labels["password"] = p.options.Password
	}

	metadata := []googleParams{{
		"key":   "startup-script",
		"value": fmt.Sprintf(googleStartupScript, p.options.Password),
	}}
	job := os.Getenv("TRAVIS_JOB_ID")
	if job != "" {
		metadata = append(metadata, googleParams{
			"key":   "travis-job",
			"value": fmt.Sprintf(`https://travis-ci.org/%s/jobs/%s`, os.Getenv("TRAVIS_REPO_SLUG"), job),
		})
	}

	diskParams := googleParams{
		"sourceImage": sourceImage,
	}

	if system.Storage == 0 {
		diskParams["diskSizeGb"] = 10
	} else if system.Storage > 0 {
		diskParams["diskSizeGb"] = int(system.Storage / gb)
	}

	minCpuPlatform := "AUTOMATIC"
	if system.CPUFamily != "" {
		minCpuPlatform = system.CPUFamily
	}

	params := googleParams{
		"name":           name,
		"machineType":    "zones/" + p.gzone() + "/machineTypes/" + plan,
		"minCpuPlatform": minCpuPlatform,
		"networkInterfaces": []googleParams{{
			"accessConfigs": []googleParams{{
				"type": "ONE_TO_ONE_NAT",
				"name": "External NAT",
			}},
			"network": "global/networks/default",
		}},
		"disks": []googleParams{{
			"autoDelete":       "true",
			"boot":             "true",
			"type":             "PERSISTENT",
			"initializeParams": diskParams,
		}},
		"metadata": googleParams{
			"items": metadata,
		},
		"labels": labels,
		"tags": googleParams{
			"items": []string{"spread"},
		},
	}

	if system.SecureBoot {
		params["shieldedInstanceConfig"] = googleParams{
			"enableSecureBoot":          true,
			"enableVtpm":                true,
			"enableIntegrityMonitoring": true,
		}
	}

	var op googleOperation
	err = p.doz("POST", "/instances", params, &op)
	if err != nil {
		return nil, &FatalError{fmt.Errorf("cannot allocate new Google server for %s: %v", system.Name, err)}
	}

	s := &googleServer{
		p: p,
		d: googleServerData{
			Name:    name,
			Plan:    plan,
			Status:  googleProvisioning,
			Created: time.Now(),
		},

		system: system,
	}

	_, err = p.waitOperation(ctx, s, "provision", op.Name)
	if err == nil {
		s.address, err = p.address(s)
	}
	if err == nil {
		err = p.waitServerBoot(ctx, s)
	}
	if err == nil {
		err = p.dropStartupScript(s)
	}
	if err != nil {
		if p.removeMachine(ctx, s) != nil {
			return nil, &FatalError{fmt.Errorf("cannot allocate or deallocate (!) new Google server %s: %v", s, err)}
		}
		return nil, &FatalError{fmt.Errorf("cannot allocate new Google server %s: %v", s, err)}
	}

	return s, nil
}

func (p *googleProvider) waitServerBoot(ctx context.Context, s *googleServer) error {
	printf("Waiting for %s to boot at %s...", s, s.address)

	timeout := time.After(3 * time.Minute)
	relog := time.NewTicker(60 * time.Second)
	defer relog.Stop()
	retry := time.NewTicker(5 * time.Second)
	defer retry.Stop()

	var err error
	var marker = []byte(googleReadyMarker)
	var trail []byte
	var result struct {
		Contents string
		Next     string
	}
	result.Next = "0"
	for {
		err = p.doz("GET", fmt.Sprintf("/instances/%s/serialPort?port=3&start=%s", s.d.Name, result.Next), nil, &result)
		if err != nil {
			printf("Cannot get console output for %s: %v", s, err)
			return fmt.Errorf("cannot get console output for %s: %v", s, err)
		}

		trail = append(trail, result.Contents...)
		debugf("Current console buffer for %s:\n-----\n%s\n-----", s, trail)
		if bytes.Contains(trail, marker) {
			return nil
		}
		if i := len(trail) - len(marker); i > 0 {
			trail = append(trail[:0], trail[i:]...)
		}

		select {
		case <-retry.C:
			debugf("Server %s is taking a while to boot...", s)
		case <-relog.C:
			printf("Server %s is taking a while to boot...", s)
		case <-timeout:
			return fmt.Errorf("cannot find ready marker in console output for %s", s)
		case <-ctx.Done():
			return fmt.Errorf("cannot wait for %s to boot: interrupted", s)
		}
	}
	panic("unreachable")
}

func (p *googleProvider) checkLabel(s *googleServer) error {
	_, err := googleParseName(s.d.Name)
	return err
}

type googleInstanceMetadata struct {
	Metadata struct {
		Fingerprint string `json:"fingerprint"`
		Items       []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"items"`
	} `json:"metadata"`
}

func (p *googleProvider) dropStartupScript(s *googleServer) error {
	instance, err := p.metadata(s)
	if err != nil {
		return err
	}
	for i, item := range instance.Metadata.Items {
		if item.Key == "startup-script" {
			instance.Metadata.Items = append(instance.Metadata.Items[:i], instance.Metadata.Items[i+1:]...)
			return p.setMetadata(s, instance)
		}
	}
	return nil
}

func (p *googleProvider) metadata(s *googleServer) (*googleInstanceMetadata, error) {
	var result googleInstanceMetadata
	err := p.doz("GET", "/instances/"+s.d.Name, nil, &result)
	if err != nil {
		return nil, fmt.Errorf("cannot get instance metadata: %v", err)
	}
	return &result, nil
}

func (p *googleProvider) setMetadata(s *googleServer, meta *googleInstanceMetadata) error {
	err := p.doz("POST", "/instances/"+s.d.Name+"/setMetadata", meta.Metadata, nil)
	if err != nil {
		return fmt.Errorf("cannot change instance metadata: %v", err)
	}
	return nil
}

type googleListResult struct {
	Items []googleServerData
}

var googleLabelWarning = true

func (p *googleProvider) list() ([]*googleServer, error) {
	debug("Listing available Google servers...")

	var result googleListResult
	err := p.doz("GET", "/instances", nil, &result)
	if err != nil {
		return nil, fmt.Errorf("cannot get instances list: %v", err)
	}

	servers := make([]*googleServer, 0, len(result.Items))
	for _, d := range result.Items {
		if _, err := googleParseName(d.Name); err != nil {
			if googleLabelWarning {
				googleLabelWarning = false
				printf("WARNING: Some Google servers ignored due to unsafe labels.")
			}
			continue
		}
		servers = append(servers, &googleServer{p: p, d: d})
	}

	return servers, nil
}

func (p *googleProvider) removeMachine(ctx context.Context, s *googleServer) error {
	if err := p.checkLabel(s); err != nil {
		return fmt.Errorf("cannot deallocate Google server %s: %v", s, err)
	}

	var op googleOperation
	err := p.doz("DELETE", "/instances/"+s.d.Name, nil, &op)
	if err != nil {
		return fmt.Errorf("cannot deallocate Google server %s: %v", s, err)
	}

	//_, err = p.waitOperation(ctx, s, "deallocate", op.Name)
	return err
}

func (p *googleProvider) GarbageCollect() error {
	servers, err := p.list()
	if err != nil {
		return err
	}

	now := time.Now()
	haltTimeout := p.backend.HaltTimeout.Duration

	// Iterate over all the running instances
	for _, s := range servers {
		serverTimeout := haltTimeout
		if value, ok := s.d.Labels["halt-timeout"]; ok {
			d, err := time.ParseDuration(strings.TrimSpace(value))
			if err != nil {
				printf("WARNING: Ignoring bad Google server %s halt-timeout label: %q", s, value)
			} else {
				serverTimeout = d
			}
		}

		if serverTimeout == 0 {
			continue
		}

		printf("Checking %s...", s)

		runningTime := now.Sub(s.d.Created)
		if runningTime > serverTimeout {
			printf("Server %s exceeds halt-timeout. Shutting it down...", s)
			err := p.removeMachine(context.Background(), s)
			if err != nil {
				printf("WARNING: Cannot garbage collect %s: %v", s, err)
			}
		}
	}
	return nil
}

type googleOperation struct {
	Name          string
	Zone          string
	OperationType string
	TargetLink    string
	TargetID      string
	Status        string // PENDING, RUNNING or DONE
	StatusMessage string
	User          string // "system" or user email
	Progress      int
	InsertTime    time.Time
	StartTime     time.Time
	EndTime       time.Time
	SelfLink      string

	Error struct {
		Errors []googleOperationError
	}
}

type googleOperationError struct {
	Code     string
	Location string
	Message  string
}

func (op *googleOperation) err() error {
	for _, e := range op.Error.Errors {
		return fmt.Errorf("%s", strings.ToLower(string(e.Message[0]))+e.Message[1:])
	}
	return nil
}

func (p *googleProvider) operation(name string) (*googleOperation, error) {
	var result googleOperation
	err := p.doz("GET", "/operations/"+name, nil, &result)
	if err != nil && result.Name == "" {
		return nil, fmt.Errorf("cannot get operation details: %v", err)
	}
	return &result, nil
}

func (p *googleProvider) waitOperation(ctx context.Context, s *googleServer, verb, opname string) (*googleOperation, error) {
	debugf("Waiting for %s to %s...", s, verb)

	timeout := time.After(3 * time.Minute)
	retry := time.NewTicker(5 * time.Second)
	defer retry.Stop()

	for {
		select {
		case <-timeout:
			return nil, fmt.Errorf("timeout waiting for %s to %s", s, verb)

		case <-retry.C:
			op, err := p.operation(opname)
			if err != nil {
				return nil, fmt.Errorf("cannot %s %s: %s", verb, s, err)
			}
			if op.Status == googleDone {
				err := op.err()
				if err != nil {
					err = fmt.Errorf("cannot %s %s: %s", verb, s, err)
				}
				return op, err
			}
		case <-ctx.Done():
			return nil, fmt.Errorf("cannot %s %s: interrupted", verb, s)
		}
	}
	panic("unreachable")
}

func (p *googleProvider) checkKey() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.keyChecked {
		return p.keyErr
	}

	var err error

	if p.gproject() == googleMissingProject || p.gzone() == googleMissingZone {
		err = fmt.Errorf("location for %q backend must use the <google project>/<compute zone> format", p.backend.Name)
	}

	if err == nil && p.client == nil {
		ctx := context.Background()
		if strings.HasPrefix(p.backend.Key, "{") {
			var cfg *jwt.Config
			cfg, err = google.JWTConfigFromJSON([]byte(p.backend.Key), googleScope)
			if err == nil {
				p.client = oauth2.NewClient(ctx, cfg.TokenSource(ctx))
			}
		} else {
			os.Setenv("GOOGLE_APPLICATION_CREDENTIALS", p.backend.Key)
			p.client, err = google.DefaultClient(context.Background(), googleScope)
		}
	}
	if err == nil {
		err = p.dofl("GET", "/zones", nil, nil, noCheckKey)
	}
	if err != nil {
		err = &FatalError{err}
	}

	p.keyChecked = true
	p.keyErr = err
	return err
}

const (
	googleMissingProject = "MISSING-PROJECT"
	googleMissingZone    = "MISSING-ZONE"
)

func (p *googleProvider) gproject() string {
	if i := strings.Index(p.backend.Location, "/"); i > 0 {
		return p.backend.Location[:i]
	}
	return googleMissingProject
}

func (p *googleProvider) gzone() string {
	if i := strings.Index(p.backend.Location, "/"); i > 0 && i+1 < len(p.backend.Location) {
		return p.backend.Location[i+1:]
	}
	return googleMissingZone
}

const googleScope = "https://www.googleapis.com/auth/cloud-platform"

type googleResult struct {
	Kind  string
	Error struct {
		Code    int
		Message string
		Status  string
		Errors  []googleError
	}
}

type googleError struct {
	Domain  string
	Reason  string
	Message string
}

func polishErrorMessage(msg string) string {
	if len(msg) > 2 {
		if unicode.IsUpper(rune(msg[0])) && unicode.IsLower(rune(msg[1])) {
			msg = strings.ToLower(string(msg[0])) + msg[1:]
		}
		if msg[len(msg)-1] == '.' && unicode.IsLetter(rune(msg[len(msg)-2])) {
			msg = msg[:len(msg)-1]
		}
	}
	return msg
}

func (r *googleResult) err() error {
	if r.Error.Code != 0 || r.Error.Message != "" || len(r.Error.Errors) > 0 {
		if r.Error.Message != "" {
			return fmt.Errorf("%s", polishErrorMessage(r.Error.Message))
		}
		for _, e := range r.Error.Errors {
			if e.Message != "" {
				return fmt.Errorf("%s", polishErrorMessage(e.Message))
			}
		}
		return fmt.Errorf("malformed Google error (code %d, status %q)", r.Error.Code, r.Error.Status)
	}
	return nil
}

type googleParams map[string]interface{}

var googleThrottle = throttle(time.Second / 10)

func (p *googleProvider) doz(method, subpath string, params interface{}, result interface{}) error {
	return p.dozfl(method, subpath, params, result, 0)
}

func (p *googleProvider) dozfl(method, subpath string, params interface{}, result interface{}, flags doFlags) error {
	return p.dofl(method, "/zones/"+p.gzone()+subpath, params, result, flags)
}

func (p *googleProvider) do(method, subpath string, params interface{}, result interface{}) error {
	return p.dofl(method, subpath, params, result, 0)
}

func (p *googleProvider) dofl(method, subpath string, params interface{}, result interface{}, flags doFlags) error {
	if flags&noCheckKey == 0 {
		if err := p.checkKey(); err != nil {
			return err
		}
	}

	log := flags&noLog == 0
	if log {
		debugf("Google request to %s %s with params: %# v\n", method, subpath, params)
	}

	var data []byte
	var err error

	if params != nil {
		data, err = json.Marshal(params)
		if err != nil {
			return fmt.Errorf("cannot marshal Google request parameters: %s", err)
		}
	}

	<-googleThrottle

	url := "https://www.googleapis.com"
	if flags&noPathPrefix == 0 {
		url += "/compute/v1/projects/" + p.gproject() + subpath
	} else {
		url += subpath
	}

	// Repeat on 500s. Note that Google's 500s may come in late, as a marshaled error
	// under a different code. See the INTERNAL handling at the end below.
	var resp *http.Response
	var req *http.Request
	var delays = rand.Perm(10)
	for i := 0; i < 10; i++ {
		req, err = http.NewRequest(method, url, bytes.NewBuffer(data))
		debugf("Google request URL: %s", req.URL)
		if err != nil {
			return &FatalError{fmt.Errorf("cannot create HTTP request: %v", err)}
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err = p.client.Do(req)
		if err == nil && 500 <= resp.StatusCode && resp.StatusCode < 600 {
			time.Sleep(time.Duration(delays[i]) * 250 * time.Millisecond)
			continue
		}

		if err != nil {
			return fmt.Errorf("cannot perform Google request: %v", err)
		}

		data, err = ungzip(ioutil.ReadAll(resp.Body))
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("cannot read Google response: %v", err)
		}

		if log && Debug {
			var r interface{}
			if err := json.Unmarshal(data, &r); err == nil {
				debugf("Google response: %# v\n", r)
			}
		}

		if result != nil {
			// Unmarshal even on errors, so the call site has a chance to inspect the data on errors.
			err = json.Unmarshal(data, result)
			if err != nil && resp.StatusCode == 404 {
				return errGoogleNotFound
			}
		}

		var eresult googleResult
		if jerr := json.Unmarshal(data, &eresult); jerr == nil {
			if eresult.Error.Status == "INTERNAL" && eresult.Error.Code == 500 {
				// Google has broken down like this before:
				// https://paste.ubuntu.com/p/HMvvxNMq9G/
				if i == 0 {
					printf("Google internal error on %s. Retrying a few times...", subpath)
				}
				continue
			}
			if rerr := eresult.err(); rerr != nil {
				return rerr
			}
		}
		break
	}

	if err != nil {
		info := pretty.Sprintf("Request:\n-----\n%# v\n-----\nResponse:\n-----\n%s\n-----\n", params, data)
		return fmt.Errorf("cannot decode Google response (status %d): %s\n%s", resp.StatusCode, err, info)
	}

	return nil
}

var errGoogleNotFound = fmt.Errorf("not found")

func ungzip(data []byte, err error) ([]byte, error) {
	if err != nil || len(data) < 2 || data[0] != 0x1f || data[1] != 0x8b {
		return data, err
	}
	r, err := gzip.NewReader(bytes.NewBuffer(data))
	if err != nil {
		return nil, fmt.Errorf("data seems compressed but corrupted")
	}
	return ioutil.ReadAll(r)
}
