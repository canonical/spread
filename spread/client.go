package spread

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	// used instead of just importing "context" for compatibility
	// with go1.6 which is used in the xenial autopkgtests
	"golang.org/x/net/context"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/terminal"
	"net"
	"regexp"
	"strconv"
	"syscall"
)

var sshDial = ssh.Dial

type Client struct {
	server Server
	sshc   *ssh.Client
	config *ssh.ClientConfig
	addr   string
	job    string

	warnTimeout time.Duration
	killTimeout time.Duration
}

func Dial(server Server, username, password string) (*Client, error) {
	config := &ssh.ClientConfig{
		User:            username,
		Auth:            []ssh.AuthMethod{ssh.Password(password)},
		Timeout:         10 * time.Second,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}
	addr := server.Address()
	if !strings.Contains(addr, ":") {
		addr += ":22"
	}
	sshc, err := sshDial("tcp", addr, config)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to %s: %v", server, err)
	}
	client := &Client{
		server: server,
		sshc:   sshc,
		config: config,
		addr:   addr,
	}
	client.SetWarnTimeout(0)
	client.SetKillTimeout(0)
	client.SetJob("")
	return client, nil
}

func (c *Client) SetJob(job string) {
	if job == "" {
		c.job = c.server.String()
	} else {
		c.job = fmt.Sprintf("%s (%s)", c.server.Label(), job)
	}
}

func (c *Client) ResetJob() {
	c.SetJob("")
}

func (c *Client) dialOnReboot(prevUptime time.Time) error {
	// First wait until SSH isn't working anymore.
	timeout := time.After(c.killTimeout)
	relog := time.NewTicker(c.warnTimeout)
	defer relog.Stop()
	retry := time.NewTicker(200 * time.Millisecond)
	defer retry.Stop()

	waitConfig := *c.config
	waitConfig.Timeout = 5 * time.Second
	uptimeChanged := 3 * time.Second

	for {
		// Try to connect to the rebooting system, note that
		// waitConfig is not well honored by golang, it is
		// set to 5sec above but in reality it takes ~60sec
		// before the code times out.
		sshc, err := ssh.Dial("tcp", c.addr, &waitConfig)
		if err == nil {
			// once successfully connected, check uptime to
			// see if the reboot actually happend
			c.sshc.Close()
			c.sshc = sshc
			currUptime, err := c.getUptime()
			if err == nil {
				uptimeDelta := currUptime.Sub(prevUptime)
				if uptimeDelta > uptimeChanged {
					// Reboot done
					return nil
				}
			}
		}

		// Use multiple selects to ensure that the channels get
		// checked in the right order. If a single select is used
		// and all channels have data golang will pick a random
		// channel. This means that on timeout there is a 1/2 chance
		// that there is also a retry and ssh.Dial() is run again
		// which needs to timeout first before the channels are
		// checked again.
		select {
		case <-timeout:
			return fmt.Errorf("kill-timeout reached after %s reboot request", c.job)
		default:
		}
		select {
		case <-relog.C:
			printf("Reboot on %s is taking a while...", c.job)
		default:
		}
		select {
		case <-retry.C:
		}
	}

	return nil
}

func (c *Client) Close() error {
	return c.sshc.Close()
}

func (c *Client) Server() Server {
	return c.server
}

func (c *Client) SetWarnTimeout(timeout time.Duration) {
	if timeout == 0 {
		timeout = defaultWarnTimeout
	} else if timeout == -1 {
		timeout = maxTimeout
	}
	c.warnTimeout = timeout

	if c.killTimeout%c.warnTimeout == 0 {
		// So message from kill won't race with warning.
		c.killTimeout -= 1 * time.Second
	}
}

func (c *Client) SetKillTimeout(timeout time.Duration) {
	if timeout == 0 {
		timeout = defaultKillTimeout
	} else if timeout == -1 {
		timeout = maxTimeout
	}
	c.killTimeout = timeout

	if c.killTimeout%c.warnTimeout == 0 {
		// So message from kill won't race with warning.
		c.killTimeout -= 1 * time.Second
	}
}

