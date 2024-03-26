package spread

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/context"

	"github.com/niemeyer/pretty"
)

func TestFlinger(p *Project, b *Backend, o *Options) Provider {
	return &TestFlingerProvider{p, b, o, &http.Client{}}
}

type TestFlingerProvider struct {
	project *Project
	backend *Backend
	options *Options

	client *http.Client
}

type TestFlingerJob struct {
	p *TestFlingerProvider
	d TestFlingerJobData

	system  *System
	address string
}

type TestFlingerJobData struct {
	Name 	  string
	JobId 	  string    `json:"job_id"`
	CreatedAt time.Time `json:"created_at"`
	JobState  string    `json:"job_state"`
}

type TestFlingerRequestData struct {
	Queue          string                      `json:"job_queue"`
	ProvisionDdata TestFlingerProvisioningData `json:"provision_data"`
	AllocateData   TestFlingerAllocateData     `json:"allocate_data"`
	Tags           []string   				   `json:"tags"`
}

type TestFlingerProvisioningData struct {
	Url    string `json:"url,omitempty"`
	Distro string `json:"distro,omitempty"`
}

type TestFlingerAllocateData struct {
	Allocate bool `json:"allocate"`
}

type TestFlingerJobResponse struct {
	JobId string `json:"job_id"`
}

type TestFlingerJobInfoResponse struct {
	JobId string  `json:"job_id"`
	Tags []string `json:"tags"`
}

type TestFlingerDeviceInfo struct {
	DeviceIP string `json:"device_ip"`
}

type TestFlingerResultResponse struct {
	JobState   string                `json:"job_state"`
	DeviceInfo TestFlingerDeviceInfo `json:"device_info"`
}

type TestFlingerActionData struct {
	Action string `json:"action"`
}

const (
	ALLOCATED = "allocated"
	CANCELLED = "cancelled"
	COMPLETE  = "complete"
	COMPLETED = "completed"
)

func (s *TestFlingerJob) String() string {
	return fmt.Sprintf("%s (%s)", s.system, s.d.Name)
}

func (s *TestFlingerJob) Label() string {
	return s.d.Name
}

func (s *TestFlingerJob) Provider() Provider {
	return s.p
}

func (s *TestFlingerJob) Address() string {
	return s.address
}

func (s *TestFlingerJob) System() *System {
	return s.system
}

func (s *TestFlingerJob) ReuseData() interface{} {
	return &s.d
}

func (s *TestFlingerJob) Discard(ctx context.Context) error {
	data := &TestFlingerActionData{
		Action: "cancel",
	}
	err := s.p.do("POST", "/job/"+s.d.JobId+"/action", data, nil)
	if err != nil {
		return fmt.Errorf("Error discarding job: %v: ", err)
	}
	return nil
}

var testFlingerJobIdExp = regexp.MustCompile("^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$")

func (p *TestFlingerProvider) validJobId(jobId string) bool {
	return testFlingerJobIdExp.MatchString(jobId)
}

func (p *TestFlingerProvider) Backend() *Backend {
	return p.backend
}

func (p *TestFlingerProvider) GarbageCollect() error {
	jobs, err := p.list()
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	haltTimeout := p.backend.HaltTimeout.Duration

	// Iterate over all the running instances
	for _, s := range jobs {
		printf("Checking %s...", s.d.JobId)

		jobTimeout := haltTimeout
		if s.d.JobState == CANCELLED || s.d.JobState == COMPLETE || s.d.JobState == COMPLETED {
			continue
		}

		var result TestFlingerJobInfoResponse
		err := p.do("GET", "/job/" + s.d.JobId , nil, &result)
		if err != nil {
			return fmt.Errorf("cannot get instance info: %v", err)
		}

		// Use specific job timeout if a tag is set with halt-timeout=TIMEOUT
		for _, tag := range result.Tags {
			if strings.HasPrefix(tag, "halt-timeout=") {
				value := strings.SplitAfter(tag, "=")[1]
				d, err := time.ParseDuration(strings.TrimSpace(value))
				if err != nil {
					printf("WARNING: Ignoring bad TestFlinger job %s halt-timeout tag: %q", s.d.JobId, value)
				} else {
					jobTimeout = d
				}
			}
		}
		if jobTimeout == 0 {
			continue
		}

		runningTime := now.Sub(s.d.CreatedAt)
		if runningTime > jobTimeout {
			printf("Job %s exceeds halt-timeout. Shutting it down...", s.d.JobId)
			err := s.Discard(context.Background())
			if err != nil {
				printf("WARNING: Cannot garbage collect %s: %v", s, err)
			}
		}
	}
	return nil
}

func (p *TestFlingerProvider) list() ([]*TestFlingerJob, error) {
	debug("Listing available TestFlinger jobs...")

	var result []TestFlingerJobData
	err := p.do("GET", "/job/search?tags=spread&state=active", nil, &result)
	if err != nil {
		return nil, fmt.Errorf("cannot get instances list: %v", err)
	}

	jobs := make([]*TestFlingerJob, 0, len(result))
	for _, d := range result {
		jobs = append(jobs, &TestFlingerJob{p: p, d: d})
	}

	return jobs, nil
}

func (p *TestFlingerProvider) Reuse(ctx context.Context, rsystem *ReuseSystem, system *System) (Server, error) {
	s := &TestFlingerJob{
		p:       p,
		system:  system,
		address: rsystem.Address,
	}
	err := rsystem.UnmarshalData(&s.d)
	if err != nil {
		return nil, fmt.Errorf("cannot unmarshal TestFlinger reuse data: %v", err)
	}
	return s, nil
}

