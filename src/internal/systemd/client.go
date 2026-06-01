package systemd

import (
	"context"
	"fmt"
	"strings"

	"gha-runner-tui/internal/command"
	"gha-runner-tui/internal/state"
)

type Client struct {
	runner command.Runner
}

type ServiceStatus struct {
	Active  state.SystemdStatus
	Enabled bool
}

func NewClient(runner command.Runner) Client {
	return Client{runner: runner}
}

func (c Client) Status(ctx context.Context, service string) (ServiceStatus, error) {
	activeOut, activeErr := c.runner.Run(ctx, "systemctl", "is-active", service)
	active := state.NormalizeSystemdStatus(strings.TrimSpace(string(activeOut)))
	if activeErr != nil && active == state.SystemdUnknown {
		return ServiceStatus{}, activeErr
	}

	enabledOut, enabledErr := c.runner.Run(ctx, "systemctl", "is-enabled", service)
	enabledText := strings.TrimSpace(string(enabledOut))
	enabled := enabledText == "enabled"
	if enabledErr != nil && enabledText == "" {
		return ServiceStatus{}, enabledErr
	}

	return ServiceStatus{
		Active:  active,
		Enabled: enabled,
	}, nil
}

func (c Client) Logs(ctx context.Context, service string, tail int, follow bool) (string, error) {
	args := []string{"-u", service, "-n", fmt.Sprintf("%d", tail), "--no-pager"}
	if follow {
		args = append(args, "-f")
	}
	out, err := c.runner.Run(ctx, "journalctl", args...)
	return string(out), err
}

func (c Client) Start(ctx context.Context, service string) error {
	_, err := c.runner.Run(ctx, "systemctl", "start", service)
	return err
}

func (c Client) Stop(ctx context.Context, service string) error {
	_, err := c.runner.Run(ctx, "systemctl", "stop", service)
	return err
}

func (c Client) Restart(ctx context.Context, service string) error {
	_, err := c.runner.Run(ctx, "systemctl", "restart", service)
	return err
}

func (c Client) Enable(ctx context.Context, service string) error {
	_, err := c.runner.Run(ctx, "systemctl", "enable", service)
	return err
}

func (c Client) DaemonReload(ctx context.Context) error {
	_, err := c.runner.Run(ctx, "systemctl", "daemon-reload")
	return err
}