func (c *Client) WriteFile(path string, data []byte) error {
	session, err := c.sshc.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	stdin, err := session.StdinPipe()
	if err != nil {
		return err
	}
	defer stdin.Close()

	errch := make(chan error, 2)
	go func() {
		_, err := stdin.Write(data)
		if err != nil {
			errch <- err
		}
		errch <- stdin.Close()
	}()

	debugf("Writing to %s on %s:\n-----\n%# v\n-----", path, c.job, string(data))

	var stderr safeBuffer
	session.Stderr = &stderr
	cmd := fmt.Sprintf(`%s/bin/bash -c "cat >'%s'"`, c.sudo(), path)
	err = c.runCommand(session, cmd, nil, &stderr)
	if err != nil {
		err = outputErr(stderr.Bytes(), err)
		return fmt.Errorf("cannot write to %s on %s: %v", path, c.job, err)
	}

	if err := <-errch; err != nil {
		printf("Error writing to %s on %s: %v", path, c.job, err)
	}
	return nil
}

func (c *Client) ReadFile(path string) ([]byte, error) {
	session, err := c.sshc.NewSession()
	if err != nil {
		return nil, err
	}
	defer session.Close()

	debugf("Reading from %s on %s...", path, c.job)

	var stdout, stderr safeBuffer
	session.Stdout = &stdout
	session.Stderr = &stderr
	cmd := fmt.Sprintf(`%scat "%s"`, c.sudo(), path)
	err = c.runCommand(session, cmd, nil, &stderr)
	if err != nil {
		err = outputErr(stderr.Bytes(), err)
		logf("Cannot read from %s on %s: %v", path, c.job, err)
		return nil, fmt.Errorf("cannot read from %s on %s: %v", path, c.job, err)
	}
	output := stdout.Bytes()
	debugf("Got data from %s on %s:\n-----\n%# v\n-----", path, c.job, string(output))
	return output, nil
}

type outputMode int

const (
	traceOutput outputMode = iota
	combinedOutput
	splitOutput
	shellOutput
)

func (c *Client) Run(script string, dir string, env *Environment) error {
	_, err := c.run(script, dir, env, combinedOutput)
	return err
}

func (c *Client) Output(script string, dir string, env *Environment) (output []byte, err error) {
	return c.run(script, dir, env, splitOutput)
}

func (c *Client) CombinedOutput(script string, dir string, env *Environment) (output []byte, err error) {
	return c.run(script, dir, env, combinedOutput)
}

func (c *Client) Trace(script string, dir string, env *Environment) (output []byte, err error) {
	return c.run(script, dir, env, traceOutput)
}

func (c *Client) Shell(script string, dir string, env *Environment) error {
	_, err := c.run(script, dir, env, shellOutput)
	return err
}

type rebootError struct {
	Key string
}

func (e *rebootError) Error() string { return "reboot requested" }

const maxReboots = 10

func (c *Client) run(script string, dir string, env *Environment, mode outputMode) (output []byte, err error) {
	if env == nil {
		env = NewEnvironment()
	}
	rebootKey := ""
	for reboot := 0; ; reboot++ {
		if rebootKey == "" {
			rebootKey = strconv.Itoa(reboot)
		}
		env.Set("SPREAD_REBOOT", rebootKey)
		output, err = c.runPart(script, dir, env, mode, output)
		rerr, ok := err.(*rebootError)
		if !ok {
			return output, err
		}
		if reboot > maxReboots {
			return nil, fmt.Errorf("rebooted on %s more than %d times", c.job, maxReboots)
		}

		printf("Rebooting on %s as requested...", c.job)

		rebootKey = rerr.Key
		output = append(output, '\n')

		uptime, err := c.getUptime()
		if err != nil {
			return nil, err
		}
		c.Run("reboot", "", nil)

		if err := c.dialOnReboot(uptime); err != nil {
			return nil, err
		}
	}
	panic("unreachable")
}

func (c *Client) getUptime() (time.Time, error) {
	uptime, err := c.Output("date -u -d \"$(cut -f1 -d. /proc/uptime) seconds ago\" +\"%Y-%m-%dT%H:%M:%SZ\"", "", nil)
	if err != nil {
		return time.Time{}, fmt.Errorf("cannot obtain the remote system uptime: %v", err)
	}

	parsedUptime, err := time.Parse(time.RFC3339, string(uptime))
	if err != nil {
		return time.Time{}, fmt.Errorf("cannot parse the remote system uptime: %q", uptime)
	}

	return parsedUptime, nil
}

