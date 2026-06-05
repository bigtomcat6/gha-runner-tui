package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"

	"gha-runner-tui/internal/command"
	"gha-runner-tui/internal/config"
	"gha-runner-tui/internal/docker"
	gh "gha-runner-tui/internal/github"
	"gha-runner-tui/internal/loop"
)

func main() {
	configPath := flag.String("config", "", "Path to profile config file")
	flag.Parse()

	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "--config is required")
		os.Exit(2)
	}

	profile, err := config.LoadProfile(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load profile: %v\n", err)
		os.Exit(1)
	}

	tokenFile := profile.GitHub.TokenFile
	if tokenFile == "" {
		tokenFile = profile.GitHub.EnvFile
	}

	runner := command.OSRunner{}
	supervisor := loop.Supervisor{
		ProfilePath: *configPath,
		Docker:      docker.NewClient(runner),
		GitHub:      gh.NewClient("", profile.GitHub.TokenEnv, tokenFile, nil, http.DefaultClient),
	}

	if err := supervisor.Run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "gha-ephemeral-loop failed: %v\n", err)
		os.Exit(1)
	}
}
