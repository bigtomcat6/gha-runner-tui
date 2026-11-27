package systemd

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// UnitInfo is the read-only systemd evidence used by fleet planning. It
// reflects drop-ins because systemctl show reports the effective unit.
type UnitInfo struct {
	LoadState    string
	ActiveState  string
	FragmentPath string
	ExecStart    string
	DropInPaths  []string
	configPath   string
	configCount  int
}

// ConfigPath returns the single --config argument of ExecStart, if exactly one
// unambiguous occurrence exists.
func (u UnitInfo) ConfigPath() (string, error) {
	if u.configCount == 0 {
		return "", errors.New("effective ExecStart has no --config")
	}
	if u.configCount > 1 {
		return "", errors.New("effective ExecStart has more than one --config")
	}
	return u.configPath, nil
}

func (c Client) Show(ctx context.Context, service string) (UnitInfo, error) {
	out, err := c.runner.Run(ctx, "systemctl", "show", "-p", "ExecStart", "-p", "LoadState", "-p", "ActiveState", "-p", "FragmentPath", "-p", "DropInPaths", service)
	if err != nil {
		// An absent unit can be reported with a nonzero exit. Other output
		// from a failed command is not reliable status evidence.
		if info, parseErr := ParseUnitShow(string(out)); parseErr == nil && info.LoadState == "not-found" {
			return info, nil
		}
		return UnitInfo{}, err
	}
	return ParseUnitShow(string(out))
}

func ParseUnitShow(text string) (UnitInfo, error) {
	var info UnitInfo
	var execStartLines []string
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "LoadState":
			info.LoadState = strings.TrimSpace(value)
		case "ActiveState":
			info.ActiveState = strings.TrimSpace(value)
		case "FragmentPath":
			info.FragmentPath = strings.TrimSpace(value)
		case "DropInPaths":
			info.DropInPaths = strings.Fields(value)
		case "ExecStart":
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				execStartLines = append(execStartLines, trimmed)
			}
		}
	}
	if len(execStartLines) > 1 {
		info.ExecStart = strings.Join(execStartLines, "\n")
		return info, errors.New("effective unit has more than one ExecStart")
	}
	if len(execStartLines) == 1 {
		info.ExecStart = execStartLines[0]
	}
	args, err := execStartArgs(info.ExecStart)
	if err != nil {
		return info, err
	}
	for i, arg := range args {
		value := ""
		switch {
		case arg == "--config":
			if i+1 >= len(args) {
				info.configCount++
				continue
			}
			value = args[i+1]
		case strings.HasPrefix(arg, "--config="):
			value = strings.TrimPrefix(arg, "--config=")
		default:
			continue
		}
		info.configCount++
		if value != "" {
			info.configPath = value
		}
	}
	return info, nil
}

func execStartArgs(value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	if !strings.HasPrefix(value, "{") {
		// A hand-written value can be the bare command line in tests or older
		// systemd output. Split it without shell interpretation.
		return strings.Fields(value), nil
	}
	start := strings.Index(value, "argv[]=")
	if start < 0 {
		return nil, errors.New("ExecStart has no argv[]")
	}
	argv := value[start+len("argv[]="):]
	if end := strings.Index(argv, " ; "); end >= 0 {
		argv = argv[:end]
	} else if end := strings.Index(argv, "; "); end >= 0 {
		argv = argv[:end]
	}
	argv = strings.TrimSpace(argv)
	if argv == "" {
		return nil, errors.New("ExecStart argv is empty")
	}
	args := strings.Fields(argv)
	if len(args) == 0 {
		return nil, errors.New("ExecStart argv is empty")
	}
	return args, nil
}

func (c Client) RestartNoBlock(ctx context.Context, service string) error {
	_, err := c.runner.Run(ctx, "systemctl", "restart", "--no-block", service)
	return err
}

func (c Client) Disable(ctx context.Context, service string) error {
	_, err := c.runner.Run(ctx, "systemctl", "disable", service)
	return err
}

// FormatShow is a diagnostic helper for error messages.
func FormatShow(info UnitInfo) string {
	return fmt.Sprintf("LoadState=%s ActiveState=%s FragmentPath=%s ExecStart=%s", info.LoadState, info.ActiveState, info.FragmentPath, info.ExecStart)
}