var toBashRC = map[string]bool{
	"PS1":            true,
	"SPREAD_PATH":    true,
	"SPREAD_BACKEND": true,
	"SPREAD_SYSTEM":  true,
}

func (c *Client) runPart(script string, dir string, env *Environment, mode outputMode, previous []byte) (output []byte, err error) {
	script = strings.TrimSpace(script)
	if len(script) == 0 && mode != shellOutput {
		return nil, nil
	}
	script += "\n"
	session, err := c.sshc.NewSession()
	if err != nil {
		return nil, err
	}
	defer session.Close()

	var buf bytes.Buffer
	buf.WriteString("set -eu\n")
	var rc = func(use bool, s string) string { return s }
	if mode == shellOutput {
		buf.WriteString("true > /root/.bashrc\n")
		rc = func(use bool, s string) string {
			if !use {
				return ""
			}
			return "cat >> /root/.bashrc <<'END'\n" + s + "END\n"
		}
	}
	if dir != "" {
		buf.WriteString(fmt.Sprintf("cd \"%s\"\n", dir))
	}
	if c.sudo() != "" {
		buf.WriteString("unset SUDO_COMMAND\n")
		buf.WriteString("unset SUDO_USER\n")
		buf.WriteString("unset SUDO_UID\n")
		buf.WriteString("unset SUDO_GID\n")
	}
	buf.WriteString(rc(false, "REBOOT() { { set +xu; } 2> /dev/null; [ -z \"$1\" ] && echo '<REBOOT>' || echo \"<REBOOT $1>\"; exit 213; }\n"))
	buf.WriteString(rc(false, "ERROR() { { set +xu; } 2> /dev/null; [ -z \"$1\" ] && echo '<ERROR>' || echo \"<ERROR $@>\"; exit 213; }\n"))
	// We are not using pipes here, see:
	//  https://github.com/snapcore/spread/pull/64
	// We also run it in a subshell, see
	//  https://github.com/snapcore/spread/pull/67
	buf.WriteString(rc(true, "MATCH() ( { set +xu; } 2> /dev/null; [ ${#@} -gt 0 ] || { echo \"error: missing regexp argument\"; return 1; }; local stdin=\"$(cat)\"; grep -q -E \"$@\" <<< \"$stdin\" || { res=$?; echo \"grep error: pattern not found, got:\n$stdin\">&2; if [ $res != 1 ]; then echo \"unexpected grep exit status: $res\"; fi; return 1; }; )\n"))
	buf.WriteString(rc(true, "NOMATCH() ( { set +xu; } 2> /dev/null; [ ${#@} -gt 0 ] || { echo \"error: missing regexp argument\"; return 1; }; local stdin=\"$(cat)\"; if echo \"$stdin\" | grep -q -E \"$@\"; then echo \"NOMATCH pattern='$@' found in:\n$stdin\">&2; return 1; fi; )\n"))
	buf.WriteString("export DEBIAN_FRONTEND=noninteractive\n")
	buf.WriteString("export DEBIAN_PRIORITY=critical\n")
	buf.WriteString("export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/snap/bin\n")

	for _, k := range env.Keys() {
		v := env.Get(k)
		var kv string
		if len(v) == 0 || v[0] == '"' || v[0] == '\'' {
			kv = fmt.Sprintf("export %s=%s\n", k, v)
		} else {
			kv = fmt.Sprintf("export %s=\"%s\"\n", k, v)
		}
		if toBashRC[k] {
			kv = rc(true, kv)
		}
		buf.WriteString(kv)
	}

	// Don't trace environment variables so secrets don't leak.
	if mode == traceOutput {
		buf.WriteString("set -x\n")
	}

	if mode == shellOutput {
		buf.WriteString("\n/bin/bash\n")
	} else {
		// Prevent any commands attempting to read from stdin to consume
		// the shell script itself being sent to bash via its stdin.
		fmt.Fprintf(&buf, "\n(\n%s\n) < /dev/null\n", script)
	}

	errch := make(chan error, 2)
	if mode == shellOutput {
		session.Stdin = os.Stdin
		errch <- nil
	} else {
		stdin, err := session.StdinPipe()
		if err != nil {
			return nil, err
		}
		defer stdin.Close()

		go func() {
			_, err := stdin.Write(buf.Bytes())
			if err != nil {
				errch <- err
			}
			errch <- stdin.Close()
		}()
	}

	debugf("Sending script for %s:\n-----\n%s\n------", c.job, buf.Bytes())

	var stdout, stderr safeBuffer
	var cmd string
	switch mode {
	case traceOutput, combinedOutput:
		cmd = c.sudo() + "/bin/bash - 2>&1"
		session.Stdout = &stdout
	case splitOutput:
		cmd = c.sudo() + "/bin/bash -"
		session.Stdout = &stdout
		session.Stderr = &stderr
	case shellOutput:
		cmd = fmt.Sprintf("{\nf=$(mktemp)\ntrap 'rm '$f EXIT\ncat > $f <<'SCRIPT_END'\n%s\nSCRIPT_END\n%s/bin/bash $f\n}", buf.String(), c.sudo())
		session.Stdout = os.Stdout
		session.Stderr = os.Stderr
		w, h, err := terminal.GetSize(0)
		if err != nil {
			return nil, fmt.Errorf("cannot get local terminal size: %v", err)
		}
		if err := session.RequestPty(getenv("TERM", "vt100"), h, w, nil); err != nil {
			return nil, fmt.Errorf("cannot get remote pseudo terminal: %v", err)
		}
	default:
		panic("internal error: invalid output mode")
	}

	if mode == shellOutput {
		termLock()
		tstate, terr := terminal.MakeRaw(0)
		if terr != nil {
			return nil, fmt.Errorf("cannot put local terminal in raw mode: %v", terr)
		}
		err = session.Run(cmd)
		terminal.Restore(0, tstate)
		termUnlock()
	} else {
		err = c.runCommand(session, cmd, &stdout, &stderr)
	}

	if stdout.Len() > 0 {
		debugf("Output from running script on %s:\n-----\n%s\n-----", c.job, stdout.Bytes())
	}
	if stderr.Len() > 0 {
		debugf("Error output from running script on %s:\n-----\n%s\n-----", c.job, stderr.Bytes())
	}

	if e, ok := err.(*ssh.ExitError); ok && e.ExitStatus() == 213 {
		lines := bytes.Split(bytes.TrimSpace(stdout.Bytes()), []byte{'\n'})
		m := commandExp.FindSubmatch(lines[len(lines)-1])
		if len(m) > 0 && string(m[1]) == "REBOOT" {
			return append(previous, stdout.Bytes()...), &rebootError{string(m[2])}
		}
		if len(m) > 0 && string(m[1]) == "ERROR" {
			return nil, fmt.Errorf("%s", m[2])
		}
	}

	if err == nil || mode != splitOutput {
		output = stdout.Bytes()
	} else if mode == splitOutput {
		output = stderr.Bytes()
	}

	// When running scripts under Go's non-shell ssh session, this fails:
	// # echo echo ok | /bin/bash -c "/bin/bash --login" 2>&1 | cat
	const errmsg = "mesg: ttyname failed: Inappropriate ioctl for device"
	output = bytes.TrimSpace(bytes.TrimPrefix(output, []byte(errmsg)))

	output = append(previous, output...)
	if err != nil {
		return nil, outputErr(output, err)
	}
	if err := <-errch; err != nil {
		printf("Error writing script for %s: %v", c.job, err)
	}
	return output, nil
}

