package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverLegacyProfilesParsesSystemdUnitsAndEnvFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	serviceDir := filepath.Join(root, "systemd")
	envDir := filepath.Join(root, "gha-runner")
	if err := os.MkdirAll(serviceDir, 0o755); err != nil {
		t.Fatalf("MkdirAll serviceDir returned error: %v", err)
	}
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		t.Fatalf("MkdirAll envDir returned error: %v", err)
	}

	unitPath := filepath.Join(serviceDir, "gha-remind-me-base.service")
	envPath := filepath.Join(envDir, "remind-me-base.env")

	if err := os.WriteFile(unitPath, []byte(`
[Service]
EnvironmentFile=`+envPath+`
ExecStart=/usr/local/bin/gha-ephemeral-loop
`), 0o644); err != nil {
		t.Fatalf("WriteFile unit returned error: %v", err)
	}
	if err := os.WriteFile(envPath, []byte(`
REPO_OWNER=bigtomcat6
REPO_NAME=remind-me
RUNNER_NAME=o-tokyo-s2-remind-me-base
RUNNER_LABELS=docker-ephemeral,o-tokyo-s2
IMAGE=gha-runner-base:latest
`), 0o644); err != nil {
		t.Fatalf("WriteFile env returned error: %v", err)
	}

	profiles, errs, err := DiscoverLegacyProfiles(serviceDir)
	if err != nil {
		t.Fatalf("DiscoverLegacyProfiles returned error: %v", err)
	}
	if len(errs) != 0 {
		t.Fatalf("expected 0 discovery errors, got %d", len(errs))
	}
	if len(profiles) != 1 {
		t.Fatalf("expected 1 profile, got %d", len(profiles))
	}

	profile := profiles[0]
	if profile.Name != "remind-me-base" {
		t.Fatalf("expected profile name remind-me-base, got %q", profile.Name)
	}
	if profile.Service.Name != "gha-remind-me-base.service" {
		t.Fatalf("unexpected service name %q", profile.Service.Name)
	}
	if profile.Runner.NamePrefix != "o-tokyo-s2-remind-me-base" {
		t.Fatalf("unexpected runner name %q", profile.Runner.NamePrefix)
	}
	if profile.Docker.ContainerNamePrefix != "gha-remind-me" {
		t.Fatalf("unexpected container prefix %q", profile.Docker.ContainerNamePrefix)
	}
	if profile.Docker.Image != "gha-runner-base:latest" {
		t.Fatalf("unexpected image %q", profile.Docker.Image)
	}
}
