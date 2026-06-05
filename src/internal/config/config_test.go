package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadGlobalConfigUsesDefaultsWhenFileMissing(t *testing.T) {
	t.Parallel()

	cfg, err := LoadGlobalConfig(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatalf("LoadGlobalConfig returned error: %v", err)
	}

	if cfg.GitHub.TokenEnv != "GITHUB_TOKEN" {
		t.Fatalf("expected default token env, got %q", cfg.GitHub.TokenEnv)
	}
	if cfg.GitHub.EnvFile != "/etc/gha-runner-tui/github.env" {
		t.Fatalf("expected default github env file, got %q", cfg.GitHub.EnvFile)
	}
	if cfg.Paths.ProfilesDir != "/etc/gha-runner-tui/profiles" {
		t.Fatalf("expected default profiles dir, got %q", cfg.Paths.ProfilesDir)
	}
	if cfg.Systemd.LoopBinaryPath != "/usr/local/bin/gha-ephemeral-loop" {
		t.Fatalf("expected default loop binary path, got %q", cfg.Systemd.LoopBinaryPath)
	}
	if !cfg.Docker.UseCLI {
		t.Fatal("expected docker.use_cli to default to true")
	}
	if cfg.Docker.DefaultAccessMode != DockerAccessModeRootless {
		t.Fatalf("expected default docker access mode %q, got %q", DockerAccessModeRootless, cfg.Docker.DefaultAccessMode)
	}
	if !cfg.Docker.AutoDetectRootlessSocket {
		t.Fatal("expected docker.auto_detect_rootless_socket to default to true")
	}
	if !cfg.Docker.AllowHostSocketOptIn {
		t.Fatal("expected docker.allow_host_socket_opt_in to default to true")
	}
	if cfg.Docker.HostSocketPath != "/var/run/docker.sock" {
		t.Fatalf("expected default host socket path, got %q", cfg.Docker.HostSocketPath)
	}
}

func TestLoadGlobalConfigMergesDefaults(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte("github:\n  token_env: CI_GITHUB_TOKEN\n  env_file: /etc/gha-runner-tui/ci.env\npaths:\n  profiles_dir: /tmp/profiles\nsystemd:\n  loop_binary_path: /usr/local/bin/gha-ephemeral-loop-tui\ndocker:\n  default_access_mode: host-socket\n  rootless_socket_path: /run/user/1001/docker.sock\n  auto_detect_rootless_socket: false\n  allow_host_socket_opt_in: false\n  host_socket_path: /custom/docker.sock\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg, err := LoadGlobalConfig(path)
	if err != nil {
		t.Fatalf("LoadGlobalConfig returned error: %v", err)
	}

	if cfg.GitHub.TokenEnv != "CI_GITHUB_TOKEN" {
		t.Fatalf("expected token env override, got %q", cfg.GitHub.TokenEnv)
	}
	if cfg.GitHub.EnvFile != "/etc/gha-runner-tui/ci.env" {
		t.Fatalf("expected env file override, got %q", cfg.GitHub.EnvFile)
	}
	if cfg.GitHub.APIBaseURL != "https://api.github.com" {
		t.Fatalf("expected default api base url, got %q", cfg.GitHub.APIBaseURL)
	}
	if cfg.Paths.ProfilesDir != "/tmp/profiles" {
		t.Fatalf("expected custom profiles dir, got %q", cfg.Paths.ProfilesDir)
	}
	if cfg.Paths.StateDir != "/var/lib/gha-runner-tui/state" {
		t.Fatalf("expected default state dir, got %q", cfg.Paths.StateDir)
	}
	if cfg.Systemd.LoopBinaryPath != "/usr/local/bin/gha-ephemeral-loop-tui" {
		t.Fatalf("expected loop binary override, got %q", cfg.Systemd.LoopBinaryPath)
	}
	if !cfg.Docker.UseCLI {
		t.Fatal("expected docker.use_cli to remain true")
	}
	if cfg.Docker.DefaultAccessMode != DockerAccessModeHostSocket {
		t.Fatalf("expected docker access mode override %q, got %q", DockerAccessModeHostSocket, cfg.Docker.DefaultAccessMode)
	}
	if cfg.Docker.RootlessSocketPath != "/run/user/1001/docker.sock" {
		t.Fatalf("expected rootless socket path override, got %q", cfg.Docker.RootlessSocketPath)
	}
	if cfg.Docker.AutoDetectRootlessSocket {
		t.Fatal("expected docker.auto_detect_rootless_socket override to be false")
	}
	if cfg.Docker.AllowHostSocketOptIn {
		t.Fatal("expected docker.allow_host_socket_opt_in override to be false")
	}
	if cfg.Docker.HostSocketPath != "/custom/docker.sock" {
		t.Fatalf("expected host socket path override, got %q", cfg.Docker.HostSocketPath)
	}
}