func (c *Client) sudo() string {
	if c.config.User == "root" {
		return ""
	}
	return "sudo -i "
}

func getenv(name, defaultValue string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return defaultValue
}

func (c *Client) RemoveAll(path string) error {
	_, err := c.CombinedOutput(fmt.Sprintf(`rm -rf "%s"`, path), "", nil)
	return err
}

func (c *Client) SetupRootAccess(password string) error {
	var script string
	if c.config.User == "root" {
		script = fmt.Sprintf(`echo root:'%s' | chpasswd`, password)
	} else {
		script = strings.Join([]string{
			`sudo sed -i 's/^\s*#\?\s*\(PermitRootLogin\|PasswordAuthentication\)\>.*/\1 yes/' /etc/ssh/sshd_config`,
			`echo root:'` + password + `' | sudo chpasswd`,
			`sudo pkill -o -HUP sshd || true`,
		}, "\n")
	}
	_, err := c.CombinedOutput(script, "", nil)
	if err != nil {
		return fmt.Errorf("cannot setup root access: %s", err)
	}
	if c.config.User == "root" {
		c.config.Auth = []ssh.AuthMethod{ssh.Password(password)}
	}
	return nil
}

func (c *Client) MissingOrEmpty(dir string) (bool, error) {
	output, err := c.Output(fmt.Sprintf(`! test -e "%s" || ls -a "%s"`, dir, dir), "", nil)
	if err != nil {
		return false, fmt.Errorf("cannot check if %s on %s is empty: %v", dir, c.job, err)
	}
	output = bytes.TrimSpace(output)
	if len(output) > 0 {
		for _, s := range strings.Split(string(output), "\n") {
			if s != "." && s != ".." {
				debugf("Found %q inside %q on %s, considering non-empty.", s, dir, c.job)
				return false, nil
			}
		}
	}
	return true, nil
}

