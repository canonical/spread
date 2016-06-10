package spread

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/terminal"
)

type Client struct {
	server Server
	sshc   *ssh.Client
}

func Dial(server Server, password string) (*Client, error) {
	config := &ssh.ClientConfig{
		User:    "root",
		Auth:    []ssh.AuthMethod{ssh.Password(password)},
		Timeout: 10 * time.Second,
	}
	client, err := ssh.Dial("tcp", server.Address()+":22", config)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to %s: %v", server, err)
	}
	return &Client{server, client}, nil
}

func (c *Client) Close() error {
	return c.sshc.Close()
}

func (c *Client) Server() Server {
	return c.server
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
	output, err := session.CombinedOutput(fmt.Sprintf(`cat >"%s"`, path))
	if err != nil {
		err = outputErr(output, err)
		return fmt.Errorf("cannot write to %s at %s: %v", c.server, path, err)
	}

	if err := <-errch; err != nil {
		printf("Error writing to %s at %s: %v", c.server, path, err)
	}
	return nil
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

func (c *Client) ReadFile(path string) ([]byte, error) {
	session, err := c.sshc.NewSession()
	if err != nil {
		return nil, err
	}
	defer session.Close()

	debugf("Reading from %s at %s...", c.server, path)
	output, err := session.CombinedOutput(fmt.Sprintf("cat '%s'", path))
	if err != nil {
		err = outputErr(output, err)
		logf("Cannot read from %s at %s: %v", c.server, path, err)
		return nil, fmt.Errorf("cannot read from %s at %s: %v", c.server, path, err)
	}
	debugf("Got data from %s at %s:\n-----\n%# v\n-----", c.server, path, string(output))

	return output, nil
}

const (
	traceOutput = iota
	combinedOutput
	splitOutput
	shellOutput
)

func (c *Client) Run(script string, dir string, env map[string]string) error {
	_, err := c.run(script, dir, env, combinedOutput)
	return err
}

func (c *Client) Output(script string, dir string, env map[string]string) (output []byte, err error) {
	return c.run(script, dir, env, splitOutput)
}

func (c *Client) CombinedOutput(script string, dir string, env map[string]string) (output []byte, err error) {
	return c.run(script, dir, env, combinedOutput)
}

func (c *Client) Trace(script string, dir string, env map[string]string) (output []byte, err error) {
	return c.run(script, dir, env, traceOutput)
}

func (c *Client) Shell(script string, dir string, env map[string]string) error {
	_, err := c.run(script, dir, env, shellOutput)
	return err
}

func (c *Client) run(script string, dir string, env map[string]string, mode int) (output []byte, err error) {
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

	for key, value := range env {
		// TODO Value escaping.
		fmt.Fprintf(&buf, "export %s=\"%s\"\n", key, value)
	}
	if mode == shellOutput && env["PS1"] != "" {
		fmt.Fprintf(&buf, `echo PS1=\''%s'\' > $HOME/.bashrc`, env["PS1"])
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

	var stderr bytes.Buffer
	var cmd string
	switch mode {
	case traceOutput, combinedOutput:
		cmd = "/bin/sh -e - 2>&1"
	case splitOutput:
		cmd = "/bin/sh -e -"
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
		tstate, err := terminal.MakeRaw(0)
		if err != nil {
			return nil, fmt.Errorf("cannot put local terminal in raw mode: %v", err)
		}
		err = session.Run(cmd)
		terminal.Restore(0, tstate)
		termUnlock()
	} else {
		output, err = session.Output(cmd)
	}

	if len(output) > 0 {
		debugf("Output from running script on %s:\n-----\n%s\n-----", c.server, output)
	}
	if stderr.Len() > 0 {
		debugf("Error output from running script on %s:\n-----\n%s\n-----", c.server, output)
	}

	if err != nil {
		if mode == splitOutput {
			err = outputErr(stderr.Bytes(), err)
		} else {
			err = outputErr(output, err)
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

	errch := make(chan error, 2)
	go func() {
		errch <- cmd.Wait()
		stdin.Close()
	}()

	output, err := session.CombinedOutput(fmt.Sprintf(`mkdir -p "%s" && cd "%s" && /bin/tar -xz 2>&1`, to, to))
	if err != nil {
		return outputErr(output, err)
	}

	if err := <-errch; err != nil {
		return fmt.Errorf("local tar command returned error: %v", err)
	}

	return nil
}
