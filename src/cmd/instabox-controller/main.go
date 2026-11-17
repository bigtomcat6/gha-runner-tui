package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"gha-runner-tui/internal/instaboxcontroller"
)

// dependencies exist so tests can prove that a missing key causes no network
// or SSH activity without contacting the real service.
type dependencies struct {
	getenv    func(string) string
	newAPI    func(key string) (instaboxcontroller.MachineAPI, error)
	newWriter func() instaboxcontroller.DeadlineWriter
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, dependencies{
		getenv: os.Getenv,
		newAPI: func(key string) (instaboxcontroller.MachineAPI, error) {
			return instaboxcontroller.NewAPIClient(key, nil), nil
		},
		newWriter: func() instaboxcontroller.DeadlineWriter {
			return instaboxcontroller.NewSSHWriter()
		},
	}))
}

func run(args []string, stdout, stderr io.Writer, deps dependencies) int {
	fs := flag.NewFlagSet("instabox-controller", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "", "Path to controller JSON config")
	once := fs.Bool("once", false, "Run one polling round and exit")
	dryRun := fs.Bool("dry-run", false, "Never write a deadline or wake a machine")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "unexpected positional arguments")
		return 2
	}
	if *configPath == "" {
		fmt.Fprintln(stderr, "--config is required")
		return 2
	}

	// Authentication is checked first: no config read, lock file, network
	// request or SSH connection happens without a key.
	key := ""
	if deps.getenv != nil {
		key = strings.TrimSpace(deps.getenv("INSTABOX_API_KEY"))
	}
	if key == "" {
		fmt.Fprintln(stderr, "INSTABOX_API_KEY is required; no network or SSH request was made")
		return 2
	}

	cfg, err := instaboxcontroller.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "instabox-controller config: %v\n", err)
		return 2
	}
	api, err := deps.newAPI(key)
	if err != nil {
		fmt.Fprintln(stderr, "instabox-controller could not create the API client")
		return 2
	}
	controller := &instaboxcontroller.Controller{
		Config: cfg,
		API:    api,
		Writer: deps.newWriter(),
		DryRun: *dryRun,
		Logger: log.New(stderr, "instabox-controller: ", log.LstdFlags),
	}
	defer controller.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	if *once {
		if err := controller.RunOnce(ctx); err != nil {
			fmt.Fprintf(stderr, "instabox-controller round failed: %v\n", err)
			return 1
		}
		return 0
	}
	if err := controller.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(stderr, "instabox-controller failed: %v\n", err)
		return 1
	}
	return 0
}
