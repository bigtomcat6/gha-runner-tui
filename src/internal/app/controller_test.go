package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"gha-runner-tui/internal/command"
	"gha-runner-tui/internal/config"
	"gha-runner-tui/internal/docker"
	gh "gha-runner-tui/internal/github"
	"gha-runner-tui/internal/systemd"
)

type recordingRunner struct {
	calls []string
}

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := name
	for _, arg := range args {
		call += " " + arg
	}
	r.calls = append(r.calls, call)
	if name == "install" && len(args) >= 4 {
		data, err := os.ReadFile(args[2])
		if err != nil {
			return nil, err
		}
		parent := filepath.Dir(args[3])
		if err := os.Chmod(parent, 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(args[3], data, 0o644); err != nil {
			return nil, err
		}
		_ = os.Chmod(parent, 0o555)
	}
	return []byte("ok\n"), nil
}

type scriptedRunner struct {
	calls   []string
	outputs map[string][]byte
}

func (r *scriptedRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := name
	for _, arg := range args {
		call += " " + arg
	}
	r.calls = append(r.calls, call)
	if out, ok := r.outputs[call]; ok {
		return out, nil
	}
	return []byte("ok\n"), nil
}

func TestCreateProfileUsesLegacyLayoutWhenLegacyTokenExists(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	legacyDir := filepath.Join(root, "gha-runner")
	systemdDir := filepath.Join(root, "systemd")
	tokenFile := filepath.Join(legacyDir, "github_pat")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatalf("MkdirAll legacyDir returned error: %v", err)
	}
	if err := os.MkdirAll(systemdDir, 0o755); err != nil {
		t.Fatalf("MkdirAll systemdDir returned error: %v", err)
	}
	if err := os.WriteFile(tokenFile, []byte("token"), 0o600); err != nil {
		t.Fatalf("WriteFile token returned error: %v", err)
	}

	runner := &recordingRunner{}
	manager := NewRunnerManager(
		filepath.Join(root, "missing-config.yaml"),
		systemd.NewClient(runner),
		docker.NewClient(command.OSRunner{}),
		gh.NewClient("", "", "", nil, nil),
	)
	manager.SystemdUnitDir = systemdDir
	manager.LegacyEnvDir = legacyDir
	manager.LegacyTokenFile = tokenFile
	manager.LegacyLoopBinary = "/usr/local/bin/gha-ephemeral-loop"

	err := manager.CreateProfile(context.Background(), CreateProfileInput{
		Name:                "bigtomcat6-exp",
		RepoOwner:           "bigtomcat6",
		RepoName:            "bigtomcat6",
		RunnerLabels:        []string{"self-hosted", "linux", "o-tokyo-s2-exp"},
		DockerImage:         "gha-runner-base:latest",
		ServiceName:         "gha-bigtomcat6-exp.service",
		ContainerNamePrefix: "gha-bigtomcat6-",
		CPUs:                "1.0",
		Memory:              "1g",
		Ephemeral:           true,
	})
	if err != nil {
		t.Fatalf("CreateProfile returned error: %v", err)
	}

	envPath := filepath.Join(legacyDir, "bigtomcat6-exp.env")
	if _, err := os.Stat(envPath); err != nil {
		t.Fatalf("expected legacy env file, stat returned %v", err)
	}

	servicePath := filepath.Join(systemdDir, "gha-bigtomcat6-exp.service")
	serviceData, err := os.ReadFile(servicePath)
	if err != nil {
		t.Fatalf("ReadFile service returned error: %v", err)
	}
	if !contains(string(serviceData), "EnvironmentFile="+envPath) {
		t.Fatalf("expected legacy service EnvironmentFile, got:\n%s", string(serviceData))
	}

	yamlPath := filepath.Join("/etc/gha-runner-tui/profiles", "bigtomcat6-exp.yaml")
	if _, err := os.Stat(yamlPath); err == nil {
		t.Fatalf("did not expect yaml profile at %s in legacy mode", yamlPath)
	}

	if len(runner.calls) != 3 {
		t.Fatalf("expected 3 systemd calls, got %v", runner.calls)
	}
}

func TestCreateProfileFallsBackToInstallForProtectedLegacyPaths(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	legacyDir := filepath.Join(root, "gha-runner")
	systemdDir := filepath.Join(root, "systemd")
	tokenFile := filepath.Join(legacyDir, "github_pat")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatalf("MkdirAll legacyDir returned error: %v", err)
	}
	if err := os.MkdirAll(systemdDir, 0o755); err != nil {
		t.Fatalf("MkdirAll systemdDir returned error: %v", err)
	}
	if err := os.WriteFile(tokenFile, []byte("token"), 0o600); err != nil {
		t.Fatalf("WriteFile token returned error: %v", err)
	}
	if err := os.Chmod(legacyDir, 0o555); err != nil {
		t.Fatalf("Chmod legacyDir returned error: %v", err)
	}
	if err := os.Chmod(systemdDir, 0o555); err != nil {
		t.Fatalf("Chmod systemdDir returned error: %v", err)
	}
	defer os.Chmod(legacyDir, 0o755)
	defer os.Chmod(systemdDir, 0o755)

	runner := &recordingRunner{}
	manager := NewRunnerManager(
		filepath.Join(root, "missing-config.yaml"),
		systemd.NewClient(runner),
		docker.NewClient(command.OSRunner{}),
		gh.NewClient("", "", "", nil, nil),
	)
	manager.Runner = runner
	manager.SystemdUnitDir = systemdDir
	manager.LegacyEnvDir = legacyDir
	manager.LegacyTokenFile = tokenFile
	manager.LegacyLoopBinary = "/usr/local/bin/gha-ephemeral-loop"

	err := manager.CreateProfile(context.Background(), CreateProfileInput{
		Name:                "bigtomcat6-exp",
		RepoOwner:           "bigtomcat6",
		RepoName:            "bigtomcat6",
		RunnerLabels:        []string{"self-hosted", "linux", "o-tokyo-s2-exp"},
		DockerImage:         "gha-runner-base:latest",
		ServiceName:         "gha-bigtomcat6-exp.service",
		ContainerNamePrefix: "gha-bigtomcat6-",
		CPUs:                "1.0",
		Memory:              "1g",
		Ephemeral:           true,
	})
	if err != nil {
		t.Fatalf("CreateProfile returned error: %v", err)
	}

	if len(runner.calls) == 0 {
		t.Fatal("expected fallback runner calls, got none")
	}
}