func (c *Client) Send(from, to string, include, exclude []string) error {
	empty, err := c.MissingOrEmpty(to)
	if err != nil {
		return err
	}
	if !empty {
		return fmt.Errorf("remote directory %s on %s is not empty", to, c.job)
	}

	session, err := c.sshc.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	stdin, err := session.StdinPipe()
	if err != nil {
		return err
	}
	defer stdin.Close()

	args := []string{
		"-cz",
		"--exclude=.spread-reuse.*",
	}
	for _, pattern := range exclude {
		args = append(args, "--exclude="+pattern)
	}
	for _, pattern := range include {
		args = append(args, pattern)
	}

	var stderr bytes.Buffer

	cmd := exec.Command("tar", args...)
	cmd.Dir = from
	cmd.Stdout = stdin
	cmd.Stderr = &stderr
	err = cmd.Start()
	if err != nil {
		return fmt.Errorf("cannot start local tar command: %v", err)
	}

	errch := make(chan error, 1)
	go func() {
		errch <- cmd.Wait()
		stdin.Close()
	}()

	var stdout safeBuffer
	session.Stdout = &stdout
	rcmd := fmt.Sprintf(`%s/bin/bash -c "mkdir -p '%s' && cd '%s' && /bin/tar -xz 2>&1"`, c.sudo(), to, to)
	err = c.runCommand(session, rcmd, &stdout, nil)
	if err != nil {
		return outputErr(stdout.Bytes(), err)
	}

	if err := <-errch; err != nil {
		return fmt.Errorf("local tar command returned error: %v", outputErr(stderr.Bytes(), err))
	}
	return nil
}

func (c *Client) SendTar(tar io.Reader, unpackDir string) error {
	empty, err := c.MissingOrEmpty(unpackDir)
	if err != nil {
		return err
	}
	if !empty {
		return fmt.Errorf("remote directory %s on %s is not empty", unpackDir, c.job)
	}

	session, err := c.sshc.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	var stdout safeBuffer
	session.Stdin = tar
	session.Stdout = &stdout
	cmd := fmt.Sprintf(`%s/bin/bash -c "mkdir -p '%s' && cd '%s' && /bin/tar xz 2>&1"`, c.sudo(), unpackDir, unpackDir)
	err = c.runCommand(session, cmd, &stdout, nil)
	if err != nil {
		return outputErr(stdout.Bytes(), err)
	}
	return nil
}

func (c *Client) RecvTar(packDir string, include []string, tar io.Writer) error {
	session, err := c.sshc.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	var args []string
	if len(include) == 0 {
		args = []string{"."}
	} else {
		args = make([]string, len(include))
		for i, arg := range include {
			arg = strings.Replace(arg, "'", `'"'"'`, -1)
			arg = strings.Replace(arg, "*", `'*'`, -1)
			args[i] = "'" + arg + "'"
		}
	}

	var stderr safeBuffer
	session.Stdout = tar
	session.Stderr = &stderr
	cmd := fmt.Sprintf(`cd '%s' && %s/bin/tar cJ --sort=name --ignore-failed-read -- %s`, packDir, c.sudo(), strings.Join(args, " "))
	err = c.runCommand(session, cmd, nil, &stderr)
	if err != nil {
		return outputErr(stderr.Bytes(), err)
	}
	return nil
}

