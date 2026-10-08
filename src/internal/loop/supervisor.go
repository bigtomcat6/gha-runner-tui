package loop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	rand "math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gha-runner-tui/internal/config"
	dockerpkg "gha-runner-tui/internal/docker"
	gh "gha-runner-tui/internal/github"
	"gha-runner-tui/internal/state"
)

type Supervisor struct {
	ProfilePath string
	Docker      dockerpkg.Client
	GitHub      gh.Client

	// Now and Sleep are injectable for deterministic tests. A nil Now uses
	// time.Now; a nil Sleep uses a context-aware timer.
	Now         func() time.Time
	Sleep       func(context.Context, time.Duration) error
	TryLock     func() (unlock func() error, acquired bool, err error)
	Jitter      func(time.Duration) time.Duration
	Stderr      io.Writer // nil uses os.Stderr for journal-visible diagnostics
	skippedJobs map[int64]time.Time
	// restartCount counts actual RunDetached attempts in this process, not polls
	// or completed jobs. Adoption and draining never change it.
	restartCount int

	// stopDeadline is the single drain budget shared by every container this
	// Supervisor adopts. It is set once, on the first stop observation.
	stopDeadline time.Time
}

type registrationTokenClient interface {
	CreateRegistrationToken(ctx context.Context, owner, repo string) (string, error)
	CreateOrgRegistrationToken(ctx context.Context, org string) (string, error)
}

type stateRecord struct {
	Profile           string  `json:"profile"`
	Repo              string  `json:"repo"`
	State             string  `json:"state"`
	Health            string  `json:"health"`
	LastTransitionAt  string  `json:"last_transition_at"`
	LastRunnerName    string  `json:"last_runner_name,omitempty"`
	LastContainerID   string  `json:"last_container_id,omitempty"`
	LastContainerName string  `json:"last_container_name,omitempty"`
	LastExitCode      *int    `json:"last_exit_code,omitempty"`
	LastError         *string `json:"last_error,omitempty"`
	RestartCount      int     `json:"restart_count"`
}

var errStateWrite = errors.New("write loop state failed")

func (s *Supervisor) Run(ctx context.Context) error {
	s.restartCount = 0
	profile, err := config.LoadProfile(s.ProfilePath)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(profile.Loop.StateFile), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(profile.Loop.LogDir, 0o755); err != nil {
		return err
	}

	settings, err := validateLoopProfile(profile)
	if err != nil {
		if writeErr := s.writeCycle(profile, cycleResult{state: state.LoopFailed}, err); writeErr != nil {
			return writeErr
		}
		<-ctx.Done()
		return nil
	}
	s.defaults()
	s.skippedJobs = make(map[int64]time.Time)
	s.stopDeadline = time.Time{}
	backoff := max(profile.Loop.BackoffSeconds, 1)
	maxBackoff := max(profile.Loop.MaxBackoffSeconds, backoff)
	startup := true
	for ctx.Err() == nil {
		result := cycleResult{state: state.LoopSleeping}
		if startup {
			err = s.adoptManaged(ctx, profile, settings)
			if err == nil {
				startup = false
			}
		}
		if err == nil && ctx.Err() == nil {
			result, err = s.runCycle(ctx, profile, settings)
		}
		if errors.Is(err, errStateWrite) {
			return err
		}
		if ctx.Err() != nil {
			break
		}
		var wait time.Duration
		if err != nil {
			result.state = state.LoopBackoff
			wait = time.Duration(backoff) * time.Second
			backoff = min(backoff*2, maxBackoff)
		} else {
			backoff = max(profile.Loop.BackoffSeconds, 1)
			wait = s.Jitter(settings.poll)
		}
		if writeErr := s.writeCycle(profile, result, err); writeErr != nil {
			return writeErr
		}
		if sleepErr := s.sleep(ctx, wait); sleepErr != nil {
			if ctx.Err() == nil {
				return sleepErr
			}
			break
		}
		err = nil
	}
	// Cancellation may interrupt startup or an unverified run. Never infer an
	// empty slot from that: check this profile's labels under one shared budget.
	s.beginStop()
	if err := s.adoptManaged(ctx, profile, settings); err != nil {
		if errors.Is(err, errStateWrite) {
			return err
		}
		s.noteWaitIssue(profile, dockerpkg.ContainerInfo{}, "stop adoption failed; containers retained")
	}
	return nil
}

type cycleResult struct {
	runnerName    string
	containerID   string
	containerName string
	exitCode      *int
	state         state.LoopStatus
}

