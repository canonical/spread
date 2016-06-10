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

var (
	verbose  = flag.Bool("v", false, "Show detailed progress information")
	vverbose = flag.Bool("vv", false, "Show debugging messages as well")
	list     = flag.Bool("list", false, "Just show list of jobs that would run")
	pass     = flag.String("pass", "", "Server password to use, defaults to random")
	keep     = flag.Bool("keep", false, "Keep servers running for reuse")
	reuse    = flag.String("reuse", "", "Reuse servers held running by -keep")
	resend   = flag.Bool("resend", false, "Resend project data to reused servers")
	debug    = flag.Bool("debug", false, "Run shell after script errors")
	shell    = flag.Bool("shell", false, "Run shell instead of task scripts")
	abend    = flag.Bool("abend", false, "Stop without restoring on first error")
	restore  = flag.Bool("restore", false, "Run only the restore scripts")
	discard  = flag.Bool("discard", false, "Discard reused servers without running")
)

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
	if *keep && *discard {
		return fmt.Errorf("cannot have both -keep and -discard")
	}

	var other bool
	for _, b := range []bool{*debug, *shell, *abend, *restore} {
		if b && other {
			return fmt.Errorf("cannot have more than one of -debug, -shell, -abend, and -restore")

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
		Password: password,
		Filter:   filter,
		Keep:     *keep,
		Resend:   *resend,
		Debug:    *debug,
		Shell:    *shell,
		Abend:    *abend,
		Restore:  *restore,
		Discard:  *discard,
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
		options.Reuse = strings.Split(*reuse, ",")
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
