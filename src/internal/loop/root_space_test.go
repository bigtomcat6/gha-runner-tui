package loop

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHostRootSpaceFloor(t *testing.T) {
	for _, tc := range []struct {
		name, policy string
		blocked      bool
	}{
		{"disabled by default", `{"max_jobs":4}`, false},
		{"above floor", `{"max_jobs":4,"min_root_free_bytes":1}`, false},
		{"below floor", `{"max_jobs":4,"min_root_free_bytes":18446744073709551615}`, true},
		{"negative floor fails closed", `{"max_jobs":4,"min_root_free_bytes":-1}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "policy.json")
			if err := os.WriteFile(path, []byte(tc.policy), 0600); err != nil {
				t.Fatal(err)
			}
			s := &Supervisor{HostPolicyPath: path}
			capacity, err := s.hostCapacityForLaunch()
			if (err != nil) != tc.blocked || (!tc.blocked && capacity != 4) {
				t.Fatalf("capacity=%d error=%v blocked=%t", capacity, err, tc.blocked)
			}
		})
	}
}

func TestLowRootSpaceDoesNotBlockAdoptionCapacity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, []byte(`{"max_jobs":4,"min_root_free_bytes":18446744073709551615}`), 0600); err != nil {
		t.Fatal(err)
	}
	s := &Supervisor{HostPolicyPath: path}
	if capacity, err := s.hostCapacity(); err != nil || capacity != 4 {
		t.Fatalf("existing-container capacity must ignore launch floor: capacity=%d error=%v", capacity, err)
	}
}