func (s *Supervisor) runCycle(ctx context.Context, p config.Profile, settings loopSettings) (result cycleResult, err error) {
	s.defaults()
	result.state = state.LoopSleeping
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if _, err := s.cleanupManagedTerminals(ctx, p); err != nil {
		return result, err
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	repos := make([]gh.Repository, 0, len(settings.watch))
	for _, slug := range settings.watch {
		owner, repo, _ := splitOwnerRepo(slug)
		repos = append(repos, gh.Repository{Owner: owner, Name: repo})
	}
	queryCtx, cancelQuery := context.WithTimeout(ctx, waitSubcallTimeout)
	jobs, err := s.GitHub.QueuedJobs(queryCtx, repos, p.Runner.Labels)
	cancelQuery()
	if err != nil {
		return result, err
	}
	for id, until := range s.skippedJobs {
		if !s.now().Before(until) {
			delete(s.skippedJobs, id)
		}
	}
	candidates := make([]int64, 0, len(jobs))
	for _, job := range jobs {
		if _, skipped := s.skippedJobs[job.ID]; !skipped {
			candidates = append(candidates, job.ID)
		}
	}
	if len(candidates) == 0 {
		return result, nil
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	unlock, acquired, err := s.TryLock()
	if err != nil {
		return result, err
	}
	if !acquired {
		result.state = state.LoopWaitingHost
		return result, nil
	}
	defer func() { err = errors.Join(err, unlock()) }()
	queryCtx, cancelQuery = context.WithTimeout(ctx, waitSubcallTimeout)
	holders, err := s.Docker.SlotHolders(queryCtx)
	cancelQuery()
	if err != nil {
		return result, err
	}
	if len(holders) != 0 {
		result.state = state.LoopWaitingHost
		return result, nil
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	result.state = state.LoopRegistering
	if err := s.writeCycle(p, result, nil); err != nil {
		return result, err
	}
	tokenCtx, cancelToken := context.WithTimeout(ctx, waitSubcallTimeout)
	registrationToken, err := registrationTokenForProfile(tokenCtx, s.GitHub, p)
	cancelToken()
	if err != nil {
		return result, err
	}
	stamp := s.now().UTC().Format("20060102-150405")
	runnerPrefix := p.Runner.NamePrefix
	if runnerPrefix == "" {
		runnerPrefix = p.Name
	}
	result.runnerName = runnerPrefix + "-" + stamp
	result.containerName = p.Docker.ContainerNamePrefix + "-" + stamp
	result.state = state.LoopStarting
	if err := s.writeCycle(p, result, nil); err != nil {
		return result, err
	}
	spec := dockerpkg.RunSpec{
		Name: result.containerName, Image: p.Docker.Image, CPUs: p.Docker.CPUs, Memory: p.Docker.Memory,
		Volumes: p.Docker.Volumes, Env: runnerEnv(p, result.runnerName, registrationToken),
		Labels: map[string]string{"io.gha-runner-tui.managed": "true", "io.gha-runner-tui.profile": p.Name, "io.gha-runner-tui.runner": result.runnerName},
	}
	runCtx, cancelRun := context.WithTimeout(context.WithoutCancel(ctx), 120*time.Second)
	if ctx.Err() != nil {
		cancelRun()
		return result, ctx.Err()
	}
	s.restartCount++
	result.containerID, err = s.Docker.RunDetached(runCtx, spec)
	cancelRun()
	if ctx.Err() != nil {
		s.beginStop()
	}
	c := dockerpkg.ContainerInfo{ID: result.containerID, Name: result.containerName, Profile: p.Name, RunnerName: result.runnerName}
	if err != nil {
		// A failed CLI may still have created the container. Unknown facts stay
		// under this lock and Task 4 retries inspect; only NotFound releases it.
		details, inspectErr := s.inspectContainer(ctx, c.Name, ctx.Err() != nil)
		if errors.Is(inspectErr, dockerpkg.ErrContainerNotFound) {
			// Cancellation skips Run's cycle publication, and empty stop adoption
			// writes nothing. Persist this actual attempt before the early return.
			if writeErr := s.writeStartAttempt(p, result); writeErr != nil {
				return result, fmt.Errorf("%w: %w", errStateWrite, writeErr)
			}
			return result, err
		}
		if inspectErr == nil {
			result.containerID, c.ID = details.ID, details.ID
		}
	}
	result.state = state.LoopRunningJob
	if writeErr := s.writeCycle(p, result, nil); writeErr != nil {
		return result, writeErr
	}
	wr, waitErr := s.waitContainer(ctx, p, c, settings.idle)
	if waitErr != nil {
		return result, waitErr
	}
	result.exitCode = wr.exitCode
	if wr.retained {
		s.noteWaitIssue(p, c, wr.reason)
		return result, nil
	}
	if finishErr := s.finishContainer(ctx, p, c, wr); finishErr != nil {
		return result, finishErr
	}
	if wr.idleExpired && ctx.Err() == nil {
		for _, id := range candidates {
			s.skippedJobs[id] = s.now().Add(30 * time.Minute)
		}
		s.noteWaitIssue(p, c, "idle runner removed; check runner group/labels; candidate jobs skipped for 30 minutes")
	}
	result.state = state.LoopSleeping
	if wr.exitCode != nil && *wr.exitCode != 0 && !wr.idleExpired {
		return result, fmt.Errorf("runner container exited with code %d", *wr.exitCode)
	}
	if err != nil && wr.exitCode == nil && !wr.forceRemove && ctx.Err() == nil {
		return result, err
	}
	return result, nil
}

const hostLockPath = "/run/gha-runner-tui/host.lock"

func tryHostLock(path string) (unlock func() error, acquired bool, err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, false, err
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err = f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, false, err
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		_ = f.Close()
		return nil, false, nil
	}
	if err != nil {
		_ = f.Close()
		return nil, false, err
	}
	return func() error { return errors.Join(syscall.Flock(int(f.Fd()), syscall.LOCK_UN), f.Close()) }, true, nil
}

func (s *Supervisor) defaults() {
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.Sleep == nil {
		s.Sleep = sleepContext
	}
	if s.TryLock == nil {
		s.TryLock = func() (func() error, bool, error) { return tryHostLock(hostLockPath) }
	}
	if s.Jitter == nil {
		s.Jitter = func(d time.Duration) time.Duration { return time.Duration(float64(d) * (0.8 + rand.Float64()*0.4)) }
	}
	if s.skippedJobs == nil {
		s.skippedJobs = make(map[int64]time.Time)
	}
}

func (s *Supervisor) writeCycle(p config.Profile, result cycleResult, cycleErr error) error {
	fields, previous, err := readStateFile(p.Loop.StateFile)
	if err != nil {
		return fmt.Errorf("%w: %w", errStateWrite, err)
	}
	record := stateRecord{Profile: p.Name, Repo: targetSlug(p), State: string(result.state), Health: "running",
		LastTransitionAt: s.now().UTC().Format(time.RFC3339), LastRunnerName: result.runnerName,
		LastContainerID: result.containerID, LastContainerName: result.containerName, LastExitCode: result.exitCode, RestartCount: s.restartCount}
	if result.state == state.LoopSleeping {
		record.Health = "healthy"
	}
	if cycleErr != nil {
		reason := cycleErr.Error()
		record.LastError = &reason
		record.Health = "warning"
		if result.containerName == "" && previous != nil {
			record.LastRunnerName, record.LastContainerID, record.LastContainerName = previous.LastRunnerName, previous.LastContainerID, previous.LastContainerName
			record.LastExitCode = previous.LastExitCode
		}
	} else if result.state == state.LoopSleeping && result.containerName != "" && previous != nil {
		// Carry Task 4's safe diagnostic through the post-wait transition.
		if previous.LastContainerName == result.containerName {
			record.LastError = previous.LastError
		}
	}
	if err := s.writeState(p, record, fields, previous); err != nil {
		return fmt.Errorf("%w: %w", errStateWrite, err)
	}
	return nil
}

// writeStartAttempt publishes only attempt identity/count into a fresh snapshot.
// A failed command does not justify a new enum, transition time, or diagnostic.
func (s *Supervisor) writeStartAttempt(p config.Profile, result cycleResult) error {
	fields, previous, err := readStateFile(p.Loop.StateFile)
	if err != nil {
		return err
	}
	if previous == nil {
		return s.writeState(p, stateRecord{Profile: p.Name, Repo: targetSlug(p), State: string(result.state), Health: "running",
			LastTransitionAt: s.now().UTC().Format(time.RFC3339), LastRunnerName: result.runnerName,
			LastContainerID: result.containerID, LastContainerName: result.containerName, RestartCount: s.restartCount}, fields, previous)
	}
	before, err := stateFieldsJSON(fields)
	if err != nil {
		return err
	}
	setRaw(fields, "restart_count", s.restartCount)
	setRaw(fields, "last_runner_name", result.runnerName)
	setRaw(fields, "last_container_name", result.containerName)
	if result.containerID == "" {
		delete(fields, "last_container_id")
	} else {
		setRaw(fields, "last_container_id", result.containerID)
	}
	return writeStateFields(p.Loop.StateFile, fields, before, true)
}

// cleanupManagedTerminals removes only reverified own terminal containers,
// without taking the live-runner lock. Remaining containers are returned for
// startup/stop adoption; a terminal that restarted still needs that adoption.
func (s *Supervisor) cleanupManagedTerminals(ctx context.Context, p config.Profile) ([]dockerpkg.ContainerInfo, error) {
	if p.Name == "" {
		return nil, errors.New("managed cleanup requires a nonempty profile")
	}
	stopping := ctx.Err() != nil
	if stopping {
		s.beginStop()
	}
	base, timeout := s.subcallContext(ctx, stopping)
	queryCtx, cancelQuery := context.WithTimeout(base, timeout)
	containers, err := s.Docker.ListManaged(queryCtx, p.Name)
	cancelQuery()
	if err != nil {
		return nil, err
	}
	remaining := make([]dockerpkg.ContainerInfo, 0, len(containers))
	for _, c := range containers {
		if c.Profile != p.Name || c.State == state.ContainerRemoving {
			continue
		}
		if c.State != state.ContainerExited && c.State != state.ContainerDead {
			remaining = append(remaining, c)
			continue
		}
		if c.ID == "" {
			return nil, errors.New("managed cleanup candidate has no ID")
		}
		stopping = ctx.Err() != nil
		if stopping {
			s.beginStop()
			if !s.now().Before(s.stopDeadline) {
				s.noteWaitIssue(p, c, "stop budget exhausted; container retained")
				return remaining, nil
			}
		}
		details, err := s.inspectContainer(ctx, c.ID, stopping)
		if errors.Is(err, dockerpkg.ErrContainerNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if details.Labels["io.gha-runner-tui.managed"] != "true" || details.Labels["io.gha-runner-tui.profile"] != p.Name {
			continue
		}
		if details.ID == "" {
			return nil, errors.New("managed cleanup inspection has no ID")
		}
		if details.State != state.ContainerExited && details.State != state.ContainerDead {
			remaining = append(remaining, c)
			continue
		}
		stopping = ctx.Err() != nil
		if stopping {
			s.beginStop()
			if !s.now().Before(s.stopDeadline) {
				s.noteWaitIssue(p, c, "stop budget exhausted; container retained")
				return remaining, nil
			}
		}
		if err := s.removeContainer(ctx, details.ID, false, stopping); err != nil && !errors.Is(err, dockerpkg.ErrContainerNotFound) {
			return nil, err
		}
	}
	return remaining, nil
}

// adoptManaged runs once before job polling, and again on cancellation. It
// never creates a runner. Each container uses the same Supervisor stopDeadline.
func (s *Supervisor) adoptManaged(ctx context.Context, p config.Profile, settings loopSettings) error {
	stopping := ctx.Err() != nil
	if stopping {
		s.beginStop()
		if !s.now().Before(s.stopDeadline) {
			s.noteWaitIssue(p, dockerpkg.ContainerInfo{}, "stop budget exhausted; containers retained")
			return nil
		}
	}
	containers, err := s.cleanupManagedTerminals(ctx, p)
	if err != nil {
		return err
	}
	for _, c := range containers {
		if c.Profile != p.Name || c.State == state.ContainerRemoving {
			continue
		}
		for {
			stopping = ctx.Err() != nil
			if stopping {
				s.beginStop()
				if !s.now().Before(s.stopDeadline) {
					s.noteWaitIssue(p, c, "stop budget exhausted; container retained")
					return nil
				}
			}
			unlock, acquired, lockErr := s.TryLock()
			if lockErr != nil {
				return lockErr
			}
			if acquired {
				err = func() (err error) {
					defer func() { err = errors.Join(err, unlock()) }()
					_, inspectErr := s.inspectContainer(ctx, c.Name, stopping)
					if errors.Is(inspectErr, dockerpkg.ErrContainerNotFound) {
						return nil
					}
					result := cycleResult{runnerName: c.RunnerName, containerID: c.ID, containerName: c.Name, state: state.LoopRunningJob}
					if err := s.writeCycle(p, result, nil); err != nil {
						return err
					}
					wr, err := s.waitContainer(ctx, p, c, settings.idle)
					if err != nil {
						return err
					}
					if wr.retained {
						s.noteWaitIssue(p, c, wr.reason)
						return nil
					}
					if err := s.finishContainer(ctx, p, c, wr); err != nil {
						return err
					}
					result.exitCode, result.state = wr.exitCode, state.LoopSleeping
					if err := s.writeCycle(p, result, nil); err != nil {
						return err
					}
					if wr.exitCode != nil && *wr.exitCode != 0 && !wr.idleExpired {
						return fmt.Errorf("adopted runner container exited with code %d", *wr.exitCode)
					}
					return nil
				}()
				if err != nil {
					return err
				}
				break
			}
			if stopping {
				s.noteWaitIssue(p, c, "host lock busy during stop; container retained")
			} else {
				if err := s.writeCycle(p, cycleResult{state: state.LoopWaitingHost}, nil); err != nil {
					return err
				}
			}
			interval := settings.poll
			if stopping {
				interval = waitStopInterval
			} else {
				interval = s.Jitter(interval)
			}
			if err := s.sleepUntil(ctx, interval, stopping); err != nil {
				return err
			}
		}
	}
	return nil
}

func runnerEnv(profile config.Profile, runnerName, registrationToken string) map[string]string {
	env := make(map[string]string, len(profile.Docker.Env)+9)
	for key, value := range profile.Docker.Env {
		env[key] = value
	}
	env["RUNNER_NAME"] = runnerName
	env["RUNNER_TOKEN"] = registrationToken
	env["REG_TOKEN"] = registrationToken
	target, err := profile.ResolveTarget()
	if err == nil {
		env["RUNNER_REPO_URL"] = target.GitHubURL()
		env["REPO_URL"] = target.GitHubURL()
		if target.Scope == config.TargetScopeOrganization && profile.RunnerGroup.Name != "" {
			env["RUNNER_GROUP"] = profile.RunnerGroup.Name
		}
	} else {
		repoURL := fmt.Sprintf("https://github.com/%s/%s", profile.Repo.Owner, profile.Repo.Name)
		env["RUNNER_REPO_URL"] = repoURL
		env["REPO_URL"] = repoURL
	}
	env["RUNNER_LABELS"] = strings.Join(profile.Runner.Labels, ",")
	env["RUNNER_WORKDIR"] = profile.Runner.Workdir
	env["RUNNER_EPHEMERAL"] = fmt.Sprintf("%t", profile.Runner.Ephemeral)
	return env
}

// readStateFile takes a fresh snapshot for each publication. Only absent or
// corrupt state is repairable; other read failures must not become a blind write.
// A valid JSON object can still contain recoverable forward-compatible keys.
func readStateFile(path string) (map[string]json.RawMessage, *state.LoopState, error) {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return map[string]json.RawMessage{}, nil, nil
	}
	previous, err := state.ParseLoopState(data)
	if err != nil || previous.State == state.LoopUnknown || previous.LastTransitionAt.IsZero() {
		return fields, nil, nil
	}
	return fields, &previous, nil
}

func (s Supervisor) writeState(profile config.Profile, record stateRecord, fields map[string]json.RawMessage, previous *state.LoopState) error {
	if previous != nil && string(previous.State) == record.State {
		// Preserve the serialized timestamp, including its original offset.
		if err := json.Unmarshal(fields["last_transition_at"], &record.LastTransitionAt); err != nil {
			return err
		}
	}
	before, err := stateFieldsJSON(fields)
	if err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	// Unmarshal replaces present keys but does not clear omitted optionals.
	delete(fields, "last_runner_name")
	delete(fields, "last_container_id")
	delete(fields, "last_container_name")
	delete(fields, "last_exit_code")
	delete(fields, "last_error")
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	return writeStateFields(profile.Loop.StateFile, fields, before, previous != nil)
}

// Compare canonical JSON, not candidate timestamps or file formatting. A no-op
// never opens the temp file, preserving contents, inode, and mtime verbatim.
func writeStateFields(path string, fields map[string]json.RawMessage, before []byte, valid bool) error {
	data, err := stateFieldsJSON(fields)
	if err != nil {
		return err
	}
	if valid && bytes.Equal(before, data) {
		return nil
	}
	data, err = json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path+".tmp", append(data, '\n'), 0o640); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// Normalize JSON escapes/formatting for equality without rounding unknown
// numeric values. The original raw fields remain the source for persistence.
func stateFieldsJSON(fields map[string]json.RawMessage) ([]byte, error) {
	data, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	var values map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&values); err != nil {
		return nil, err
	}
	return json.Marshal(values)
}

func (s Supervisor) persistLogs(profile config.Profile, containerName, stamp, content string) error {
	filename := fmt.Sprintf("%s-%s.log", containerName, stamp)
	path := filepath.Join(profile.Loop.LogDir, filename)
	return os.WriteFile(path, []byte(content), 0o640)
}

func registrationTokenForProfile(ctx context.Context, client registrationTokenClient, profile config.Profile) (string, error) {
	target, err := profile.ResolveTarget()
	if err != nil {
		return "", err
	}
	if target.Scope == config.TargetScopeOrganization {
		return client.CreateOrgRegistrationToken(ctx, target.OrgSlug)
	}
	return client.CreateRegistrationToken(ctx, target.Owner, target.Repo)
}

func targetSlug(profile config.Profile) string {
	target, err := profile.ResolveTarget()
	if err != nil {
		return profile.Repo.Owner + "/" + profile.Repo.Name
	}
	if target.Scope == config.TargetScopeOrganization {
		return "org:" + target.OrgSlug
	}
	return target.Owner + "/" + target.Repo
}

func sleepContext(ctx context.Context, wait time.Duration) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// --- Task 4: independent container wait / finish ---

const (
	waitInspectInterval  = 15 * time.Second
	waitGitHubInterval   = 60 * time.Second
	waitStopInterval     = 5 * time.Second
	waitCreatedGrace     = 60 * time.Second
	waitStopBudget       = 60 * time.Second
	waitSubcallTimeout   = 10 * time.Second
	waitFinishLogTimeout = 30 * time.Second
)

// waitResult describes how a container left the host-wide slot. Only an
// authoritative GitHub DELETE 204, a configured exited removal, or two
// consecutive reliable "runner gone" observations may authorize removal.
type waitResult struct {
	exitCode    *int
	forceRemove bool
	idleExpired bool
	retained    bool
	reason      string
}

func (s Supervisor) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Supervisor) sleep(ctx context.Context, d time.Duration) error {
	if s.Sleep != nil {
		return s.Sleep(ctx, d)
	}
	return sleepContext(ctx, d)
}

