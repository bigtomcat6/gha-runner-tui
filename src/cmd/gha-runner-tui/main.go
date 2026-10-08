package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

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
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, launchTUI, newManager))
}

func run(args []string, stdout, stderr io.Writer, launchTUI func(string, string) error, makeManager func(string, string) app.RunnerManager) int {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return runCLI(context.Background(), args, stdout, stderr, makeManager)
	}
	fs := flag.NewFlagSet("gha-runner-tui", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", defaultConfigPath, "Path to global config file")
	unitDir := fs.String("systemd-unit-dir", defaultUnitDir, "Path to systemd unit directory used by create flow")
	if err := fs.Parse(args); err != nil {
		return writeArgumentError(stderr, err)
	}
	if fs.NArg() != 0 {
		return writeArgumentError(stderr, fmt.Errorf("unexpected TUI arguments"))
	}
	if err := launchTUI(*configPath, *unitDir); err != nil {
		return writeOperationError(stderr, fmt.Errorf("gha-runner-tui failed: %w", err))
	}
	return 0
}

func launchTUI(configPath, unitDir string) error {
	manager := newManager(configPath, unitDir)
	program := tea.NewProgram(tui.NewModel(manager), tea.WithAltScreen())
	_, err := program.Run()
	return err
}

func newManager(configPath, systemdUnitDir string) app.RunnerManager {
	runner := command.NewSudoRunner(command.OSRunner{})
	githubConfig := githubConfigForManager(configPath)
	manager := app.NewRunnerManager(
		configPath,
		systemd.NewClient(runner),
		docker.NewClient(runner),
		gh.NewGlobalClient(githubConfig.APIBaseURL, githubConfig.TokenEnv, githubConfig.EnvFile, runner, http.DefaultClient),
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
