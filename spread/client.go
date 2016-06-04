package spread

import (
	"bytes"
	"fmt"
	"time"

	"golang.org/x/crypto/ssh"
	"os/exec"
	"strings"
)

type Client struct {
	server Server
	sshc *ssh.Client
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
		output := string(bytes.TrimSpace(output))
		if len(output) > 0 {
			err = fmt.Errorf("%s", output)
		}
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
	output, err := session.CombinedOutput(fmt.Sprintf("cat '%s'", path))
	if err != nil {
		output := string(bytes.TrimSpace(output))
		if len(output) > 0 {
			err = fmt.Errorf("%s", output)
		}
		logf("Cannot read from %s at %s: %v", c.server, path, err)
		return nil, fmt.Errorf("cannot read from %s at %s: %v", c.server, path, err)
	}
	debugf("Got data from %s at %s:\n-----\n%# v\n-----", c.server, path, string(output))

	return output, nil
}

func (c *Client) Run(script []string, cwd string, env map[string]string) (output []byte, err error) {
	if len(script) == 0 {
		return nil, nil
	}
	session, err := c.sshc.NewSession()
	if err != nil {
		return nil, err
	}
	defer session.Close()

	stdin, err := session.StdinPipe()
	if err != nil {
		return nil, err
	}
	defer stdin.Close()

	errch := make(chan error, 2)
	go func() {
		for _, line := range script {
			_, err := stdin.Write([]byte(line))
			if err == nil {
				_, err = stdin.Write([]byte{'\n'})
			}
			if err != nil {
				errch <- err
				break
			}
		}
		errch <- stdin.Close()
	}()

	if false && Debug {
		var buf bytes.Buffer
		for key, value := range env {
			// TODO Value escaping.
			buf.WriteString(fmt.Sprintf("export %s=\"%s\"\n", key, value))
		}
		for _, line := range script {
			buf.WriteString(line)
			buf.WriteString("\n")
		}
		debugf("Sending script to %s:\n-----\n%s\n------", c.server, buf.Bytes())
	}

	output, err = session.CombinedOutput(fmt.Sprintf(`cd "%s" && /bin/sh -e -x - 2>&1`, cwd))
	if err != nil {
		output := bytes.TrimSpace(output)
		if len(output) > 0 {
			if bytes.Contains(output, []byte{'\n'}) {
				err = fmt.Errorf("\n-----\n%s\n-----", output)
			} else {
				err = fmt.Errorf("%s", output)
			}
		}
		return nil, err
	}
	debugf("Output from running script on %s:\n-----\n%s\n-----", c.server, output)

	if err := <-errch; err != nil {
		printf("Error writing script to %s: %v", c.server, err)
	}
	return output, nil
}

func (c *Client) RemoveAll(path string) error {
	_, err := c.Run([]string{fmt.Sprintf(`rm -rf "%s"`, path)}, "", nil)
	return err
}

func (c *Client) MissingOrEmpty(dir string) (bool, error) {
	output, err := c.Run([]string{fmt.Sprintf(`! test -e "%s" || ls -a "%s"`, dir, dir)}, "", nil)
	if err != nil {
		return false, fmt.Errorf("cannot check if %s on %s is empty: %v", dir, c.server, err)
	}
	for _, s := range strings.Split(string(output), "\n") {
		if s != "." && s != ".." {
			return false, nil
		}
	}
	return true, nil
}

func (c *Client) Send(from, to string, include []string, exclude []string) error {
	empty, err := c.MissingOrEmpty(to)
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
		args = append(args, "--exclude=" + pattern)
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
		output := string(bytes.TrimSpace(output))
		if len(output) > 0 {
			err = fmt.Errorf("%s", output)
		}
		return err
	}

	if err := <-errch; err != nil {
		return fmt.Errorf("local tar command returned error: %v", err)
	}

	return nil
}
