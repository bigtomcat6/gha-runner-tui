package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"gha-runner-tui/internal/app"
	"gha-runner-tui/internal/command"
	"gha-runner-tui/internal/docker"
	gh "gha-runner-tui/internal/github"
	"gha-runner-tui/internal/systemd"
	"gha-runner-tui/internal/tui"
)

func main() {
	configPath := flag.String("config", "/etc/gha-runner-tui/config.yaml", "Path to global config file")
	systemdUnitDir := flag.String("systemd-unit-dir", "/etc/systemd/system", "Path to systemd unit directory used by create flow")
	flag.Parse()

	runner := command.NewSudoRunner(command.OSRunner{})
	manager := app.NewRunnerManager(
		*configPath,
		systemd.NewClient(runner),
		docker.NewClient(runner),
		gh.NewClient("", "", "", runner, http.DefaultClient),
	)
	manager.Runner = runner
	manager.SystemdUnitDir = *systemdUnitDir

	program := tea.NewProgram(tui.NewModel(manager), tea.WithAltScreen())
	if _, err := program.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "gha-runner-tui failed: %v\n", err)
		os.Exit(1)
	}
}
