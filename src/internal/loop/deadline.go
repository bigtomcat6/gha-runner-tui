package loop

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"gha-runner-tui/internal/config"
)

// sessionDeadlineMinRemaining is 90 minutes of work plus a 10-minute buffer.
// A configured deadline with less remaining is not enough to admit new work.
const sessionDeadlineMinRemaining = 100 * time.Minute

// errSessionDeadline marks an expected refusal: the optional protection is
// enabled but cannot prove enough remaining time.
var errSessionDeadline = errors.New("session deadline protection refused new work")

type sessionDeadlineError struct {
	reason string
}

func (e *sessionDeadlineError) Error() string {
	return "session deadline protection: " + e.reason
}

func (e *sessionDeadlineError) Unwrap() error { return errSessionDeadline }

type sessionDeadlinePayload struct {
	Deadline string `json:"deadline"`
}

// readSessionDeadline re-reads the file on every call so an external
// coordinator can update the deadline without restarting the loop. Missing,
// unreadable, invalid, expired, or below-floor deadlines refuse. The returned
// deadline is zero when the file cannot be trusted at all; a non-zero deadline
// with an error means the time was parsed but is no longer usable.
func readSessionDeadline(path string, now time.Time) (time.Time, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return time.Time{}, &sessionDeadlineError{reason: "deadline file is missing"}
		}
		return time.Time{}, &sessionDeadlineError{reason: "deadline file is unreadable"}
	}
	var payload sessionDeadlinePayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return time.Time{}, &sessionDeadlineError{reason: "deadline file is not valid JSON"}
	}
	raw := strings.TrimSpace(payload.Deadline)
	if raw == "" {
		return time.Time{}, &sessionDeadlineError{reason: "deadline is missing"}
	}
	deadline, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, &sessionDeadlineError{reason: "deadline is not a valid RFC3339 time"}
	}
	remaining := deadline.Sub(now)
	switch {
	case remaining <= 0:
		return deadline, &sessionDeadlineError{reason: "deadline has expired"}
	case remaining < sessionDeadlineMinRemaining:
		return deadline, &sessionDeadlineError{reason: "remaining time is below the 100-minute admission floor"}
	}
	return deadline, nil
}

// sessionDeadlineAdmission keeps the unconfigured loop unchanged. Only the
// admission path calls this; startup adoption must still observe a runner that
// was created before the threshold.
func sessionDeadlineAdmission(p config.Profile, now time.Time) error {
	if p.Loop.SessionDeadlineFile == "" {
		return nil
	}
	_, err := readSessionDeadline(p.Loop.SessionDeadlineFile, now)
	return err
}

func (s *Supervisor) sessionDeadlineAdmission(p config.Profile) error {
	if p.Loop.SessionDeadlineFile == "" {
		return nil
	}
	return sessionDeadlineAdmission(p, s.now())
}

// sessionDeadlineExhausted reports a parsed deadline at or below the floor.
// Unknown or unreadable deadlines are not treated as exhausted: without a
// trustworthy time, withdrawal must not guess.
func sessionDeadlineExhausted(path string, now time.Time) bool {
	if path == "" {
		return false
	}
	deadline, err := readSessionDeadline(path, now)
	return err != nil && !deadline.IsZero()
}

// sessionDeadlineReached keeps the unconfigured loop free of extra clock and
// file reads, preserving its existing behavior exactly.
func (s *Supervisor) sessionDeadlineReached(p config.Profile) bool {
	if p.Loop.SessionDeadlineFile == "" {
		return false
	}
	return sessionDeadlineExhausted(p.Loop.SessionDeadlineFile, s.now())
}