const (
	defaultWarnTimeout = 5 * time.Minute
	defaultKillTimeout = 15 * time.Minute
	maxTimeout         = 365 * 24 * time.Hour
)

func (c *Client) runCommand(session *ssh.Session, cmd string, stdout, stderr io.Writer) error {
	start := time.Now()

	err := session.Start(cmd)
	if err != nil {
		return fmt.Errorf("cannot start remote command on %s: %v", c.job, err)
	}

	done := make(chan error)
	go func() {
		done <- session.Wait()
	}()

	var lastOut, lastErr int

	kill := time.After(c.killTimeout)
	warn := time.NewTicker(c.warnTimeout)
	defer warn.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-kill:
			session.Signal(ssh.SIGKILL)
			out := stdout
			if out == nil {
				out = stderr
			}
			if out != nil {
				out.Write([]byte("\n<kill-timeout reached>"))
			}
			return fmt.Errorf("kill-timeout reached")
		case <-warn.C:
			var output, errput []byte
			if buf, ok := stdout.(*safeBuffer); ok {
				output, lastOut = buf.Since(lastOut)
			}
			if buf, ok := stderr.(*safeBuffer); ok {
				errput, lastErr = buf.Since(lastErr)
				if len(output) == 0 || bytes.HasPrefix(errput, output) {
					// Also avoids double (... same ...) message.
					output = errput
				} else if len(errput) > 0 {
					output = append(output, '\n', '\n')
					output = append(output, errput...)
				}
			}
			// Use a different time so it has a different id on Travis, but keep
			// the original start time so the message shows the task time so far.
			start = start.Add(1)
			if bytes.Equal(output, unchangedMarker) {
				printft(start, startTime|endTime, "WARNING: %s running late. Output unchanged.", c.job)
			} else if len(output) == 0 {
				printft(start, startTime|endTime, "WARNING: %s running late. Output still empty.", c.job)
			} else {
				printft(start, startTime|endTime|startFold|endFold, "WARNING: %s running late. Current output:\n-----\n%s\n-----", c.job, tail(output))
			}
		}
	}
	panic("unreachable")
}

func tail(output []byte) []byte {
	display := 10
	min := display
	max := display + 3
	mark := 0
	for i := len(output) - 1; i >= 0; i-- {
		if output[i] != '\n' {
			continue
		}

		min--
		max--

		if min == 0 {
			mark = i + 1
			continue
		}
		if max == 0 {
			var buf bytes.Buffer
			fmt.Fprintf(&buf, "(... %d lines above ...)\n%s", bytes.Count(output, []byte{'\n'})-display, output[mark:])
			return buf.Bytes()
		}
	}
	return output
}

var commandExp = regexp.MustCompile("^<([A-Z_]+)(?: (.*))?>$")

// localScript holds and runs a local script in a polished manner.
//
// It's not used by the SSH client, but mimics the Client.runPart+runCommand closely.
type localScript struct {
	script      string
	dir         string
	env         *Environment
	warnTimeout time.Duration
	killTimeout time.Duration
	mode        outputMode
	extraFiles  []*os.File
	stop        <-chan struct{}
}

