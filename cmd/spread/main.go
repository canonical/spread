package main

import (
	"crypto/rand"
	"flag"
	"fmt"
	"log"
	mrand "math/rand"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/niemeyer/pretty"
	"github.com/snapcore/spread/spread"
)

var (
	verbose        = flag.Bool("v", false, "Show detailed progress information")
	vverbose       = flag.Bool("vv", false, "Show debugging messages as well")
	list           = flag.Bool("list", false, "Just show list of jobs that would run")
	pass           = flag.String("pass", "", "Server password to use, defaults to random")
	reuse          = flag.Bool("reuse", false, "Keep servers running for reuse")
	reusePid       = flag.Int("reuse-pid", 0, "Reuse servers from crashed process")
	resend         = flag.Bool("resend", false, "Resend project content to reused servers")
	debug          = flag.Bool("debug", false, "Run shell after script errors")
	shell          = flag.Bool("shell", false, "Run shell instead of task scripts")
	shellBefore    = flag.Bool("shell-before", false, "Run shell before task scripts")
	shellAfter     = flag.Bool("shell-after", false, "Run shell after task scripts")
	abend          = flag.Bool("abend", false, "Stop without restoring on first error")
	restore        = flag.Bool("restore", false, "Run only the restore scripts")
	discard        = flag.Bool("discard", false, "Discard reused servers without running")
	artifacts      = flag.String("artifacts", "", "Where to store task artifacts")
	seed           = flag.Int64("seed", 0, "Seed for job order permutation")
	repeat         = flag.Int("repeat", 0, "Number of times to repeat each task")
	garbageCollect = flag.Bool("gc", false, "Garbage collect backend resources when possible")
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	mrand.Seed(time.Now().UnixNano())
	flag.Parse()

	spread.Logger = log.New(os.Stdout, "", 0)
	spread.Verbose = *verbose
	spread.Debug = *vverbose

	var other bool
	for _, b := range []bool{*debug, *shell, *shellBefore || *shellAfter, *abend, *restore} {
		if b && other {
			return fmt.Errorf("cannot have more than one of -debug, -shell, -shell-before/after, -abend, and -restore")

		}
		other = other || b
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

	var filter spread.Filter
	var err error
	if args := flag.Args(); len(args) > 0 {
		filter, err = spread.NewFilter(args)
		if err != nil {
			return err
		}
	}

	options := &spread.Options{
		Password:       password,
		Filter:         filter,
		Reuse:          *reuse,
		ReusePid:       *reusePid,
		Resend:         *resend,
		Debug:          *debug,
		Shell:          *shell,
		ShellBefore:    *shellBefore,
		ShellAfter:     *shellAfter,
		Abend:          *abend,
		Restore:        *restore,
		Discard:        *discard,
		Artifacts:      *artifacts,
		Seed:           *seed,
		Repeat:         *repeat,
		GarbageCollect: *garbageCollect,
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

	if options.Discard && options.ReusePid == 0 {
		options.Reuse = true
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

func parseReuseEntry(entry string) (backend string, addrs []string) {
	if i := strings.Index(entry, ":"); i > 0 {
		return entry[:i], strings.Split(entry[i+1:], ",")
	}
	return "", nil
}