func (p *TestFlingerProvider) Allocate(ctx context.Context, system *System) (Server, error) {
	s, err := p.requestDevice(ctx, system)
	if err != nil {
		return nil, err
	}
	err = p.waitDeviceBoot(ctx, s)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (p *TestFlingerProvider) requestDevice(ctx context.Context, system *System) (*TestFlingerJob, error) {
	image := system.Image
	queue := TestFlingerQueue(system)

	pdata := TestFlingerProvisioningData{Url: image,}
	// In case the image is a url, then the provisioning data is used with url,
	// otherwise it is used with distro
	_, err := url.ParseRequestURI(image)
	if err != nil {
		pdata = TestFlingerProvisioningData{Distro: image,}
	}

	data := &TestFlingerRequestData{
		Queue: queue,
		ProvisionDdata: pdata,
		AllocateData: TestFlingerAllocateData{
			Allocate: true,
		},
		// Tags used are:
		// 1. spread which is used to find the spread active jobs
		// 2. halt-timeout=DURATION which is used to determine when a running job has to be cancelled
		Tags: []string{"spread", "halt-timeout=" + p.backend.HaltTimeout.Duration.String()},
	}

	var jobRes TestFlingerJobResponse
	err = p.do("POST", "/job", data, &jobRes)

	// First step is to get the job_id running the submit command
	jobId := ""
	if err != nil {
		return nil, fmt.Errorf("Error creating job: %v: ", err)
	}
	if p.validJobId(jobRes.JobId) {
		jobId = jobRes.JobId
		printf("TestFlinger job %s created for system %s", jobId, system.Name)
	} else {
		return nil, fmt.Errorf("Failed to retrieve job id: %s", jobRes.JobId)
	}

	s := &TestFlingerJob{
		p: p,
		d: TestFlingerJobData{
			Name:  system.Name,
			JobId: jobId,
		},
		system: system,
	}
	return s, nil
}

func (p *TestFlingerProvider) waitDeviceBoot(ctx context.Context, s *TestFlingerJob) error {
	printf("Waiting for TestFlinger %s to have an address...", s.d.Name)
	wait_timeout := s.system.WaitTimeout.Duration
	timeout := time.NewTicker(15 * time.Minute)
	if wait_timeout != 0 {
		timeout = time.NewTicker(wait_timeout)
	}
	warn := time.NewTicker(3 * time.Minute)
	retry := time.NewTicker(15 * time.Second)
	defer retry.Stop()
	for {
		var resRes TestFlingerResultResponse
		err := p.do("GET", "/result/"+s.d.JobId, nil, &resRes)

		if err != nil {
			return fmt.Errorf("Error requesting job status: %v", err)
		}
		state := ""
		if resRes.JobState != "" {
			state = resRes.JobState
		} else {
			return fmt.Errorf("Failed to retrieve job state")
		}

		// allocated stated means the ip for the device available
		if state == ALLOCATED {
			if resRes.DeviceInfo.DeviceIP != "" {
				s.address = resRes.DeviceInfo.DeviceIP
				if net.ParseIP(s.address) == nil {
					return fmt.Errorf("Wrong ip format %s: ", s.address)
				}
				printf("Allocated device with ip %s", s.address)
				return nil
			}
		}

		// The job_id is not active anymore
		if state == CANCELLED || state == COMPLETE || state == COMPLETED {
			return fmt.Errorf("Job state is either cancelled or completed")
		}
		
		select {
		case <-retry.C:
		case <-warn.C:
			printf("Job %s for device % s is in state %s", s.d.JobId, s.d.Name, state)
		case <-timeout.C:
			s.Discard(ctx)
			return fmt.Errorf("Wait timeout reached, job discarded")
		}
	}

	return fmt.Errorf("ip address not found")
}

func TestFlingerQueue(system *System) string {
	if system.Queue != "" {
		return system.Queue
	}
	return system.Name
}

func (p *TestFlingerProvider) do(method, subpath string, params interface{}, result interface{}) error {
	var data []byte
	var err error

	if params != nil {
		data, err = json.Marshal(params)
		if err != nil {
			return fmt.Errorf("cannot marshal TestFlinger request parameters: %s", err)
		}
	}

	url := os.Getenv("TF_ENDPOINT")
	version := os.Getenv("TF_API_VERSION")
    if len(url) == 0 {
        url = "https://testflinger.canonical.com"
    }
    if len(version) == 0 {
        version = "v1"
    }
    url += "/" + version + subpath

	// Repeat on 500s. Note that Google's 500s may come in late, as a marshaled error
	// under a different code. See the INTERNAL handling at the end below.
	var resp *http.Response
	var req *http.Request
	var delays = rand.Perm(10)
	for i := 0; i < 10; i++ {
		req, err = http.NewRequest(method, url, bytes.NewBuffer(data))
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
			return fmt.Errorf("cannot perform TestFlinger request: %v", err)
		}
		
		data, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("cannot read TestFlinger response: %v", err)
		}

		if result != nil {
			// Unmarshal even on errors, so the call site has a chance to inspect the data on errors.
			err = json.Unmarshal(data, result)
			if err != nil && resp.StatusCode == 404 {
				return errTestFlingerNotFound
			}
			if err != nil {
				return err
			}
		}

		break
	}

	if err != nil {
		info := pretty.Sprintf("Request:\n-----\n%# v\n-----\nResponse:\n-----\n%s\n-----\n", params, data)
		return fmt.Errorf("cannot decode TestFlinger response (status %d): %s\n%s", resp.StatusCode, err, info)
	}

	return nil
}

var errTestFlingerNotFound = fmt.Errorf("not found")
