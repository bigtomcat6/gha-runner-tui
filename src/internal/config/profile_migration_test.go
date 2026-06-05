package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrateProfileAddsHostSocketAccessMode(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.yaml")
	writeProfileFile(t, path, `
name: legacy
repo:
  owner: example
  name: repo
service:
  name: gha-legacy.service
runner:
  name_prefix: legacy
docker:
  image: runner:latest
  container_name_prefix: gha-legacy
  volumes:
    - /var/run/docker.sock:/var/run/docker.sock
loop:
  state_file: /tmp/legacy.json
`)

	result, err := MigrateProfileAccessMode(path)
	if err != nil {
		t.Fatalf("MigrateProfileAccessMode returned error: %v", err)
	}
	if result.Status != ProfileMigrationUpdated {
		t.Fatalf("expected updated status, got %q", result.Status)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	if !strings.Contains(string(data), "access_mode: host-socket") {
		t.Fatalf("expected migrated host-socket access_mode, got:\n%s", string(data))
	}

	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Fatalf("expected backup file, stat returned %v", err)
	}
}

func TestMigrateProfileAddsRootlessAccessMode(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.yaml")
	writeProfileFile(t, path, `
name: legacy
repo:
  owner: example
  name: repo
service:
  name: gha-legacy.service
runner:
  name_prefix: legacy
docker:
  image: runner:latest
  container_name_prefix: gha-legacy
  volumes:
    - /run/user/1001/docker.sock:/var/run/docker.sock
  env:
    DOCKER_HOST: unix:///var/run/docker.sock
loop:
  state_file: /tmp/legacy.json
`)

	result, err := MigrateProfileAccessMode(path)
	if err != nil {
		t.Fatalf("MigrateProfileAccessMode returned error: %v", err)
	}
	if result.Status != ProfileMigrationUpdated {
		t.Fatalf("expected updated status, got %q", result.Status)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	if !strings.Contains(string(data), "access_mode: rootless") {
		t.Fatalf("expected migrated rootless access_mode, got:\n%s", string(data))
	}
}

func TestMigrateProfileSkipsAmbiguousSocketConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.yaml")
	original := strings.TrimPrefix(`
name: legacy
repo:
  owner: example
  name: repo
service:
  name: gha-legacy.service
runner:
  name_prefix: legacy
docker:
  image: runner:latest
  container_name_prefix: gha-legacy
  volumes:
    - /var/run/docker.sock:/var/run/docker.sock
    - /run/user/1001/docker.sock:/var/run/docker.sock
  env:
    DOCKER_HOST: unix:///var/run/docker.sock
loop:
  state_file: /tmp/legacy.json
`, "\n")
	writeProfileFile(t, path, original)

	result, err := MigrateProfileAccessMode(path)
	if err != nil {
		t.Fatalf("MigrateProfileAccessMode returned error: %v", err)
	}
	if result.Status != ProfileMigrationSkipped {
		t.Fatalf("expected skipped status, got %q", result.Status)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	if string(data) != original {
		t.Fatalf("expected ambiguous profile to remain unchanged, got:\n%s", string(data))
	}
}

func TestMigrateProfileCreatesBackupOnce(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.yaml")
	writeProfileFile(t, path, `
name: legacy
repo:
  owner: example
  name: repo
service:
  name: gha-legacy.service
runner:
  name_prefix: legacy
docker:
  image: runner:latest
  container_name_prefix: gha-legacy
  volumes:
    - /var/run/docker.sock:/var/run/docker.sock
loop:
  state_file: /tmp/legacy.json
`)

	if _, err := MigrateProfileAccessMode(path); err != nil {
		t.Fatalf("first migration returned error: %v", err)
	}
	backupPath := path + ".bak"
	backupData, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("ReadFile backup returned error: %v", err)
	}

	if _, err := MigrateProfileAccessMode(path); err != nil {
		t.Fatalf("second migration returned error: %v", err)
	}
	backupDataAfter, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("ReadFile backup after second migration returned error: %v", err)
	}

	if string(backupDataAfter) != string(backupData) {
		t.Fatal("expected backup contents to remain unchanged across repeated migration")
	}
}

func TestMigrateProfileLeavesAlreadyTaggedProfileUntouched(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.yaml")
	original := strings.TrimPrefix(`
name: legacy
repo:
  owner: example
  name: repo
service:
  name: gha-legacy.service
runner:
  name_prefix: legacy
docker:
  access_mode: host-socket
  image: runner:latest
  container_name_prefix: gha-legacy
  volumes:
    - /var/run/docker.sock:/var/run/docker.sock
loop:
  state_file: /tmp/legacy.json
`, "\n")
	writeProfileFile(t, path, original)

	result, err := MigrateProfileAccessMode(path)
	if err != nil {
		t.Fatalf("MigrateProfileAccessMode returned error: %v", err)
	}
	if result.Status != ProfileMigrationSkipped {
		t.Fatalf("expected skipped status, got %q", result.Status)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	if string(data) != original {
		t.Fatalf("expected already tagged profile to remain unchanged, got:\n%s", string(data))
	}
}

func writeProfileFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(strings.TrimPrefix(content, "\n")), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
}
