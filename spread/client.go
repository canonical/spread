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

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/terminal"
)

type Client struct {
	server Server
	sshc   *ssh.Client
	user   string

	warnTimeout time.Duration
	killTimeout time.Duration
}

func Dial(server Server, username, password string) (*Client, error) {
	config := &ssh.ClientConfig{
		User:    username,
		Auth:    []ssh.AuthMethod{ssh.Password(password)},
		Timeout: 10 * time.Second,
	}
	addr := server.Address()
	if !strings.Contains(addr, ":") {
		addr += ":22"
	}
	sshc, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to %s: %v", server, err)
	}
	client := &Client{
		server: server,
		sshc:   sshc,
	}
	return client, nil
}

func (c *Client) Close() error {
	return c.sshc.Close()
}

func (c *Client) Server() Server {
	return c.server
}

func (c *Client) SetWarnTimeout(warn time.Duration) {
	c.warnTimeout = warn
}

func (c *Client) SetKillTimeout(warn time.Duration) {
	c.killTimeout = warn
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

	debugf("Writing to %s at %s:\n-----\n%# v\n-----", c.server, path, string(data))

	var stderr safeBuffer
	session.Stderr = &stderr
	cmd := fmt.Sprintf(`cat >"%s"`, path)
	err = c.runCommand(session, cmd, nil, &stderr)
	if err != nil {
		err = outputErr(stderr.Bytes(), err)
		return fmt.Errorf("cannot write to %s at %s: %v", c.server, path, err)
	}

	if err := <-errch; err != nil {
		printf("Error writing to %s at %s: %v", c.server, path, err)
	}
	return nil
}

func (c *Client) ReadFile(path string) ([]byte, error) {
	session, err := c.sshc.NewSession()
	if err != nil {
		return nil, err
	}
	defer session.Close()

	debugf("Reading from %s at %s...", c.server, path)

	var stdout, stderr safeBuffer
	session.Stdout = &stdout
	session.Stderr = &stderr
	cmd := fmt.Sprintf(`cat "%s"`, path)
	err = c.runCommand(session, cmd, nil, &stderr)
	if err != nil {
		err = outputErr(stderr.Bytes(), err)
		logf("Cannot read from %s at %s: %v", c.server, path, err)
		return nil, fmt.Errorf("cannot read from %s at %s: %v", c.server, path, err)
	}
	output := stdout.Bytes()
	debugf("Got data from %s at %s:\n-----\n%# v\n-----", c.server, path, string(output))
	return output, nil
}

