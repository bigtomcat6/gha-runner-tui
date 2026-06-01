package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"gha-runner-tui/internal/command"
	"gha-runner-tui/internal/state"
)

type Client struct {
	runner command.Runner
}

type ContainerInfo struct {
	ID         string
	Name       string
	Image      string
	StatusText string
	State      state.ContainerStatus
}

type ContainerDetails struct {
	ID    string
	Name  string
	Image string
	State state.ContainerStatus
	Env   map[string]string
}

type RunSpec struct {
	Name    string
	Image   string
	CPUs    string
	Memory  string
	Volumes []string
	Env     map[string]string
}

func NewClient(runner command.Runner) Client {
	return Client{runner: runner}
}

func (c Client) CurrentOrLatest(ctx context.Context, prefix string) (ContainerInfo, error) {
	containers, err := c.ListByPrefix(ctx, prefix)
	if err != nil {
		return ContainerInfo{State: state.ContainerNone}, err
	}
	if len(containers) == 0 {
		return ContainerInfo{State: state.ContainerNone}, nil
	}
	return containers[0], nil
}

func (c Client) ListByPrefix(ctx context.Context, prefix string) ([]ContainerInfo, error) {
	out, err := c.runner.Run(ctx, "docker", "ps", "--all", "--filter", "name="+prefix, "--format", "{{.ID}}\t{{.Names}}\t{{.Image}}\t{{.Status}}")
	if err != nil && strings.TrimSpace(string(out)) == "" {
		return nil, err
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return nil, nil
	}

	containers := make([]ContainerInfo, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		container, parseErr := ParseContainerLine(line)
		if parseErr != nil {
			return nil, parseErr
		}
		containers = append(containers, container)
	}
	return containers, nil
}

func ParseContainerLine(line string) (ContainerInfo, error) {
	parts := strings.SplitN(line, "\t", 4)
	if len(parts) != 4 {
		return ContainerInfo{}, fmt.Errorf("unexpected docker line: %q", line)
	}

	return ContainerInfo{
		ID:         parts[0],
		Name:       parts[1],
		Image:      parts[2],
		StatusText: parts[3],
		State:      normalizeDockerStatus(parts[3]),
	}, nil
}

func (c Client) Logs(ctx context.Context, container string, tail int, follow bool) (string, error) {
	args := []string{"logs", "--tail", fmt.Sprintf("%d", tail)}
	if follow {
		args = append(args, "-f")
	}
	args = append(args, container)
	out, err := c.runner.Run(ctx, "docker", args...)
	return string(out), err
}

func (c Client) Kill(ctx context.Context, container string) error {
	_, err := c.runner.Run(ctx, "docker", "kill", container)
	return err
}

func (c Client) CleanupExited(ctx context.Context, prefix string) ([]string, error) {
	containers, err := c.ListByPrefix(ctx, prefix)
	if err != nil {
		return nil, err
	}
	removed := make([]string, 0)
	for _, container := range containers {
		if container.State != state.ContainerExited && container.State != state.ContainerDead {
			continue
		}
		if _, err := c.runner.Run(ctx, "docker", "rm", container.ID); err != nil {
			return removed, err
		}
		removed = append(removed, container.Name)
	}
	return removed, nil
}

func (c Client) Inspect(ctx context.Context, idOrName string) (ContainerDetails, error) {
	out, err := c.runner.Run(ctx, "docker", "inspect", idOrName)
	if err != nil {
		return ContainerDetails{}, err
	}

	var payload []struct {
		ID     string `json:"Id"`
		Name   string `json:"Name"`
		Config struct {
			Image string   `json:"Image"`
			Env   []string `json:"Env"`
		} `json:"Config"`
		State struct {
			Status string `json:"Status"`
		} `json:"State"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return ContainerDetails{}, err
	}
	if len(payload) == 0 {
		return ContainerDetails{}, fmt.Errorf("container %q not found", idOrName)
	}

	env := map[string]string{}
	for _, entry := range payload[0].Config.Env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			env[key] = value
		}
	}

	return ContainerDetails{
		ID:    payload[0].ID,
		Name:  strings.TrimPrefix(payload[0].Name, "/"),
		Image: payload[0].Config.Image,
		State: normalizeDockerStatus(payload[0].State.Status),
		Env:   env,
	}, nil
}

func (c Client) RunDetached(ctx context.Context, spec RunSpec) (string, error) {
	args := []string{"run", "-d", "--name", spec.Name}
	if spec.CPUs != "" {
		args = append(args, "--cpus", spec.CPUs)
	}
	if spec.Memory != "" {
		args = append(args, "--memory", spec.Memory)
	}
	for _, volume := range spec.Volumes {
		args = append(args, "-v", volume)
	}
	for key, value := range spec.Env {
		args = append(args, "-e", fmt.Sprintf("%s=%s", key, value))
	}
	args = append(args, spec.Image)

	out, err := c.runner.Run(ctx, "docker", args...)
	return strings.TrimSpace(string(out)), err
}

func (c Client) Wait(ctx context.Context, container string) (int, error) {
	out, err := c.runner.Run(ctx, "docker", "wait", container)
	if err != nil {
		return 0, err
	}
	exitCode, parseErr := strconv.Atoi(strings.TrimSpace(string(out)))
	if parseErr != nil {
		return 0, parseErr
	}
	return exitCode, nil
}

func (c Client) Remove(ctx context.Context, container string, force bool) error {
	args := []string{"rm"}
	if force {
		args = append(args, "-f")
	}
	args = append(args, container)
	_, err := c.runner.Run(ctx, "docker", args...)
	return err
}

func (c Client) ReadLogs(ctx context.Context, container string) (string, error) {
	out, err := c.runner.Run(ctx, "docker", "logs", container)
	return string(out), err
}

func normalizeDockerStatus(value string) state.ContainerStatus {
	lower := strings.ToLower(value)
	switch {
	case lower == "":
		return state.ContainerNone
	case lower == "running":
		return state.ContainerRunning
	case lower == "created":
		return state.ContainerCreated
	case lower == "exited":
		return state.ContainerExited
	case lower == "dead":
		return state.ContainerDead
	case strings.HasPrefix(lower, "up "):
		return state.ContainerRunning
	case strings.HasPrefix(lower, "created"):
		return state.ContainerCreated
	case strings.HasPrefix(lower, "exited"):
		return state.ContainerExited
	case strings.HasPrefix(lower, "dead"):
		return state.ContainerDead
	default:
		return state.ContainerUnknown
	}
}
