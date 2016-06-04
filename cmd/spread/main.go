package main

import (
	"crypto/rand"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/kr/pretty"
	"github.com/snapcore/spread/spread"
	"os/signal"
)

var verbose = flag.Bool("v", false, "Show detailed progress information")
var vverbose = flag.Bool("vv", false, "Show debugging messages as well")
var list = flag.Bool("list", false, "Just show list of tasks to be run")
var keep = flag.Bool("keep", false, "Keep servers running when done")
var debug = flag.Bool("debug", false, "Stop on failure for debugging (implies -keep)")
var pass = flag.String("pass", "", "Server password to use, defaults to random")
var reuse = flag.String("reuse", "", "Reuse server with provided comma-separated IDs")

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	flag.Parse()

	spread.Logger = log.New(os.Stdout, "", log.LstdFlags)
	spread.Verbose = *verbose
	spread.Debug = *vverbose

	if *reuse != "" && *pass == "" {
		return fmt.Errorf("cannot have -reuse without -pass")
	}

	password := *pass
	if password == "" {
		buf := make([]byte, 8)
		_, err := rand.Read(buf)
		if err != nil {
			return fmt.Errorf("cannot generate random password: %v", err)
		}
		password = fmt.Sprintf("%x", buf)
	}

	filter, err := spread.NewFilter(flag.Args())
	if err != nil {
		return err
	}

	options := &spread.Options{
		Keep:     *keep,
		Debug:    *debug,
		Filter:   filter,
		Password: password,
	}

	project, err := spread.Load(".")
	if err != nil {
		return err
	}

	if *list {
		jobs, err := project.Jobs(options)
		if err != nil {
			return err
		}
		for _, job := range jobs {
			fmt.Println(job.Name)
		}
		return nil
	}

	if *reuse != "" {
		value, err := parseReuse(project, *reuse)
		if err != nil {
			return err
		}
		options.Reuse = value
	}

	runner, err := spread.Start(project, options)
	if err != nil {
		return err
	}

	sigch := make(chan os.Signal, 1)
	signal.Notify(sigch, os.Interrupt)
	go func() {
		<-sigch
		runner.Stop()
	}()

	return runner.Wait()
}

func printf(format string, v ...interface{}) {
	if spread.Logger != nil {
		spread.Logger.Output(2, pretty.Sprintf(format, v...))
	}
}

func parseReuse(project *spread.Project, s string) (map[string][]string, error) {
	reuse := make(map[string][]string)
	for _, entry := range strings.Fields(s) {
		bname, addrs := parseReuseEntry(entry)
		if len(bname) == 0 || len(addrs) == 0 {
			return nil, fmt.Errorf("-reuse must be formatted as 'backend1:1.2.3.4,1.2.3.5 backend2:1.2.3.6'")
		}
		if _, ok := project.Backends[bname]; !ok {
			return nil, fmt.Errorf("-reuse refers to unknown backend %q", bname)
		}
		reuse[bname] = addrs
	}
	return reuse, nil
}

func parseReuseEntry(entry string) (backend string, addrs []string) {
	if i := strings.Index(entry, ":"); i > 0 {
		return entry[:i], strings.Split(entry[i+1:], ",")
	}
	return "", nil
}