func (s *localScript) run() (stdout, stderr []byte, err error) {
	script := strings.TrimSpace(s.script)
	if len(script) == 0 {
		return nil, nil, nil
	}
	script += "\n"

	var buf bytes.Buffer
	buf.WriteString("set -eu\n")
	buf.WriteString("ADDRESS() { { set +xu; } 2> /dev/null; [ -z \"$1\" ] && echo '<ADDRESS>' || echo \"<ADDRESS $1>\"; }\n")
	buf.WriteString("FATAL() { { set +xu; } 2> /dev/null; [ -z \"$1\" ] && echo '<FATAL>' || echo \"<FATAL $@>\"; exit 213; }\n")
	buf.WriteString("ERROR() { { set +xu; } 2> /dev/null; [ -z \"$1\" ] && echo '<ERROR>' || echo \"<ERROR $@>\"; exit 213; }\n")
	buf.WriteString("MATCH() { { set +xu; } 2> /dev/null; local stdin=$(cat); echo $stdin | grep -q -E \"$@\" || { echo \"error: pattern not found on stdin:\\n$stdin\">&2; return 1; }; }\n")
	buf.WriteString("NOMATCH() { { set +xu; } 2> /dev/null; local stdin=$(cat); if echo $stdin | grep -q -E \"$@\"; then echo \"NOMATCH pattern='$@' found in:\n$stdin\">&2; return 1; fi }\n")
	buf.WriteString("export DEBIAN_FRONTEND=noninteractive\n")
	buf.WriteString("export DEBIAN_PRIORITY=critical\n")
	buf.WriteString("export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/snap/bin\n")

	for _, k := range s.env.Keys() {
		v := s.env.Get(k)
		if len(v) == 0 || v[0] == '"' || v[0] == '\'' {
			fmt.Fprintf(&buf, "export %s=%s\n", k, v)
		} else {
			fmt.Fprintf(&buf, "export %s=\"%s\"\n", k, v)
		}
	}

	// Don't trace environment variables so secrets don't leak.
	if s.mode == traceOutput {
		fmt.Fprintf(&buf, "set -x\n")
	}

	// Prevent any commands attempting to read from stdin to consume
	// the shell script itself being sent to bash via its stdin.
	fmt.Fprintf(&buf, "\n(\n%s\n) < /dev/null\n", script)

	debugf("Running local script:\n-----\n%s\n------", buf.Bytes())

	var outbuf, errbuf safeBuffer
	cmd := exec.Command("/bin/bash", "-eu", "-")
	cmd.Stdin = &buf
	cmd.Dir = s.dir
	cmd.ExtraFiles = s.extraFiles
	switch s.mode {
	case traceOutput, combinedOutput:
		cmd.Stdout = &outbuf
		cmd.Stderr = &outbuf
	case splitOutput:
		cmd.Stdout = &outbuf
		cmd.Stderr = &errbuf
	case shellOutput:
		panic("internal error: runScript does not support shell mode")
	default:
		panic("internal error: invalid output mode")
	}

	err = cmd.Start()
	if err != nil {
		return nil, nil, fmt.Errorf("cannot start local command: %v", err)
	}

	done := make(chan error)
	go func() {
		done <- cmd.Wait()
	}()

	warnTimeout := s.warnTimeout
	killTimeout := s.killTimeout
	if warnTimeout == 0 {
		warnTimeout = defaultWarnTimeout
	} else if warnTimeout == -1 {
		warnTimeout = maxTimeout
	}
	if killTimeout == 0 {
		killTimeout = defaultKillTimeout
	} else if killTimeout == -1 {
		killTimeout = maxTimeout
	}

	if killTimeout%warnTimeout == 0 {
		// So message from kill won't race with warning.
		killTimeout -= 1 * time.Second
	}

	var lastOut, lastErr int

	kill := time.After(killTimeout)
	warn := time.NewTicker(warnTimeout)
	defer warn.Stop()
Loop:
	for {
		select {
		case err = <-done:
			break Loop
		case <-s.stop:
			buf.Write([]byte("\n<interrupted>"))
			err = fmt.Errorf("interrupted")
			break Loop
		case <-kill:
			cmd.Process.Kill()
			buf := &outbuf
			if errbuf.Len() > 0 {
				buf = &errbuf
			}
			buf.Write([]byte("\n<kill-timeout reached>"))
			err = fmt.Errorf("kill-timeout reached")
		case <-warn.C:
			var output, errput []byte
			output, lastOut = outbuf.Since(lastOut)
			errput, lastErr = errbuf.Since(lastErr)
			if len(output) == 0 || bytes.HasPrefix(errput, output) {
				// Also avoids double (... same ...) message.
				output = errput
			} else if len(errput) > 0 {
				output = append(output, '\n', '\n')
				output = append(output, errput...)
			}
			if bytes.Equal(output, unchangedMarker) {
				printf("WARNING: local script running late. Output unchanged.")
			} else if len(output) == 0 {
				printf("WARNING: local script running late. Output still empty.")
			} else {
				printf("WARNING: local script running late. Current output:\n-----\n%s\n-----", tail(output))
			}
		}
	}

	if outbuf.Len() > 0 {
		debugf("Output from running local script:\n-----\n%s\n-----", outbuf.Bytes())
	}
	if errbuf.Len() > 0 {
		debugf("Error output from running script:\n-----\n%s\n-----", errbuf.Bytes())
	}

	if exitStatus(err) == 213 {
		lines := bytes.Split(bytes.TrimSpace(outbuf.Bytes()), []byte{'\n'})
		m := commandExp.FindSubmatch(lines[len(lines)-1])
		if len(m) > 0 && string(m[1]) == "ERROR" {
			return nil, nil, fmt.Errorf("%s", m[2])
		}
		if len(m) > 0 && string(m[1]) == "FATAL" {
			return nil, nil, &FatalError{fmt.Errorf("%s", m[2])}
		}
	}

	if err != nil {
		if errbuf.Len() > 0 {
			err = outputErr(errbuf.Bytes(), err)
		} else if outbuf.Len() > 0 {
			err = outputErr(outbuf.Bytes(), err)
		}
		return nil, nil, err
	}
	return outbuf.Bytes(), errbuf.Bytes(), nil
}