func TestRestartLoopKillsRunningContainerBeforeRestartingService(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{}
	manager := NewRunnerManager(
		"",
		systemd.NewClient(runner),
		docker.NewClient(runner),
		gh.NewClient("", "", "", nil, nil),
	)

	snapshot := ProfileSnapshot{
		Profile: config.Profile{
			Service: config.ServiceConfig{Name: "gha-remind-me-exp.service"},
		},
		Container: docker.ContainerInfo{
			ID:    "abc123",
			Name:  "gha-remind-me-123",
			State: "running",
		},
	}

	if err := manager.RestartLoop(context.Background(), snapshot); err != nil {
		t.Fatalf("RestartLoop returned error: %v", err)
	}

	if len(runner.calls) < 2 {
		t.Fatalf("expected docker kill and systemctl restart, got %v", runner.calls)
	}
	if runner.calls[0] != "docker kill abc123" {
		t.Fatalf("expected first call to kill container, got %q", runner.calls[0])
	}
	if runner.calls[1] != "systemctl restart gha-remind-me-exp.service" {
		t.Fatalf("expected second call to restart service, got %q", runner.calls[1])
	}
}

func TestRestartLoopKillsAllRunningContainersForMatchingRunnerName(t *testing.T) {
	t.Parallel()

	runner := &scriptedRunner{
		outputs: map[string][]byte{
			"docker ps --all --filter name=gha-remind-me- --format {{.ID}}\t{{.Names}}\t{{.Image}}\t{{.Status}}": []byte(
				"new\tgha-remind-me-200\tgha-runner-base:latest\tUp 1 minute\n" +
					"old\tgha-remind-me-100\tgha-runner-base:latest\tUp 5 minutes\n" +
					"base\tgha-remind-me-050\tgha-runner-base:latest\tUp 10 hours\n",
			),
			"docker inspect gha-remind-me-200": []byte(inspectJSON("new", "gha-remind-me-200", "gha-runner-base:latest", "running", "gha-remind-me-exp")),
			"docker inspect gha-remind-me-100": []byte(inspectJSON("old", "gha-remind-me-100", "gha-runner-base:latest", "running", "gha-remind-me-exp")),
			"docker inspect gha-remind-me-050": []byte(inspectJSON("base", "gha-remind-me-050", "gha-runner-base:latest", "running", "o-tokyo-s2-remind-me-base")),
		},
	}

	manager := NewRunnerManager(
		"",
		systemd.NewClient(runner),
		docker.NewClient(runner),
		gh.NewClient("", "", "", nil, nil),
	)

	snapshot := ProfileSnapshot{
		Profile: config.Profile{
			Service: config.ServiceConfig{Name: "gha-remind-me-exp.service"},
			Runner:  config.RunnerConfig{NamePrefix: "gha-remind-me-exp"},
			Docker: config.DockerProfile{
				ContainerNamePrefix: "gha-remind-me-",
				Image:               "gha-runner-base:latest",
			},
		},
		Container: docker.ContainerInfo{
			ID:    "new",
			Name:  "gha-remind-me-200",
			State: "running",
		},
	}

	if err := manager.RestartLoop(context.Background(), snapshot); err != nil {
		t.Fatalf("RestartLoop returned error: %v", err)
	}

	expected := []string{
		"docker ps --all --filter name=gha-remind-me- --format {{.ID}}\t{{.Names}}\t{{.Image}}\t{{.Status}}",
		"docker inspect gha-remind-me-200",
		"docker inspect gha-remind-me-100",
		"docker inspect gha-remind-me-050",
		"docker kill new",
		"docker kill old",
		"systemctl restart gha-remind-me-exp.service",
	}
	if len(runner.calls) != len(expected) {
		t.Fatalf("expected calls %v, got %v", expected, runner.calls)
	}
	for i, want := range expected {
		if runner.calls[i] != want {
			t.Fatalf("call %d: expected %q, got %q", i, want, runner.calls[i])
		}
	}
}

func inspectJSON(id, name, image, status, runnerName string) string {
	return fmt.Sprintf(`[{"Id":"%s","Name":"/%s","Config":{"Image":"%s","Env":["RUNNER_NAME=%s"]},"State":{"Status":"%s"}}]`,
		id,
		name,
		image,
		runnerName,
		status,
	)
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && filepath.Base(needle) != "" && stringContains(haystack, needle))
}

func stringContains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	}())
}