const (
	traceOutput = iota
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

func (c *Client) run(script string, dir string, env *Environment, mode int) (output []byte, err error) {
	script = strings.TrimSpace(script)
	if len(script) == 0 {
		return nil, nil
	}
	script += "\n"
	session, err := c.sshc.NewSession()
	if err != nil {
		return nil, err
	}
	defer session.Close()

	var buf bytes.Buffer
	buf.WriteString("export DEBIAN_FRONTEND=noninteractive\n")
	buf.WriteString("export DEBIAN_PRIORITY=critical\n")

	for _, k := range env.Keys() {
		v := env.Get(k)
		if len(v) == 0 || v[0] == '"' || v[0] == '\'' {
			fmt.Fprintf(&buf, "export %s=%s\n", k, v)
		} else {
			fmt.Fprintf(&buf, "export %s=\"%s\"\n", k, v)
		}
	}
	if mode == shellOutput && env.Get("PS1") != "" {
		fmt.Fprintf(&buf, `echo PS1=\''%s'\' > $HOME/.bashrc`, env.Get("PS1"))
	}
	if mode == traceOutput {
		// Don't trace environment variables so secrets don't leak.
		fmt.Fprintf(&buf, "set -x\n")
	}
	fmt.Fprintf(&buf, "\n%s\n", script)

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

	debugf("Sending script to %s:\n-----\n%s\n------", c.server, buf.Bytes())

	var stdout, stderr safeBuffer
	var cmd string
	switch mode {
	case traceOutput, combinedOutput:
		cmd = "/bin/bash -eu - 2>&1"
		session.Stdout = &stdout
	case splitOutput:
		cmd = "/bin/bash -eu -"
		session.Stdout = &stdout
		session.Stderr = &stderr
	case shellOutput:
		cmd = "{\n" + buf.String() + "\n}"
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

	if dir != "" {
		cmd = fmt.Sprintf(`cd "%s" && %s`, dir, cmd)
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
		debugf("Output from running script on %s:\n-----\n%s\n-----", c.server, stdout.Bytes())
	}
	if stderr.Len() > 0 {
		debugf("Error output from running script on %s:\n-----\n%s\n-----", c.server, stderr.Bytes())
	}

	if err != nil {
		if mode == splitOutput {
			err = outputErr(stderr.Bytes(), err)
		} else {
			err = outputErr(stdout.Bytes(), err)
		}
		return nil, err
	}

	if err := <-errch; err != nil {
		printf("Error writing script to %s: %v", c.server, err)
	}
	return output, nil
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
	if c.user == "root" {
		script = fmt.Sprintf(`echo root:'%s' | chpasswd`, password)
	} else {
		script = strings.Join([]string{
			`sudo sed -i 's/\(PermitRootLogin\|PasswordAuthentication\)\>.*/\1 yes/' /etc/ssh/sshd_config`,
			`echo root:'` + password + `' | sudo chpasswd`,
			`sudo pkill -o -HUP sshd || true`,
		}, "\n")
	}
	_, err := c.CombinedOutput(script, "", nil)
	if err != nil {
		return fmt.Errorf("cannot setup root access: %s", err)
	}
	return nil
}

func (c *Client) MissingOrEmpty(dir string) (bool, error) {
	output, err := c.Output(fmt.Sprintf(`! test -e "%s" || ls -a "%s"`, dir, dir), "", nil)
	if err != nil {
		return false, fmt.Errorf("cannot check if %s on %s is empty: %v", dir, c.server, err)
	}
	output = bytes.TrimSpace(output)
	if len(output) > 0 {
		for _, s := range strings.Split(string(output), "\n") {
			if s != "." && s != ".." {
				debugf("Found %q inside %q, considering non-empty.", s, dir)
				return false, nil
			}
		}
	}
	return true, nil
}

func (c *Client) Send(from, to string, include []string, exclude []string) error {
	empty, err := c.MissingOrEmpty(to)
	if err != nil {
		return err
	}
	if !empty {
		return fmt.Errorf("remote directory %s is not empty", to)
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

	args := []string{"-cz"}
	for _, pattern := range exclude {
		args = append(args, "--exclude="+pattern)
	}
	for _, pattern := range include {
		args = append(args, pattern)
	}
	cmd := exec.Command("tar", args...)
	cmd.Dir = from
	cmd.Stdout = stdin
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
	rcmd := fmt.Sprintf(`mkdir -p "%s" && cd "%s" && /bin/tar -xz 2>&1`, to, to)
	err = c.runCommand(session, rcmd, &stdout, nil)
	if err != nil {
		return outputErr(stdout.Bytes(), err)
	}

	if err := <-errch; err != nil {
		return fmt.Errorf("local tar command returned error: %v", err)
	}
	return nil
}

const (
	defaultWarnTimeout = 5 * time.Minute
	defaultKillTimeout = 15 * time.Minute
	maxTimeout         = 365 * 24 * time.Hour
)

func (c *Client) runCommand(session *ssh.Session, cmd string, stdout, stderr io.Writer) error {
	err := session.Start(cmd)
	if err != nil {
		return fmt.Errorf("cannot start remote command: %v", err)
	}

	done := make(chan error)
	go func() {
		done <- session.Wait()
	}()

	warnTimeout := c.warnTimeout
	killTimeout := c.killTimeout
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
			if bytes.Equal(output, unchangedMarker) {
				printf("WARNING: %s running late. Output unchanged.", c.server)
			} else if len(output) == 0 {
				printf("WARNING: %s running late. Output still empty.", c.server)
			} else {
				printf("WARNING: %s running late. Current output:\n-----\n%s\n-----", c.server, output)
			}
		}
	}
	panic("unreachable")
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