func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	exit, ok := err.(*exec.ExitError)
	if !ok {
		return 1
	}
	wait, ok := exit.ProcessState.Sys().(syscall.WaitStatus)
	if !ok {
		return 1
	}
	return wait.ExitStatus()
}

type safeBuffer struct {
	buf bytes.Buffer
	mu  sync.Mutex
}

func (sbuf *safeBuffer) Write(data []byte) (int, error) {
	sbuf.mu.Lock()
	n, err := sbuf.buf.Write(data)
	sbuf.mu.Unlock()
	return n, err
}

func (sbuf *safeBuffer) Bytes() []byte {
	sbuf.mu.Lock()
	data := sbuf.buf.Bytes()
	sbuf.mu.Unlock()
	return data
}

var unchangedMarker = []byte("(...)")

func (sbuf *safeBuffer) Since(offset int) (data []byte, len int) {
	sbuf.mu.Lock()
	defer sbuf.mu.Unlock()

	data = sbuf.buf.Bytes()
	copy := true
	for i := offset - 1; i > 1; i-- {
		if data[i] == '\n' {
			data = append(unchangedMarker, data[i:]...)
			copy = false
			break
		}
	}
	if copy {
		data = append([]byte(nil), data...)
	}
	return bytes.TrimSpace(data), sbuf.buf.Len()
}

func (sbuf *safeBuffer) Len() int {
	sbuf.mu.Lock()
	l := sbuf.buf.Len()
	sbuf.mu.Unlock()
	return l
}

func outputErr(output []byte, err error) error {
	output = bytes.TrimSpace(output)
	if len(output) > 0 {
		if bytes.Contains(output, []byte{'\n'}) {
			err = fmt.Errorf("\n-----\n%s\n-----", output)
		} else {
			err = fmt.Errorf("%s", output)
		}
	}
	return err
}

func waitPortUp(ctx context.Context, what fmt.Stringer, address string) error {
	if !strings.Contains(address, ":") {
		address += ":22"
	}

	var timeout = time.After(5 * time.Minute)
	var relog = time.NewTicker(15 * time.Second)
	defer relog.Stop()
	var retry = time.NewTicker(1 * time.Second)
	defer retry.Stop()

	for {
		debugf("Waiting until %s is listening at %s...", what, address)
		conn, err := net.Dial("tcp", address)
		if err == nil {
			conn.Close()
			break
		}
		select {
		case <-retry.C:
		case <-relog.C:
			printf("Cannot connect to %s: %v", what, err)
		case <-timeout:
			return fmt.Errorf("cannot connect to %s: %v", what, err)
		case <-ctx.Done():
			return fmt.Errorf("cannot connect to %s: interrupted", what)
		}
	}
	return nil
}

func waitServerUp(ctx context.Context, server Server, username, password string) error {
	var timeout = time.After(5 * time.Minute)
	var relog = time.NewTicker(2 * time.Minute)
	defer relog.Stop()
	var retry = time.NewTicker(1 * time.Second)
	defer retry.Stop()

	for {
		debugf("Waiting until %s is listening...", server)
		client, err := Dial(server, username, password)
		if err == nil {
			client.Close()
			break
		}
		select {
		case <-retry.C:
		case <-relog.C:
			printf("Cannot connect to %s: %v", server, err)
		case <-timeout:
			return fmt.Errorf("cannot connect to %s: %v", server, err)
		case <-ctx.Done():
			return fmt.Errorf("cannot connect to %s: interrupted", server)
		}
	}
	return nil
}
