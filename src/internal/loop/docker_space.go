package loop

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gha-runner-tui/internal/config"
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

type dockerSpaceError struct{ free, outstanding, need uint64 }

func (e *dockerSpaceError) Error() string {
	return fmt.Sprintf("%v: free=%s outstanding=%s need=%s", errDockerSpace, gib(e.free), gib(e.outstanding), gib(e.need))
}

func (e *dockerSpaceError) Unwrap() error { return errDockerSpace }

func gib(n uint64) string { return fmt.Sprintf("%.1fGiB", float64(n)/(1<<30)) }

// dockerSpaceForLaunch runs under the host lock, right before a launch. It
// prunes rebuildable Docker data (see Client.ReclaimCaches). Tagged images,
// named volumes and containers go only under an opt-in reclaim label (see
// Client.ReclaimLabeled). The launch floor is the greater of the
// host policy minimum and the profile's disk reservation; running containers
// that declared a reservation and have not yet written that much count against
// the remaining free space.
func (s *Supervisor) dockerSpaceForLaunch(ctx context.Context, p config.Profile) error {
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
		var reclaimed []string
		var reclaimErrs []error
		// Leftovers first: they are the largest, and their layers then prune.
		for _, label := range policy.DockerReclaimLabels {
			removed, labelErr := s.Docker.ReclaimLabeled(reclaimCtx, label, s.now())
			reclaimed = append(reclaimed, removed...)
			reclaimErrs = append(reclaimErrs, labelErr)
		}
		reclaimErr := errors.Join(append(reclaimErrs, s.Docker.ReclaimCaches(reclaimCtx))...)
		cancel()
		before := free
		if free, err = s.freeBytes(policy.DockerRootDir); err != nil {
			return fmt.Errorf("check docker disk space: %w", err)
		}
		msg := fmt.Sprintf("docker cache reclaim: free %s -> %s", gib(before), gib(free))
		if len(reclaimed) > 0 {
			msg += "; removed " + strings.Join(reclaimed, ", ")
		}
		if reclaimErr != nil {
			msg += fmt.Sprintf(" (partial: %v)", reclaimErr)
		}
		s.diagnostic(dockerpkg.ContainerInfo{}, msg)
	}

	queryCtx, cancelQuery := context.WithTimeout(ctx, waitSubcallTimeout)
	reservations, err := s.Docker.DiskReservations(queryCtx)
	cancelQuery()
	if err != nil {
		return fmt.Errorf("check docker disk reservations: %w", err)
	}
	outstanding := outstandingDiskReserve(reservations)
	if len(reservations) > 0 {
		// The batched inspect above takes time while labeled containers keep
		// writing. The earlier free reading would overcredit that growth, so
		// keep the lower of the two readings.
		finalFree, err := s.freeBytes(policy.DockerRootDir)
		if err != nil {
			return fmt.Errorf("check docker disk space: %w", err)
		}
		if finalFree < free {
			free = finalFree
		}
	}

	need := policy.MinDockerFreeBytes
	if p.Docker.DiskReserveBytes > need {
		need = p.Docker.DiskReserveBytes
	}
	if belowFloor(free, outstanding, need) {
		return &dockerSpaceError{free: free, outstanding: outstanding, need: need}
	}
	return nil
}

// belowFloor compares free minus outstanding with need without unsigned
// underflow or overflow. When need is zero, exhausted reservations alone are
// not a reason to refuse a launch.
func belowFloor(free, outstanding, need uint64) bool {
	if free < outstanding {
		return need > 0
	}
	return free-outstanding < need
}

// outstandingDiskReserve sums, per labeled running container, only the part of
// its reservation that it has not yet consumed with writable-layer data.
func outstandingDiskReserve(reservations []dockerpkg.DiskReservation) uint64 {
	var total uint64
	for _, reservation := range reservations {
		if reservation.Reserve <= reservation.SizeRw {
			continue
		}
		remaining := reservation.Reserve - reservation.SizeRw
		if total > math.MaxUint64-remaining {
			return math.MaxUint64
		}
		total += remaining
	}
	return total
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