func (s *Supervisor) beginStop() {
	if s.stopDeadline.IsZero() {
		s.stopDeadline = s.now().Add(waitStopBudget)
	}
}

// subcallContext bounds one normal inspect/API call to 10s. Once stopping, the
// caller's cancellation is no longer the authority: an independent context is
// derived with the smaller of 10s and the remaining stop budget.
func (s *Supervisor) subcallContext(ctx context.Context, stopping bool) (context.Context, time.Duration) {
	timeout := waitSubcallTimeout
	if !stopping {
		return ctx, timeout
	}
	base := context.WithoutCancel(ctx)
	if remaining := s.stopDeadline.Sub(s.now()); remaining < timeout {
		timeout = remaining
	}
	if timeout <= 0 {
		timeout = time.Nanosecond
	}
	return base, timeout
}

func (s *Supervisor) inspectContainer(ctx context.Context, name string, stopping bool) (dockerpkg.ContainerDetails, error) {
	base, timeout := s.subcallContext(ctx, stopping)
	inspectCtx, cancel := context.WithTimeout(base, timeout)
	defer cancel()
	return s.Docker.Inspect(inspectCtx, name)
}

func (s *Supervisor) findRunner(ctx context.Context, target config.ResolvedTarget, name string, stopping bool) (*gh.Runner, error) {
	base, timeout := s.subcallContext(ctx, stopping)
	queryCtx, cancel := context.WithTimeout(base, timeout)
	defer cancel()
	return s.GitHub.FindRunner(queryCtx, target, name)
}

