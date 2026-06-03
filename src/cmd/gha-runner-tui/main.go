package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"gha-runner-tui/internal/app"
	"gha-runner-tui/internal/command"
	"gha-runner-tui/internal/config"
	"gha-runner-tui/internal/docker"
	gh "gha-runner-tui/internal/github"
	"gha-runner-tui/internal/systemd"
	"gha-runner-tui/internal/tui"
)

type syncOptions struct {
	configPath  string
	profilePath string
}

type syncer interface {
	SyncProfilePath(ctx context.Context, profilePath string) error
	SyncConfigProfiles(ctx context.Context) error
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "sync" {
		opts, err := parseSyncArgs(os.Args[2:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "sync failed: %v\n", err)
			os.Exit(2)
		}
		manager := newManager(opts.configPath, "/etc/systemd/system")
		if err := runSyncWith(context.Background(), opts, manager); err != nil {
			fmt.Fprintf(os.Stderr, "sync failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stdout, "runner groups synced")
		return
	}

	configPath := flag.String("config", "/etc/gha-runner-tui/config.yaml", "Path to global config file")
	systemdUnitDir := flag.String("systemd-unit-dir", "/etc/systemd/system", "Path to systemd unit directory used by create flow")
	flag.Parse()

	manager := newManager(*configPath, *systemdUnitDir)

	program := tea.NewProgram(tui.NewModel(manager), tea.WithAltScreen())
	if _, err := program.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "gha-runner-tui failed: %v\n", err)
		os.Exit(1)
	}
}

func newManager(configPath, systemdUnitDir string) app.RunnerManager {
	runner := command.NewSudoRunner(command.OSRunner{})
	githubConfig := githubConfigForManager(configPath)
	manager := app.NewRunnerManager(
		configPath,
		systemd.NewClient(runner),
		docker.NewClient(runner),
		gh.NewClient(githubConfig.APIBaseURL, githubConfig.TokenEnv, githubConfig.EnvFile, runner, http.DefaultClient),
	)
	manager.Runner = runner
	manager.SystemdUnitDir = systemdUnitDir
	return manager
}

func githubConfigForManager(configPath string) config.GitHubConfig {
	cfg, err := config.LoadGlobalConfig(configPath)
	if err != nil {
		return config.DefaultGlobalConfig().GitHub
	}
	return cfg.GitHub
}

func parseSyncArgs(args []string) (syncOptions, error) {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	opts := syncOptions{}
	fs.StringVar(&opts.configPath, "config", "/etc/gha-runner-tui/config.yaml", "Path to global config file")
	fs.StringVar(&opts.profilePath, "profile", "", "Path to a single profile file")

	if err := fs.Parse(args); err != nil {
		return syncOptions{}, err
	}
	return opts, nil
}

func runSyncWith(ctx context.Context, opts syncOptions, s syncer) error {
	if opts.profilePath != "" {
		return s.SyncProfilePath(ctx, opts.profilePath)
	}
	return s.SyncConfigProfiles(ctx)
}
