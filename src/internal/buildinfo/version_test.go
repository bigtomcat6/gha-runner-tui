package buildinfo

import (
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

func TestFormatBuildSettings(t *testing.T) {
	info := &debug.BuildInfo{GoVersion: "go1.24.5", Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "abc123"}, {Key: "vcs.time", Value: "2026-10-07T00:00:00Z"}, {Key: "vcs.modified", Value: "true"},
	}}
	if got := Format(info); got != "revision: abc123-dirty\ntime: 2026-10-07T00:00:00Z\ngo: go1.24.5\n" {
		t.Fatalf("got %q", got)
	}
	info.Settings[2].Value = "false"
	if got := Format(info); got != "revision: abc123\ntime: 2026-10-07T00:00:00Z\ngo: go1.24.5\n" {
		t.Fatalf("clean=%q", got)
	}
	for _, info := range []*debug.BuildInfo{nil, {}, {Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: ""}, {Key: "vcs.time", Value: ""}}}} {
		if got := Format(info); got != "revision: unknown\ntime: unknown\ngo: "+runtime.Version()+"\n" {
			t.Fatalf("fallback=%q", got)
		}
	}
}

func TestCurrentUsesBuildInfo(t *testing.T) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		info = nil
	}
	if got := Current(); got != Format(info) || !strings.Contains(got, "go: ") {
		t.Fatalf("Current=%q", got)
	}
}