func (s *Supervisor) deleteRunner(ctx context.Context, target config.ResolvedTarget, id int64, stopping bool) error {
	base, timeout := s.subcallContext(ctx, stopping)
	deleteCtx, cancel := context.WithTimeout(base, timeout)
	defer cancel()
	return deleteRunnerForTarget(deleteCtx, s.GitHub, target, id)
}

func (s *Supervisor) removeContainer(ctx context.Context, name string, force, stopping bool) error {
	base, timeout := s.subcallContext(ctx, stopping)
	removeCtx, cancel := context.WithTimeout(base, timeout)
	defer cancel()
	return s.Docker.Remove(removeCtx, name, force)
}

// deleteRunnerForTarget issues the exact-name runner DELETE for the resolved
// scope. The caller decides whether the outcome authorizes a container rm.
func deleteRunnerForTarget(ctx context.Context, c gh.Client, target config.ResolvedTarget, id int64) error {
	if target.Scope == config.TargetScopeOrganization {
		return c.DeleteOrgRunner(ctx, target.OrgSlug, id)
	}
	return c.DeleteRunner(ctx, target.Owner, target.Repo, id)
}

func idleExceeded(now, startedAt time.Time, idle time.Duration) bool {
	if startedAt.IsZero() {
		return false
	}
	age := now.Sub(startedAt)
	if age < 0 {
		return false
	}
	return age >= idle
}

