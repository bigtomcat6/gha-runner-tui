package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"gha-runner-tui/internal/buildinfo"
	"gha-runner-tui/internal/command"
	"gha-runner-tui/internal/config"
	"gha-runner-tui/internal/docker"
	gh "gha-runner-tui/internal/github"
	"gha-runner-tui/internal/loop"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gha-ephemeral-loop", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "", "Path to profile config file")
	version := fs.Bool("version", false, "Output build version")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "unexpected positional arguments")
		return 2
	}
	if *version {
		text := buildinfo.Current()
		n, err := io.WriteString(stdout, text)
		if err != nil || n != len(text) {
			fmt.Fprintln(stderr, "version output failed")
			return 1
		}
		return 0
	}

	if *configPath == "" {
		fmt.Fprintln(stderr, "--config is required")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := runLoop(ctx, *configPath, command.OSRunner{}, http.DefaultClient); err != nil {
		fmt.Fprintf(stderr, "gha-ephemeral-loop failed: %v\n", err)
		return 1
	}
	return 0
}

func runLoop(ctx context.Context, profilePath string, runner command.Runner, httpClient gh.HTTPDoer) error {
	profile, err := config.LoadProfile(profilePath)
	if err != nil {
		return err
	}

	tokenFile := profile.GitHub.TokenFile
	if tokenFile == "" {
		tokenFile = profile.GitHub.EnvFile
	}

	supervisor := loop.Supervisor{
		ProfilePath: profilePath,
		Docker:      docker.NewClient(runner),
		GitHub:      gh.NewClient("", profile.GitHub.TokenEnv, tokenFile, runner, httpClient),
	}

	return supervisor.Run(ctx)
}
