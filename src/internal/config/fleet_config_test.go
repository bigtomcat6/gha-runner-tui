package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagedByValidationAndLegacyUnknownFields(t *testing.T) {
	base := Profile{Name: "p", Repo: RepoConfig{Owner: "me", Name: "app"}, Service: ServiceConfig{Name: "gha-p.service"}, Docker: DockerProfile{ContainerNamePrefix: "gha-p"}}
	for _, value := range []string{"", ManagedByLocal, ManagedByFleet} {
		base.ManagedBy = value
		if err := base.Validate(); err != nil {
			t.Fatalf("%q: %v", value, err)
		}
		if got := base.EffectiveManagedBy(); (value == "" && got != ManagedByLocal) || (value != "" && got != value) {
			t.Fatalf("%q effective=%q", value, got)
		}
	}
	base.ManagedBy = "center"
	if err := base.Validate(); err == nil || !strings.Contains(err.Error(), "managed_by") {
		t.Fatalf("invalid owner accepted: %v", err)
	}
	root := t.TempDir()
	path := filepath.Join(root, "p.yaml")
	if err := os.WriteFile(path, []byte("name: p\nrepo: {owner: me, name: app}\nservice: {name: gha-p.service}\ndocker: {container_name_prefix: gha-p}\nmanaged_by: fleet\nfuture_field: ignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := LoadProfile(path)
	if err != nil || p.ManagedBy != ManagedByFleet {
		t.Fatalf("load=%+v err=%v", p, err)
	}
}

func TestBulkMigrationSkipsFleetProfilesAndLeavesBytesUntouched(t *testing.T) {
	dir := t.TempDir()
	fleetRaw := "name: fleet\nrepo: {owner: me, name: app}\nservice: {name: gha-fleet.service}\ndocker:\n  container_name_prefix: gha-fleet\n  volumes: [/var/run/docker.sock:/var/run/docker.sock]\nmanaged_by: fleet\n"
	localRaw := "name: local\nrepo: {owner: me, name: app}\nservice: {name: gha-local.service}\ndocker:\n  container_name_prefix: gha-local\n  volumes: [/var/run/docker.sock:/var/run/docker.sock]\n"
	fleetPath, localPath := filepath.Join(dir, "fleet.yaml"), filepath.Join(dir, "local.yaml")
	if err := os.WriteFile(fleetPath, []byte(fleetRaw), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(localPath, []byte(localRaw), 0o640); err != nil {
		t.Fatal(err)
	}
	results, err := MigrateProfilesAccessMode(dir)
	if err != nil {
		t.Fatal(err)
	}
	foundSkip := false
	for _, result := range results {
		if result.Path == fleetPath && result.Status == ProfileMigrationSkipped && strings.Contains(result.Message, "fleet") {
			foundSkip = true
		}
	}
	if !foundSkip {
		t.Fatalf("fleet not skipped: %+v", results)
	}
	after, err := os.ReadFile(fleetPath)
	if err != nil || string(after) != fleetRaw {
		t.Fatalf("fleet file rewritten: %s %v", after, err)
	}
	local, _ := os.ReadFile(localPath)
	if !strings.Contains(string(local), "access_mode") {
		t.Fatalf("local file not migrated: %s", local)
	}
}

func TestGlobalConfigUnitTemplate(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte("systemd:\n  unit_template: /etc/gha-runner-tui/unit.tmpl\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadGlobalConfig(path)
	if err != nil || cfg.Systemd.UnitTemplate != "/etc/gha-runner-tui/unit.tmpl" {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
}
