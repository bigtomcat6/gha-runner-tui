package loop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	dockerpkg "gha-runner-tui/internal/docker"
)

const (
	defaultReclaimStampPath = "/run/gha-runner-tui/docker-reclaim.stamp"
	// One host-wide prune per interval: every loop reaches this gate, and a
	// disk that stays low must not turn each poll into a prune.
	dockerReclaimInterval = 10 * time.Minute
	dockerReclaimTimeout  = 3 * time.Minute
)

// errDockerSpace marks an expected refusal: the Docker data root is too full
// for a new runner, so the job is left for another host.
var errDockerSpace = errors.New("docker disk space below launch floor")

type dockerSpaceError struct{ free, floor uint64 }

func (e *dockerSpaceError) Error() string {
	return fmt.Sprintf("%v: free=%s floor=%s", errDockerSpace, gib(e.free), gib(e.floor))
}

func (e *dockerSpaceError) Unwrap() error { return errDockerSpace }

func gib(n uint64) string { return fmt.Sprintf("%.1fGiB", float64(n)/(1<<30)) }

// dockerSpaceForLaunch runs under the host lock, right before a launch. It
// prunes only rebuildable Docker data (see Client.ReclaimCaches), never tagged
// images, named volumes or containers.
func (s *Supervisor) dockerSpaceForLaunch(ctx context.Context) error {
	policy, exists, err := s.readHostPolicy()
	if err != nil || !exists || policy.DockerRootDir == "" {
		return err
	}
	free, err := s.freeBytes(policy.DockerRootDir)
	if err != nil {
		return fmt.Errorf("check docker disk space: %w", err)
	}
	if free < policy.DockerReclaimBelowBytes && s.reclaimDue() {
		reclaimCtx, cancel := context.WithTimeout(ctx, dockerReclaimTimeout)
		reclaimErr := s.Docker.ReclaimCaches(reclaimCtx)
		cancel()
		before := free
		if free, err = s.freeBytes(policy.DockerRootDir); err != nil {
			return fmt.Errorf("check docker disk space: %w", err)
		}
		msg := fmt.Sprintf("docker cache reclaim: free %s -> %s", gib(before), gib(free))
		if reclaimErr != nil {
			msg += fmt.Sprintf(" (partial: %v)", reclaimErr)
		}
		s.diagnostic(dockerpkg.ContainerInfo{}, msg)
	}
	if free < policy.MinDockerFreeBytes {
		return &dockerSpaceError{free: free, floor: policy.MinDockerFreeBytes}
	}
	return nil
}

// reclaimDue claims the host-wide reclaim slot. Callers hold the host lock,
// so reading and touching the stamp cannot race another loop.
func (s *Supervisor) reclaimDue() bool {
	path := s.ReclaimStampPath
	if path == "" {
		path = defaultReclaimStampPath
	}
	now := s.now()
	if info, err := os.Stat(path); err == nil && now.Sub(info.ModTime()) < dockerReclaimInterval {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	_ = f.Close()
	return os.Chtimes(path, now, now) == nil
}
