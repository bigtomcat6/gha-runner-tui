package command

import (
	"context"
	"os/exec"
	"strings"
)

type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type OSRunner struct{}

func (OSRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

type SudoRunner struct {
	Base Runner
}

func NewSudoRunner(base Runner) SudoRunner {
	if base == nil {
		base = OSRunner{}
	}
	return SudoRunner{Base: base}
}

func (r SudoRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := r.Base.Run(ctx, name, args...)
	if err == nil {
		return out, nil
	}
	if !shouldRetryWithSudo(name, string(out)) {
		return out, err
	}
	sudoArgs := append([]string{"-n", name}, args...)
	return r.Base.Run(ctx, "sudo", sudoArgs...)
}

func shouldRetryWithSudo(name, output string) bool {
	switch name {
	case "docker", "systemctl", "journalctl", "cat", "mkdir", "install":
	default:
		return false
	}

	lower := strings.ToLower(output)
	return strings.Contains(lower, "permission denied") ||
		strings.Contains(lower, "interactive authentication required") ||
		strings.Contains(lower, "operation not permitted") ||
		strings.Contains(lower, "access denied") ||
		strings.Contains(lower, "must be root")
}
