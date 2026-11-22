package loop

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	policy, exists, err := s.readHostPolicy()
	if err != nil || !exists {
		return policy.MaxJobs, err
	}
	// This gate is only used for new launches under the host lock. Adoption and
	// monitoring of existing containers continue even when root space is low.
	if checkRootSpace && policy.MinRootFreeBytes > 0 {
		available, err := s.freeBytes("/")
		if err != nil {
			return 0, fmt.Errorf("check root disk space: %w", err)
		}
		if available < policy.MinRootFreeBytes {
			return 0, fmt.Errorf("root disk space below admission floor: available=%d required=%d", available, policy.MinRootFreeBytes)
		}
	}
	return policy.MaxJobs, nil
}

type hostPolicy struct {
	MaxJobs          int    `json:"max_jobs"`
	MinRootFreeBytes uint64 `json:"min_root_free_bytes"`
	// Optional Docker data-root gate: below ReclaimBelow, rebuildable Docker
	// caches are pruned; below MinFree after that, new launches wait.
	DockerRootDir           string `json:"docker_root_dir"`
	MinDockerFreeBytes      uint64 `json:"min_docker_free_bytes"`
	DockerReclaimBelowBytes uint64 `json:"docker_reclaim_below_bytes"`
	// Opt-in labels whose orphaned volumes and images the reclaim may delete.
	DockerReclaimLabels []string `json:"docker_reclaim_labels"`
}

// readHostPolicy returns exists=false (and the legacy single-job capacity)
// when no policy file is present.
func (s *Supervisor) readHostPolicy() (hostPolicy, bool, error) {
	path := s.HostPolicyPath
	if path == "" {
		path = "/etc/gha-runner-tui/host-policy.json"
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return hostPolicy{MaxJobs: 1}, false, nil
	}
	if err != nil {
		return hostPolicy{}, true, fmt.Errorf("read host policy: %w", err)
	}
	var policy hostPolicy
	if err := json.Unmarshal(data, &policy); err != nil {
		return hostPolicy{}, true, fmt.Errorf("invalid host policy: %w", err)
	}
	if policy.MaxJobs < 1 || policy.MaxJobs > 64 {
		return hostPolicy{}, true, errors.New("host max_jobs must be between 1 and 64")
	}
	if policy.DockerRootDir == "" {
		if policy.MinDockerFreeBytes > 0 || policy.DockerReclaimBelowBytes > 0 {
			return hostPolicy{}, true, errors.New("host docker disk thresholds require docker_root_dir")
		}
	} else if !filepath.IsAbs(policy.DockerRootDir) {
		return hostPolicy{}, true, errors.New("host docker_root_dir must be absolute")
	}
	if len(policy.DockerReclaimLabels) > 0 && (policy.DockerRootDir == "" || policy.DockerReclaimBelowBytes == 0) {
		return hostPolicy{}, true, errors.New("host docker_reclaim_labels requires docker_root_dir and docker_reclaim_below_bytes")
	}
	for _, label := range policy.DockerReclaimLabels {
		// A bare key only. The manager's own labels mark live runner state.
		if label == "" || strings.ContainsAny(label, "= \t\r\n") || strings.HasPrefix(label, "io.gha-runner-tui") {
			return hostPolicy{}, true, errors.New("host docker_reclaim_labels must be label keys outside io.gha-runner-tui")
		}
	}
	return policy, true, nil
}

func (s *Supervisor) freeBytes(path string) (uint64, error) {
	if s.FreeBytes != nil {
		return s.FreeBytes(path)
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return 0, err
	}
	return uint64(fs.Bavail) * uint64(fs.Bsize), nil
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
