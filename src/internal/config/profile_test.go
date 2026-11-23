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
  visibility: private
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
	if profile.Runner.Ephemeral {
		t.Fatal("expected legacy organization profile to default ephemeral=false")
	}
	if len(profile.Runner.WatchRepositories) != 0 {
		t.Fatalf("expected no watch repositories, got %v", profile.Runner.WatchRepositories)
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
  visibility: private
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

func TestLoadProfileRejectsUnsafeManagedNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name: "unsafe profile name",
			content: `
name: ../escape
repo:
  owner: bigtomcat6
  name: remind-me
service:
  name: gha-remind-me-swift.service
docker:
  container_name_prefix: gha-remind-me-swift
loop:
  state_file: /var/lib/gha-runner-tui/state/remind-me-swift.json
`,
			want: "name",
		},
		{
			name: "unsafe service name",
			content: `
name: remind-me-swift
repo:
  owner: bigtomcat6
  name: remind-me
service:
  name: ../gha-remind-me-swift.service
docker:
  container_name_prefix: gha-remind-me-swift
loop:
  state_file: /var/lib/gha-runner-tui/state/remind-me-swift.json
`,
			want: "service.name",
		},
		{
			name: "unsafe container prefix",
			content: `
name: remind-me-swift
repo:
  owner: bigtomcat6
  name: remind-me
service:
  name: gha-remind-me-swift.service
docker:
  container_name_prefix: ../gha-remind-me-swift
loop:
  state_file: /var/lib/gha-runner-tui/state/remind-me-swift.json
`,
			want: "docker.container_name_prefix",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "bad.yaml")
			if err := os.WriteFile(path, []byte(strings.TrimPrefix(tt.content, "\n")), 0o600); err != nil {
				t.Fatalf("WriteFile returned error: %v", err)
			}

			_, err := LoadProfile(path)
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected %q error, got %v", tt.want, err)
			}
		})
	}
}

func TestLoadProfileOptionalSessionDeadlineFile(t *testing.T) {
	t.Parallel()

	write := func(t *testing.T, extra string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "profile.yaml")
		content := "name: remind-me\nrepo: {owner: me, name: app}\nservice: {name: gha-remind-me.service}\ndocker: {container_name_prefix: gha-remind-me}\nloop:\n  state_file: /var/lib/gha-runner-tui/state/remind-me.json\n" + extra
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	profile, err := LoadProfile(write(t, ""))
	if err != nil || profile.Loop.SessionDeadlineFile != "" {
		t.Fatalf("unconfigured deadline=%q err=%v", profile.Loop.SessionDeadlineFile, err)
	}
	profile, err = LoadProfile(write(t, "  session_deadline_file: /run/gha-runner-tui/session-deadline.json\n"))
	if err != nil || profile.Loop.SessionDeadlineFile != "/run/gha-runner-tui/session-deadline.json" {
		t.Fatalf("configured deadline=%q err=%v", profile.Loop.SessionDeadlineFile, err)
	}
}

func TestLoadProfileSessionDeadlineMinRemaining(t *testing.T) {
	t.Parallel()

	write := func(t *testing.T, extra string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "profile.yaml")
		content := "name: remind-me\nrepo: {owner: me, name: app}\nservice: {name: gha-remind-me.service}\ndocker: {container_name_prefix: gha-remind-me}\nloop:\n  state_file: /var/lib/gha-runner-tui/state/remind-me.json\n" + extra
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	fileLine := "  session_deadline_file: /run/gha-runner-tui/session-deadline.json\n"
	for _, tc := range []struct {
		name    string
		extra   string
		want    int
		wantErr string
	}{
		{name: "omitted default", extra: fileLine, want: 0},
		{name: "explicit zero default", extra: fileLine + "  session_deadline_min_remaining_minutes: 0\n", want: 0},
		{name: "zero without file", extra: "  session_deadline_min_remaining_minutes: 0\n", want: 0},
		{name: "minimum inclusive", extra: fileLine + "  session_deadline_min_remaining_minutes: 20\n", want: 20},
		{name: "maximum inclusive", extra: fileLine + "  session_deadline_min_remaining_minutes: 480\n", want: 480},
		{name: "configured", extra: fileLine + "  session_deadline_min_remaining_minutes: 40\n", want: 40},
		{name: "below minimum", extra: fileLine + "  session_deadline_min_remaining_minutes: 19\n", wantErr: "at least 20"},
		{name: "above maximum", extra: fileLine + "  session_deadline_min_remaining_minutes: 481\n", wantErr: "at most 480"},
		{name: "negative", extra: fileLine + "  session_deadline_min_remaining_minutes: -5\n", wantErr: "at least 20"},
		{name: "missing file", extra: "  session_deadline_min_remaining_minutes: 40\n", wantErr: "requires loop.session_deadline_file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile, err := LoadProfile(write(t, tc.extra))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected %q error, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil || profile.Loop.SessionDeadlineMinRemainingMinutes != tc.want {
				t.Fatalf("minutes=%d err=%v", profile.Loop.SessionDeadlineMinRemainingMinutes, err)
			}
		})
	}
}