// reliableContainerState reports a trustworthy, non-created Docker
// observation. Unknown, created, and error observations must never authorize a
// DELETE-based force removal: the run token/container may still be in flight.
func reliableContainerState(status state.ContainerStatus) bool {
	switch status {
	case state.ContainerRunning, state.ContainerRestarting, state.ContainerPaused:
		return true
	default:
		return false
	}
}

// diagnostic emits only caller-supplied safe lifecycle reasons, never backend
// errors, log bodies, or environment data. Output failures do not affect cleanup.
func (s *Supervisor) diagnostic(c dockerpkg.ContainerInfo, reason string) {
	w := s.Stderr
	if w == nil {
		w = os.Stderr
	}
	_, _ = fmt.Fprintf(w, "container=%q runner=%q: %s\n", c.Name, c.RunnerName, reason)
}

// noteWaitIssue emits a safe, body-free reason and merges it into the current
// state file instead of replacing it. Every existing field (ids, exit code,
// restart count, and unknown/forward-compatible keys) is preserved. Diagnostics
// do not change an existing enum transition time. Failures never abort the wait.
func (s *Supervisor) noteWaitIssue(p config.Profile, c dockerpkg.ContainerInfo, reason string) {
	s.diagnostic(c, reason)
	fields, _, err := readStateFile(p.Loop.StateFile)
	if err != nil {
		return
	}
	before, err := stateFieldsJSON(fields)
	if err != nil {
		return
	}
	setRawIfAbsent(fields, "profile", p.Name)
	setRawIfAbsent(fields, "repo", targetSlug(p))
	if _, ok := fields["last_runner_name"]; !ok && c.RunnerName != "" {
		setRaw(fields, "last_runner_name", c.RunnerName)
	}
	if _, ok := fields["last_container_name"]; !ok && c.Name != "" {
		setRaw(fields, "last_container_name", c.Name)
	}
	setRaw(fields, "last_error", reason)
	// A diagnostic may precede normal publication and have no state enum yet.
	// Keep any valid transition time even in that partial state record.
	var transition string
	decodeErr := json.Unmarshal(fields["last_transition_at"], &transition)
	if timestamp, err := time.Parse(time.RFC3339, transition); decodeErr != nil || err != nil || timestamp.IsZero() {
		setRaw(fields, "last_transition_at", s.now().UTC().Format(time.RFC3339))
	}
	_ = writeStateFields(p.Loop.StateFile, fields, before, true)
}

