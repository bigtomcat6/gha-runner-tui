package state

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestAdmissionOverridePreservesRunningJobStateAndInvalidWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.json")
	const running = `{"state":"running-job","last_container_name":"gha-p-current"}`
	if err := os.WriteFile(path, []byte(running), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := SaveAdmissionPolicy(path, AdmissionPolicy{Mode: AdmissionPaused, DelaySeconds: 60}); err != nil {
		t.Fatal(err)
	}
	if err := SaveAdmissionPolicy(path, AdmissionPolicy{Mode: 3, DelaySeconds: 60}); err == nil {
		t.Fatal("invalid mode accepted")
	}
	policy, err := LoadAdmissionPolicy(path)
	if err != nil || policy.Mode != AdmissionPaused {
		t.Fatalf("invalid write replaced paused policy: policy=%+v err=%v", policy, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != running {
		t.Fatalf("running job state overwritten: data=%q err=%v", data, err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 2 {
		t.Fatalf("temporary files leaked: entries=%v err=%v", entries, err)
	}
}

func TestAdmissionAtomicSwitchNeverExposesPartialPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "p.json")
	if err := SaveAdmissionPolicy(path, AdmissionPolicy{Mode: AdmissionPaused, DelaySeconds: 60}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 50; i++ {
			if err := SaveAdmissionPolicy(path, AdmissionPolicy{Mode: AdmissionMode(2 * (i % 2)), DelaySeconds: 90}); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	var readErr error
	for i := 0; i < 200; i++ {
		policy, err := LoadAdmissionPolicy(path)
		if err != nil || (policy.Mode != AdmissionPaused && policy.Mode != AdmissionDelayed) {
			readErr = fmt.Errorf("partial or missing policy: %+v, %v", policy, err)
			break
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
}

func TestAdmissionMissingOverrideDefaultsToNormalWithoutWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "p.json")
	policy, err := LoadAdmissionPolicy(path)
	if err != nil || policy.Mode != AdmissionNormal || policy.DelaySeconds != 60 {
		t.Fatalf("policy=%+v err=%v", policy, err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("read created state directory: %v", err)
	}
}
