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

	// The plain config is read first so each machine's apiKeyEnv name is known.
	// It contains no key values. Every machine credential is then resolved and
	// validated before any state directory, lock file, network request or SSH
	// connection exists.
	cfg, err := instaboxcontroller.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "instabox-controller config: %v\n", err)
		return 2
	}
	secrets, err := resolveCredentials(cfg, deps.getenv)
	if err != nil {
		fmt.Fprintf(stderr, "instabox-controller credentials: %v; no network or SSH request was made\n", err)
		return 2
	}

	// One client per distinct key environment variable; machines that reference
	// the same env var deliberately share one account.
	apis := map[string]instaboxcontroller.MachineAPI{}
	for env, key := range secrets {
		api, err := deps.newAPI(key)
		if err != nil {
			fmt.Fprintf(stderr, "instabox-controller could not create an API client for %s\n", env)
			return 2
		}
		apis[env] = api
	}
	machineAPIs := map[string]instaboxcontroller.MachineAPI{}
	for i := range cfg.Machines {
		machineAPIs[cfg.Machines[i].MachineID] = apis[apiKeyEnvName(cfg.Machines[i])]
	}

	controller := &instaboxcontroller.Controller{
		Config: cfg,
		APIs:   machineAPIs,
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

func apiKeyEnvName(machine instaboxcontroller.MachineConfig) string {
	if machine.APIKeyEnv == nil {
		return instaboxcontroller.DefaultAPIKeyEnv
	}
	return *machine.APIKeyEnv
}

// resolveCredentials validates every machine's key environment before any
// client is built. A machine with an explicit apiKeyEnv never falls back to
// the global INSTABOX_API_KEY; a missing or blank value fails closed.
func resolveCredentials(cfg instaboxcontroller.Config, getenv func(string) string) (map[string]string, error) {
	secrets := map[string]string{}
	for i := range cfg.Machines {
		machine := cfg.Machines[i]
		env := apiKeyEnvName(machine)
		if _, resolved := secrets[env]; resolved {
			continue
		}
		value := ""
		if getenv != nil {
			value = strings.TrimSpace(getenv(env))
		}
		if value == "" {
			return nil, fmt.Errorf("environment variable %s for machine %s is missing or empty", env, machine.MachineID)
		}
		secrets[env] = value
	}
	return secrets, nil
}
