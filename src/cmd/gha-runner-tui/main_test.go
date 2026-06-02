package main

import (
	"context"
	"testing"
)

func TestParseSyncArgsRequiresProfileOrConfig(t *testing.T) {
	t.Parallel()

	opts, err := parseSyncArgs([]string{"--profile", "/tmp/profile.yaml"})
	if err != nil {
		t.Fatalf("parseSyncArgs returned error: %v", err)
	}
	if opts.profilePath != "/tmp/profile.yaml" {
		t.Fatalf("unexpected profile path: %q", opts.profilePath)
	}
}

type fakeSyncer struct {
	profilePath string
	configCalls int
}

func (f *fakeSyncer) SyncProfilePath(_ context.Context, profilePath string) error {
	f.profilePath = profilePath
	return nil
}

func (f *fakeSyncer) SyncConfigProfiles(context.Context) error {
	f.configCalls++
	return nil
}

func TestRunSyncWithProfilePath(t *testing.T) {
	t.Parallel()

	fake := &fakeSyncer{}
	err := runSyncWith(context.Background(), syncOptions{profilePath: "/tmp/profile.yaml"}, fake)
	if err != nil {
		t.Fatalf("runSyncWith returned error: %v", err)
	}
	if fake.profilePath != "/tmp/profile.yaml" {
		t.Fatalf("expected profile sync, got %q", fake.profilePath)
	}
	if fake.configCalls != 0 {
		t.Fatalf("did not expect config sync, got %d", fake.configCalls)
	}
}
