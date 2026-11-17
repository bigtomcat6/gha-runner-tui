package loop

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"syscall"
)

// One host policy, shared by every profile and checked under the launch lock.
// Missing policy preserves legacy single-job behavior. Invalid policy blocks admission.
func (s *Supervisor) hostCapacity() (int, error) {
	return s.readHostCapacity(false)
}

func (s *Supervisor) hostCapacityForLaunch() (int, error) {
	return s.readHostCapacity(true)
}

func (s *Supervisor) readHostCapacity(checkRootSpace bool) (int, error) {
	path := s.HostPolicyPath
	if path == "" {
		path = "/etc/gha-runner-tui/host-policy.json"
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 1, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read host policy: %w", err)
	}
	var policy struct {
		MaxJobs          int    `json:"max_jobs"`
		MinRootFreeBytes uint64 `json:"min_root_free_bytes"`
	}
	if err := json.Unmarshal(data, &policy); err != nil {
		return 0, fmt.Errorf("invalid host policy: %w", err)
	}
	if policy.MaxJobs < 1 || policy.MaxJobs > 64 {
		return 0, errors.New("host max_jobs must be between 1 and 64")
	}
	// This gate is only used for new launches under the host lock. Adoption and
	// monitoring of existing containers continue even when root space is low.
	if checkRootSpace && policy.MinRootFreeBytes > 0 {
		var fs syscall.Statfs_t
		if err := syscall.Statfs("/", &fs); err != nil {
			return 0, fmt.Errorf("check root disk space: %w", err)
		}
		available := uint64(fs.Bavail) * uint64(fs.Bsize)
		if available < policy.MinRootFreeBytes {
			return 0, fmt.Errorf("root disk space below admission floor: available=%d required=%d", available, policy.MinRootFreeBytes)
		}
	}
	return policy.MaxJobs, nil
}

// releaseOnce permits early release without double-unlocking in deferred cleanup.
func releaseOnce(unlock func() error) func() error {
	released := false
	return func() error {
		if released {
			return nil
		}
		released = true
		return unlock()
	}
}
