package config

import (
	"os"
	"path/filepath"
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