func TestLoadProfileDiskReserveBytes(t *testing.T) {
	t.Parallel()

	write := func(t *testing.T, reserve string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "profile.yaml")
		content := "name: remind-me\nrepo: {owner: me, name: app}\nservice: {name: gha-remind-me.service}\ndocker:\n  container_name_prefix: gha-remind-me\n  disk_reserve_bytes: " + reserve + "\nloop:\n  state_file: /var/lib/gha-runner-tui/state/remind-me.json\n"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	profile, err := LoadProfile(write(t, "1099511627776"))
	if err != nil || profile.Docker.DiskReserveBytes != 1099511627776 {
		t.Fatalf("1 TiB reserve=%d err=%v", profile.Docker.DiskReserveBytes, err)
	}
	profile, err = LoadProfile(write(t, "1125899906842624"))
	if err != nil || profile.Docker.DiskReserveBytes != 1125899906842624 {
		t.Fatalf("1 PiB reserve=%d err=%v", profile.Docker.DiskReserveBytes, err)
	}
	if _, err := LoadProfile(write(t, "1125899906842625")); err == nil || !strings.Contains(err.Error(), "docker.disk_reserve_bytes") {
		t.Fatalf("expected disk_reserve_bytes cap error, got %v", err)
	}
	profile, err = LoadProfile(write(t, "null"))
	if err != nil || profile.Docker.DiskReserveBytes != 0 {
		t.Fatalf("null reserve=%d err=%v", profile.Docker.DiskReserveBytes, err)
	}
	for _, reserve := range []string{"1.5", "1.0", "1e3", "-0.5"} {
		if _, err := LoadProfile(write(t, reserve)); err == nil || !strings.Contains(err.Error(), "must be an integer") {
			t.Fatalf("float reserve %q must be rejected as a non-integer, got %v", reserve, err)
		}
	}
	if _, err := LoadProfile(write(t, "-1")); err == nil {
		t.Fatalf("negative reserve must be rejected")
	}

	body := func(docker string) string {
		return "name: remind-me\nrepo: {owner: me, name: app}\nservice: {name: gha-remind-me.service}\ndocker:\n" + docker + "loop:\n  state_file: /var/lib/gha-runner-tui/state/remind-me.json\n"
	}
	unknown := filepath.Join(t.TempDir(), "unknown.yaml")
	if err := os.WriteFile(unknown, []byte(body("  container_name_prefix: gha-remind-me\n  disk_reserve_bytes: 1024\n  future_option: true\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	if profile, err := LoadProfile(unknown); err != nil || profile.Docker.DiskReserveBytes != 1024 {
		t.Fatalf("unknown docker field must stay ignored: reserve=%d err=%v", profile.Docker.DiskReserveBytes, err)
	}
	duplicate := filepath.Join(t.TempDir(), "duplicate.yaml")
	if err := os.WriteFile(duplicate, []byte(body("  container_name_prefix: gha-remind-me\n  disk_reserve_bytes: 1024\n  disk_reserve_bytes: 2048\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProfile(duplicate); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("duplicate disk_reserve_bytes must stay an error, got %v", err)
	}
}
