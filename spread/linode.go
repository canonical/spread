package spread

import (
	"encoding/json"
	"fmt"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func Linode(b *Backend) Provider {
	return &linode{b}
}

type linode struct {
	backend *Backend
}

var client = &http.Client{}

type linodeServer struct {
	l *linode

	ID     int     `json:"LINODEID"`
	Label  string  `json:"LABEL"`
	Status int     `json:"STATUS" yaml:"-"`
	Addr   string  `json:"-" yaml:"address"`
	Img    ImageID `json:"-" yaml:"image"`
	Config int     `json:"-"`
	Root   int     `json:"-"`
	Swap   int     `json:"-"`
}

func (s *linodeServer) String() string {
	return fmt.Sprintf("%s:%s (%s)", s.l.backend.Name, s.Img.SystemID(), s.Label)
}

func (s *linodeServer) Provider() Provider {
	return s.l
}

func (s *linodeServer) Address() string {
	return s.Addr
}

func (s *linodeServer) Image() ImageID {
	return s.Img
}

func (s *linodeServer) Snapshot() (ImageID, error) {
	return "", nil
}

func (s *linodeServer) ReuseData() []byte {
	data, err := yaml.Marshal(s)
	if err != nil {
		panic(err)
	}
	return data
}

const (
	linodeBeingCreated = -1
	linodeBrandNew     = 0
	linodeRunning      = 1
	linodePoweredOff   = 2
)

type linodeResult struct {
	Errors []linodeError `json:"ERRORARRAY"`
}

type linodeError struct {
	Code    int    `json:"ERRORCODE"`
	Message string `json:"ERRORMESSAGE"`
}

func (r *linodeResult) err() error {
	for _, e := range r.Errors {
		return fmt.Errorf("%s", strings.ToLower(string(e.Message[0]))+e.Message[1:])
	}
	return nil
}

func (l *linode) Backend() *Backend {
	return l.backend
}

func (l *linode) DiscardSnapshot(image ImageID) error {
	return nil
}

func (l *linode) Reuse(data []byte, password string) (Server, error) {
	// TODO Store backend.
	server := &linodeServer{}
	err := yaml.Unmarshal(data, server)
	if err != nil {
		return nil, fmt.Errorf("cannot unmarshal Linode reuse data: %v", err)
	}
	server.l = l
	return server, nil
}

type FatalError struct {
	error
}

func (l *linode) Allocate(image ImageID, password string) (Server, error) {
	servers, err := l.list()
	if err != nil {
		return nil, err
	}
	if len(servers) == 0 {
		return nil, FatalError{fmt.Errorf("no servers in Linode account")}
	}
	for _, server := range servers {
		if server.Status != linodePoweredOff {
			continue
		}
		err := l.setup(server, image, password)
		if err != nil {
			return nil, err
		}
		printf("Allocated %s.", server)
		return server, nil
	}
	return nil, fmt.Errorf("no powered off servers in Linode account")
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *linodeServer) Discard() error {
	logf("Discarding %s...", s)
	_, err1 := s.l.shutdown(s)
	err2 := s.l.removeConfig(s, s.Config)
	err3 := s.l.removeDisks(s, s.Root, s.Swap)
	return firstErr(err1, err2, err3)
}

type linodeListResult struct {
	Data []*linodeServer `json:"DATA"`
}

func (l *linode) list() ([]*linodeServer, error) {
	log("Listing available Linode servers...")
	params := linodeParams{
		"api_action": "linode.list",
	}
	var result linodeListResult
	err := l.do(params, &result)
	if err != nil {
		return nil, err
	}
	return result.Data, nil
}

// setup takes a powered on (reserved) server, and creates a disk and config
// for the given image, and then reboots the server on it.
func (l *linode) setup(server *linodeServer, image ImageID, password string) error {
	server.l = l
	server.Img = image

	ip, err := l.ip(server)
	if err != nil {
		return err
	}
	server.Addr = ip.IPAddress

	rootJob, swapJob, err := l.createDisk(server, image, password)
	if err != nil {
		return err
	}
	server.Root = rootJob.DiskID
	server.Swap = swapJob.DiskID

	configID, err := l.createConfig(server, image, server.Root, server.Swap)
	if err != nil {
		l.removeDisks(server, server.Root, server.Swap)
		return err
	}
	server.Config = configID

	bootJob, err := l.boot(server, configID)
	if err == nil {
		_, err = l.waitJob(server, "boot", bootJob.JobID)
	}
	if err != nil {
		l.removeConfig(server, server.Config)
		l.removeDisks(server, server.Root, server.Swap)
		return err
	}
	return nil
}

type linodeJob struct {
	JobID int `json:"JOBID"`
}

type linodeJobResult struct {
	linodeResult
	Data *linodeJob `json:"DATA"`
}

func (l *linode) boot(server *linodeServer, configID int) (*linodeJob, error) {
	return l.serverJob(server, "reboot", linodeParams{
		"api_action": "linode.reboot",
		"LinodeID":   server.ID,
		"ConfigID":   configID,
	})
}

func (l *linode) reboot(server *linodeServer, configID int) (*linodeJob, error) {
	return l.serverJob(server, "reboot", linodeParams{
		"api_action": "linode.reboot",
		"LinodeID":   server.ID,
		"ConfigID":   configID,
	})
}

func (l *linode) shutdown(server *linodeServer) (*linodeJob, error) {
	return l.serverJob(server, "shutdown", linodeParams{
		"api_action": "linode.shutdown",
		"LinodeID":   server.ID,
	})
}

func (l *linode) serverJob(server *linodeServer, verb string, params linodeParams) (*linodeJob, error) {
	var result linodeJobResult
	err := l.do(params, &result)
	if err == nil {
		err = result.err()
	}
	if err != nil {
		return nil, fmt.Errorf("cannot %s %s: %v", verb, server, err)
	}
	return result.Data, nil
}

type linodeImageInfo struct {
	distroID int
	kernelID int
}

var linodeImageInfos = map[ImageID]*linodeImageInfo{
	Ubuntu1604: {
		distroID: 146,
		kernelID: 138,
	},
}

type linodeDiskJob struct {
	DiskID int `json:"DISKID"`
	JobID  int `json:"JOBID"`
}

type linodeDiskJobResult struct {
	linodeResult
	Data *linodeDiskJob `json:"DATA"`
}

func (l *linode) createDisk(server *linodeServer, image ImageID, password string) (root, swap *linodeDiskJob, err error) {
	info, ok := linodeImageInfos[image]
	if !ok {
		return nil, nil, fmt.Errorf("no Linode image information for %s", image)
	}

	logf("Creating disk on %s with %s...", server, image)
	params := linodeParams{
		"api_action": "batch",
		"api_requestArray": []linodeParams{{
			"api_action":     "linode.disk.createFromDistribution",
			"LinodeID":       server.ID,
			"DistributionID": info.distroID,
			"Label":          image.Label("root"),
			"Size":           4096,
			"rootPass":       password,
		}, {
			"api_action": "linode.disk.create",
			"LinodeID":   server.ID,
			"Label":      image.Label("swap"),
			"Size":       256,
			"Type":       "swap",
		}},
	}

	var results []linodeDiskJobResult
	err = l.do(params, &results)
	for i, result := range results {
		if e := result.err(); e != nil {
			err = e
			break
		}
		if i == 0 {
			root = result.Data
			continue
		}
		swap = result.Data
		return root, swap, nil
	}

	if root != nil {
		l.removeDisks(server, root.DiskID)
	}
	if len(results) == 0 {
		err = fmt.Errorf("empty batch result")
	}
	return nil, nil, fmt.Errorf("cannot create Linode disk with %s: %v", image, err)
}

func (l *linode) removeDisks(server *linodeServer, diskIDs ...int) error {
	logf("Removing disks from %s...", server)
	var batch []linodeParams
	for _, diskID := range diskIDs {
		batch = append(batch, linodeParams{
			"api_action": "linode.disk.delete",
			"LinodeID":   server.ID,
			"DiskID":     diskID,
		})
	}
	params := linodeParams{
		"api_action":       "batch",
		"api_requestArray": batch,
	}
	var results []linodeResult
	err := l.do(params, &results)
	if err != nil {
		return fmt.Errorf("cannot remove disk on %s: %v", server, err)
	}
	for _, result := range results {
		if err := result.err(); err != nil {
			return fmt.Errorf("cannot remove disk on %s: %v", server, err)
		}
	}
	return nil
}

type linodeConfigResult struct {
	linodeResult
	Data struct {
		ConfigID int `json:"CONFIGID"`
	} `json:"DATA"`
}

func (l *linode) createConfig(server *linodeServer, image ImageID, rootID, swapID int) (configID int, err error) {
	logf("Creating configuration on %s with %s...", server, image)

	info, ok := linodeImageInfos[image]
	if !ok {
		return 0, fmt.Errorf("no Linode image information for %s", image)
	}

	params := linodeParams{
		"api_action":             "linode.config.create",
		"LinodeID":               server.ID,
		"KernelID":               info.kernelID,
		"Label":                  image.Label(""),
		"DiskList":               fmt.Sprintf("%d,%d", rootID, swapID),
		"RootDeviceNum":          1,
		"RootDeviceR0":           true,
		"helper_disableUpdateDB": true,
		"helper_distro":          true,
		"helper_depmod":          true,
		"helper_network":         false,
		"devtmpfs_automount":     true,
	}

	var result linodeConfigResult
	err = l.do(params, &result)
	if err == nil {
		err = result.err()
	}
	if err != nil {
		return 0, fmt.Errorf("cannot create config on %s with %s: %v", server, image, err)
	}
	return result.Data.ConfigID, nil
}

func (l *linode) removeConfig(server *linodeServer, configID int) error {
	logf("Removing configuration from %s...", server)

	params := linodeParams{
		"api_action": "linode.config.delete",
		"LinodeID":   server.ID,
		"ConfigID":   configID,
	}
	var result linodeResult
	err := l.do(params, &result)
	if err == nil {
		err = result.err()
	}
	if err != nil {
		return fmt.Errorf("cannot remove config from %s: %v", server, err)
	}
	return nil
}

type linodeJobInfo struct {
	JobID       int    `json:"JOBID"`
	LinodeID    int    `json:"LINODEID"`
	Action      string `json:"ACTION"`
	Label       string `json:"LABEL"`
	HostStart   string `json:"HOST_START_DT"`
	HostFinish  string `json:"HOST_FINISH_DT"`
	HostSuccess int    `json:"HOST_SUCCESS"`
	HostMessage string `json:"HOST_MESSAGE"`
}

func (job *linodeJobInfo) err() error {
	if job.HostSuccess == 1 || job.HostFinish == "" {
		return nil
	}
	if msg := job.HostMessage; msg != "" {
		return fmt.Errorf("%s", strings.ToLower(string(msg[0]))+msg[1:])
	}
	return fmt.Errorf("job %d failed silently", job.JobID)
}

type linodeJobInfoResult struct {
	linodeResult
	Data []*linodeJobInfo `json:"DATA"`
}

func (l *linode) jobInfo(server *linodeServer, jobID int) (*linodeJobInfo, error) {
	params := linodeParams{
		"api_action": "linode.job.list",
		"LinodeID":   server.ID,
		"JobID":      jobID,
	}
	var result linodeJobInfoResult
	err := l.do(params, &result)
	if err == nil {
		err = result.err()
	}
	if err == nil && len(result.Data) == 0 {
		err = fmt.Errorf("empty result")
	}
	if err != nil {
		return nil, fmt.Errorf("cannot get job details for %s: %v", server, err)
	}
	return result.Data[0], nil
}

func (l *linode) waitJob(server *linodeServer, verb string, jobID int) (*linodeJobInfo, error) {
	logf("Waiting for %s to %s...", server, verb)

	timeout := time.After(1 * time.Minute)
	retry := time.NewTicker(5 * time.Second)
	defer retry.Stop()

	var infoErr error
	for {
		select {
		case <-timeout:
			// Don't shutdown. The machine may be running something else.
			if infoErr != nil {
				return nil, infoErr
			}
			l.removeConfig(server, server.Config)
			l.removeDisks(server, server.Root, server.Swap)
			return nil, fmt.Errorf("timeout waiting for %s to %s", server, verb)

		case <-retry.C:
			job, err := l.jobInfo(server, jobID)
			if err != nil {
				infoErr = fmt.Errorf("cannot %s %s: %s", verb, server, err)
				break
			}
			if job.HostFinish != "" {
				err := job.err()
				if err != nil {
					err = fmt.Errorf("cannot %s %s: %s", verb, server, err)
				}
				return job, err
			}
		}
	}
	panic("unreachable")
}

type linodeIPResult struct {
	linodeResult
	Data []*linodeIP `json:"DATA"`
}

type linodeIP struct {
	ID        int    `json:"IPADDRESSID"`
	LinodeID  int    `json:"LINODEID"`
	IsPublic  int    `json:"ISPUBLIC"`
	IPAddress string `json:"IPADDRESS"`
	RDNSName  string `json:"RDNS_NAME"`
}

func (l *linode) ip(server *linodeServer) (*linodeIP, error) {
	logf("Obtaining address of %s...", server)

	params := linodeParams{
		"api_action": "linode.ip.list",
		"LinodeID":   server.ID,
	}
	var result linodeIPResult
	err := l.do(params, &result)
	if err != nil {
		return nil, err
	}
	if err := result.err(); err != nil {
		return nil, fmt.Errorf("cannot list IPs for %s: %v", server, err)
	}
	for _, ip := range result.Data {
		if ip.IsPublic == 1 {
			logf("Got address of %s: %s", server, ip.IPAddress)
			return ip, nil
		}
	}
	return nil, fmt.Errorf("cannot find public IP for %s", server)
}

type linodeParams map[string]interface{}

func (l *linode) do(params linodeParams, result interface{}) error {
	debugf("Linode request: %# v\n", params)

	values := make(url.Values)
	for k, v := range params {
		var vs string
		switch v := v.(type) {
		case int:
			vs = strconv.Itoa(v)
		case string:
			vs = v
		default:
			data, err := json.Marshal(v)
			if err != nil {
				return fmt.Errorf("cannot marshal Linode request parameter %q: %s", k, err)
			}
			vs = string(data)
		}
		values[k] = []string{vs}
	}
	values["api_key"] = []string{l.backend.Key}

	resp, err := client.PostForm("https://api.linode.com", values)
	if err != nil {
		return fmt.Errorf("cannot perform Linode request: %v", err)
	}
	defer resp.Body.Close()

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("cannot read Linode response: %v", err)
	}

	if Debug {
		var r interface{}
		err = json.Unmarshal(data, &r)
		if err != nil {
			return fmt.Errorf("cannot decode Linode response: %v", err)
		}
		debugf("Linode response: %# v\n", r)
	}

	err = json.Unmarshal(data, result)
	if err != nil {
		return fmt.Errorf("cannot decode Linode response: %v", err)
	}
	return nil
}
