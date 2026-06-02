package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadProfilesSkipsInvalidFilesButReturnsValidProfiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.yaml")
	invalid := filepath.Join(dir, "invalid.yaml")

	if err := os.WriteFile(valid, []byte(`
name: remind-me-swift
repo:
  owner: bigtomcat6
  name: remind-me
service:
  name: gha-remind-me-swift.service
docker:
  container_name_prefix: gha-remind-me-swift
loop:
  state_file: /var/lib/gha-runner-tui/state/remind-me-swift.json
`), 0o600); err != nil {
		t.Fatalf("WriteFile valid returned error: %v", err)
	}

	if err := os.WriteFile(invalid, []byte("name: ["), 0o600); err != nil {
		t.Fatalf("WriteFile invalid returned error: %v", err)
	}

	profiles, errs, err := LoadProfiles(dir)
	if err != nil {
		t.Fatalf("LoadProfiles returned error: %v", err)
	}

	if len(profiles) != 1 {
		t.Fatalf("expected 1 valid profile, got %d", len(profiles))
	}
	if profiles[0].Name != "remind-me-swift" {
		t.Fatalf("expected remind-me-swift profile, got %q", profiles[0].Name)
	}
	if len(errs) != 1 {
		t.Fatalf("expected 1 profile load error, got %d", len(errs))
	}
}

func TestLoadProfileValidatesRequiredFields(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte("name: missing-parts\n"), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	_, err := LoadProfile(path)
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
}

func TestLoadProfileValidatesOrganizationProfile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "org.yaml")
	if err := os.WriteFile(path, []byte(`
name: example-org-swift
target:
  scope: organization
  org: Example Org
runner_group:
  name: example-org-swift
  create: true
  visibility: all
service:
  name: gha-example-org-swift.service
runner:
  environment: swift
docker:
  container_name_prefix: gha-example-org-swift
loop:
  state_file: /var/lib/gha-runner-tui/state/example-org-swift.json
`), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	profile, err := LoadProfile(path)
	if err != nil {
		t.Fatalf("LoadProfile returned error: %v", err)
	}
	if profile.Runner.Environment != "swift" {
		t.Fatalf("expected swift environment, got %q", profile.Runner.Environment)
	}
}

func TestLoadProfileRejectsOrganizationProfileWithoutEnvironment(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "org.yaml")
	if err := os.WriteFile(path, []byte(`
name: example-org
target:
  scope: organization
  org: Example Org
runner_group:
  name: example-org
  create: true
  visibility: all
service:
  name: gha-example-org.service
docker:
  container_name_prefix: gha-example-org
`), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	_, err := LoadProfile(path)
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "runner.environment") {
		t.Fatalf("expected runner.environment error, got %v", err)
	}
}