func setRaw(fields map[string]json.RawMessage, key string, value any) {
	if data, err := json.Marshal(value); err == nil {
		fields[key] = data
	}
}

func setRawIfAbsent(fields map[string]json.RawMessage, key string, value any) {
	if _, ok := fields[key]; !ok {
		setRaw(fields, key, value)
	}
}

// waitContainer watches one managed container until it leaves the slot or the
// caller's stop budget is exhausted. ctx is only a stop signal; it never
// authorizes killing or removing a container. seen is intentionally local: a
// taken-over container is never assumed to have registered before.
func (s *Supervisor) waitContainer(ctx context.Context, p config.Profile, c dockerpkg.ContainerInfo, idle time.Duration) (waitResult, error) {
	target, err := p.ResolveTarget()
	if err != nil {
		return waitResult{}, err
	}
	runnerName := c.RunnerName
	if runnerName == "" {
		runnerName = c.Name
	}

	seen := false
	missing := 0
	notifiedNeverSeen := false
	createdRetried := false
	stopping := false
	var lastGitHub time.Time

	if ctx.Err() != nil {
		stopping = true
		s.beginStop()
	}

	for {
		if ctx.Err() != nil && !stopping {
			stopping = true
			s.beginStop()
		}
		if stopping && !s.now().Before(s.stopDeadline) {
			return waitResult{retained: true, reason: "stop budget exhausted; container retained"}, nil
		}

		details, inspectErr := s.inspectContainer(ctx, c.Name, stopping)

		if errors.Is(inspectErr, dockerpkg.ErrContainerNotFound) {
			return waitResult{}, nil
		}

		if inspectErr == nil {
			switch details.State {
			case state.ContainerExited, state.ContainerDead:
				code := details.ExitCode
				return waitResult{exitCode: &code}, nil
			case state.ContainerCreated:
				if createdRetried {
					createdRetried = false
				} else if !details.CreatedAt.IsZero() && s.now().Sub(details.CreatedAt) >= waitCreatedGrace {
					if rmErr := s.removeContainer(ctx, c.Name, false, stopping); rmErr == nil {
						return waitResult{}, nil
					}
					createdRetried = true
					continue
				}
			case state.ContainerRemoving:
				createdRetried = false
			default:
				createdRetried = false
			}
		} else {
			createdRetried = false
		}

		// A DELETE-based removal is only ever authorized from a trustworthy,
		// non-created observation. Inspect errors, created containers, and
		// unknown states never reach GitHub, so a stray busy=false 204 cannot
		// force-remove a run that has not reliably started.
		checkGitHub := false
		switch {
		case inspectErr != nil:
			checkGitHub = false
		case !reliableContainerState(details.State):
			checkGitHub = false
		case stopping:
			checkGitHub = true
		case idleExceeded(s.now(), details.StartedAt, idle):
			if lastGitHub.IsZero() || s.now().Sub(lastGitHub) >= waitGitHubInterval {
				checkGitHub = true
			}
		}

		if checkGitHub {
			lastGitHub = s.now()
			runner, queryErr := s.findRunner(ctx, target, runnerName, stopping)
			switch {
			case queryErr != nil:
				missing = 0
				s.noteWaitIssue(p, c, "runner lookup failed; container retained")
			case runner != nil:
				seen, missing = true, 0
				if !runner.Busy {
					derr := s.deleteRunner(ctx, target, runner.ID, stopping)
					if derr == nil {
						return waitResult{forceRemove: true, idleExpired: !stopping}, nil
					}
					var api *gh.APIError
					if errors.As(derr, &api) && api.StatusCode == 422 {
						// The job was accepted: keep waiting and never rm.
					} else {
						s.noteWaitIssue(p, c, "runner deletion failed; container retained")
					}
				}
			case seen:
				missing++
				if missing == 2 {
					return waitResult{forceRemove: true}, nil
				}
			default:
				if inspectErr == nil && idleExceeded(s.now(), details.StartedAt, idle) && !notifiedNeverSeen {
					s.noteWaitIssue(p, c, "runner has not registered; container retained")
					notifiedNeverSeen = true
				}
			}
		}

		interval := waitInspectInterval
		if stopping {
			interval = waitStopInterval
		}
		if err := s.sleepUntil(ctx, interval, stopping); err != nil {
			if ctx.Err() != nil {
				if !stopping {
					stopping = true
					s.beginStop()
				}
				continue
			}
			return waitResult{}, err
		}
	}
}

