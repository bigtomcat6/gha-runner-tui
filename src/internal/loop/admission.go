package loop

import (
	"time"

	"gha-runner-tui/internal/config"
	"gha-runner-tui/internal/state"
)

func (s *Supervisor) loadAdmission(p config.Profile) (state.AdmissionPolicy, error) {
	policy, err := state.LoadAdmissionPolicy(p.Loop.StateFile)
	if err != nil || policy != s.admissionPolicy {
		s.delayedJobs = nil
	}
	if err == nil {
		s.admissionPolicy = policy
	}
	return policy, err
}

// Eligible IDs must still appear in the latest queue scan. Each ID gets its
// own grace period; a replacement job cannot inherit a previous job's age.
func (s *Supervisor) admissionCandidates(ids []int64, policy state.AdmissionPolicy) []int64 {
	if policy.Mode != state.AdmissionDelayed {
		return ids
	}
	now := s.now()
	next := make(map[int64]time.Time, len(ids))
	ready := make([]int64, 0, len(ids))
	for _, id := range ids {
		first, seen := s.delayedJobs[id]
		if !seen {
			first = now
		}
		next[id] = first
		if now.Sub(first) >= time.Duration(policy.DelaySeconds)*time.Second {
			ready = append(ready, id)
		}
	}
	s.delayedJobs = next
	return ready
}

// Recheck after lock acquisition and registration-token retrieval, so a CLI
// switch during either slow operation prevents a new runner launch.
func (s *Supervisor) recheckAdmission(p config.Profile, expected state.AdmissionPolicy) (state.LoopStatus, error) {
	policy, err := s.loadAdmission(p)
	if err != nil {
		return state.LoopSleeping, err
	}
	if policy == expected {
		return "", nil
	}
	switch policy.Mode {
	case state.AdmissionPaused:
		return state.LoopPaused, nil
	case state.AdmissionDelayed:
		return state.LoopWaitingDelay, nil
	default:
		return state.LoopSleeping, nil
	}
}
