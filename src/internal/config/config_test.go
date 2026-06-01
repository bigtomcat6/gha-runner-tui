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
	if cfg.Paths.ProfilesDir != "/etc/gha-runner-tui/profiles" {
		t.Fatalf("expected default profiles dir, got %q", cfg.Paths.ProfilesDir)
	}
	if !cfg.Docker.UseCLI {
		t.Fatal("expected docker.use_cli to default to true")
	}
}

func TestLoadGlobalConfigMergesDefaults(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte("github:\n  token_env: CI_GITHUB_TOKEN\npaths:\n  profiles_dir: /tmp/profiles\n")
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
	if cfg.GitHub.APIBaseURL != "https://api.github.com" {
		t.Fatalf("expected default api base url, got %q", cfg.GitHub.APIBaseURL)
	}
	if cfg.Paths.ProfilesDir != "/tmp/profiles" {
		t.Fatalf("expected custom profiles dir, got %q", cfg.Paths.ProfilesDir)
	}
	if cfg.Paths.StateDir != "/var/lib/gha-runner-tui/state" {
		t.Fatalf("expected default state dir, got %q", cfg.Paths.StateDir)
	}
	if !cfg.Docker.UseCLI {
		t.Fatal("expected docker.use_cli to remain true")
	}
}