// sleepUntil waits for one polling interval. While stopping, the caller's
// cancellation is ignored and the wait is clamped to the remaining budget so
// the whole drain stays within the 60s window.
func (s *Supervisor) sleepUntil(ctx context.Context, d time.Duration, stopping bool) error {
	if !stopping {
		return s.sleep(ctx, d)
	}
	if remaining := s.stopDeadline.Sub(s.now()); remaining < d {
		if remaining <= 0 {
			return nil
		}
		d = remaining
	}
	return s.sleep(context.WithoutCancel(ctx), d)
}

// finishContainer snapshots redacted logs (bounded, independent of the stop
// context) and only then removes the container when there is an authoritative
// basis. Log redaction is delegated to the docker client, so an Env gate
// failure yields no persisted logs and never raw output.
func (s *Supervisor) finishContainer(ctx context.Context, p config.Profile, c dockerpkg.ContainerInfo, result waitResult) error {
	logCtx, cancelLogs := context.WithTimeout(context.WithoutCancel(ctx), waitFinishLogTimeout)
	text, logErr := s.Docker.ReadLogs(logCtx, c.Name)
	if logErr != nil {
		s.diagnostic(c, "container log acquisition failed; removal policy unchanged")
	}
	if text != "" {
		stamp := s.now().UTC().Format("20060102-150405")
		if err := s.persistLogs(p, c.Name, stamp, text); err != nil {
			s.diagnostic(c, "container log persistence failed; removal policy unchanged")
		}
	}
	cancelLogs()

	force := result.forceRemove
	remove := result.forceRemove || (result.exitCode != nil && p.Docker.RemoveAfterExit)
	if !remove {
		return nil
	}

	removeCtx, cancelRemove := context.WithTimeout(context.WithoutCancel(ctx), waitSubcallTimeout)
	defer cancelRemove()
	if err := s.Docker.Remove(removeCtx, c.Name, force); err != nil {
		return fmt.Errorf("remove container %s: %w", c.Name, err)
	}
	return nil
}

