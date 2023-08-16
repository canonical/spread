package spread

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
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

type TestFlingerServer struct {
	p *TestFlingerProvider
	d TestFlingerServerData

	system  *System
	address string
}

type TestFlingerServerData struct {
	Name  string
	JobId string
}

type TestFlingerJobData struct {
	Queue          string                      `json:"job_queue"`
	ProvisionDdata TestFlingerProvisioningData `json:"provision_data"`
	AllocateData   TestFlingerAllocateData     `json:"allocate_data"`
}

type TestFlingerProvisioningData struct {
	Url string `json:"url"`
}

type TestFlingerAllocateData struct {
	Allocate bool `json:"allocate"`
}

type TestFlingerJobResponse struct {
	JobId string `json:"job_id"`
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
	ALLOCATED = "Allocated"
	CANCELLED = "cancelled"
	COMPLETE  = "complete"
	COMPLETED = "completed"
)

func (s *TestFlingerServer) String() string {
	return fmt.Sprintf("%s (%s)", s.system, s.d.Name)
}

func (s *TestFlingerServer) Label() string {
	return s.d.Name
}

func (s *TestFlingerServer) Provider() Provider {
	return s.p
}

func (s *TestFlingerServer) Address() string {
	return s.address
}

func (s *TestFlingerServer) System() *System {
	return s.system
}

func (s *TestFlingerServer) ReuseData() interface{} {
	return &s.d
}

func (s *TestFlingerServer) Discard(ctx context.Context) error {
	data := &TestFlingerActionData{
		Action: "cancel",
	}
	err := s.p.do("POST", "/job/"+s.d.JobId+"/action", data, nil)
	if err != nil {
		return fmt.Errorf("Error discarding job: %v: ", err)
	}
	return nil
}

func (p *TestFlingerProvider) Backend() *Backend {
	return p.backend
}

func (p *TestFlingerProvider) GarbageCollect() error {
	return nil
}

func (p *TestFlingerProvider) Reuse(ctx context.Context, rsystem *ReuseSystem, system *System) (Server, error) {
	s := &TestFlingerServer{
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

func (p *TestFlingerProvider) requestDevice(ctx context.Context, system *System) (*TestFlingerServer, error) {
	image := system.Image
	queue := TestFlingerQueue(system)

	data := &TestFlingerJobData{
		Queue: queue,
		ProvisionDdata: TestFlingerProvisioningData{
			Url: image,
		},
		AllocateData: TestFlingerAllocateData{
			Allocate: true,
		},
	}

	var jobRes TestFlingerJobResponse
	err := p.do("POST", "/job", data, &jobRes)

	// First step is to get the job_id running the submit command
	jobId := ""
	if err != nil {
		return nil, fmt.Errorf("Error creating job: %v: ", err)
	}
	if jobRes.JobId != "" {
		jobId = jobRes.JobId
		printf("TestFlinger job %s created for system %s", jobId, system.Name)
	} else {
		return nil, fmt.Errorf("Failed to retrieve job id")
	}

	s := &TestFlingerServer{
		p: p,
		d: TestFlingerServerData{
			Name:  system.Name,
			JobId: jobId,
		},
		system: system,
	}
	return s, nil
}

func (p *TestFlingerProvider) waitDeviceBoot(ctx context.Context, s *TestFlingerServer) error {
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

	url := "https://testflinger.canonical.com/v1"
	url += subpath

	// Repeat on 500s. Note that Google's 500s may come in late, as a marshaled error
	// under a different code. See the INTERNAL handling at the end below.
	var resp *http.Response
	var req *http.Request
	var delays = rand.Perm(10)
	for i := 0; i < 10; i++ {
		req, err = http.NewRequest(method, url, bytes.NewBuffer(data))
		debugf("TestFlinger request URL: %s", req.URL)
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