type loopSettings struct {
	watch      []string
	poll, idle time.Duration
}

// validateLoopProfile resolves gate settings only for the loop process. Read-
// only profile/status consumers continue to accept legacy watch configuration.
func validateLoopProfile(p config.Profile) (loopSettings, error) {
	if !p.Runner.Ephemeral {
		return loopSettings{}, errors.New("loop requires runner.ephemeral=true")
	}

	poll, idle := p.Loop.PollIntervalSeconds, p.Loop.IdleTimeoutSeconds
	if poll == 0 {
		poll = 30
	}
	if idle == 0 {
		idle = 180
	}
	settings := loopSettings{
		poll: time.Duration(max(poll, 10)) * time.Second,
		idle: time.Duration(max(idle, 60)) * time.Second,
	}

	target, err := p.ResolveTarget()
	if err != nil {
		return loopSettings{}, err
	}

	if target.Scope == config.TargetScopeOrganization {
		if len(p.Runner.WatchRepositories) == 0 {
			return loopSettings{}, errors.New("runner.watch_repositories is required for organization loop profiles")
		}
		for _, raw := range p.Runner.WatchRepositories {
			owner, repo, ok := splitOwnerRepo(raw)
			if !ok {
				return loopSettings{}, fmt.Errorf("runner.watch_repositories entry %q must be owner/repo", raw)
			}
			settings.watch = append(settings.watch, owner+"/"+repo)
		}
		return settings, nil
	}

	own := target.Owner + "/" + target.Repo
	if len(p.Runner.WatchRepositories) == 0 {
		owner, repo, ok := splitOwnerRepo(own)
		if !ok {
			return loopSettings{}, fmt.Errorf("profile repository %q must be owner/repo", own)
		}
		settings.watch = []string{owner + "/" + repo}
		return settings, nil
	}
	if len(p.Runner.WatchRepositories) != 1 {
		return loopSettings{}, errors.New("repository loop allows exactly one watch repository")
	}
	owner, repo, ok := splitOwnerRepo(p.Runner.WatchRepositories[0])
	if !ok {
		return loopSettings{}, fmt.Errorf("runner.watch_repositories entry %q must be owner/repo", p.Runner.WatchRepositories[0])
	}
	if !strings.EqualFold(owner, target.Owner) || !strings.EqualFold(repo, target.Repo) {
		return loopSettings{}, errors.New("runner.watch_repositories must match the profile repository")
	}
	settings.watch = []string{owner + "/" + repo}
	return settings, nil
}

func splitOwnerRepo(value string) (owner, repo string, ok bool) {
	owner, repo, ok = strings.Cut(strings.TrimSpace(value), "/")
	ok = ok && owner != "" && repo != "" && !strings.Contains(repo, "/")
	return owner, repo, ok
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func IsMissingToken(err error) bool {
	return errors.Is(err, gh.ErrMissingToken)
}
