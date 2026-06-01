package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gha-runner-tui/internal/config"
	dockerpkg "gha-runner-tui/internal/docker"
	gh "gha-runner-tui/internal/github"
)

type Supervisor struct {
	ProfilePath string
	Docker      dockerpkg.Client
	GitHub      gh.Client
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

func (s Supervisor) Run(ctx context.Context) error {
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

	restartCount := 0
	backoff := max(profile.Loop.BackoffSeconds, 1)
	maxBackoff := max(profile.Loop.MaxBackoffSeconds, backoff)
	interval := max(profile.Loop.IntervalSeconds, 1)

	for {
		restartCount++
		result, err := s.runCycle(ctx, profile, restartCount)
		if err == nil {
			if writeErr := s.writeState(profile, stateRecord{
				Profile:           profile.Name,
				Repo:              repoSlug(profile),
				State:             "sleeping",
				Health:            "healthy",
				LastTransitionAt:  time.Now().UTC().Format(time.RFC3339),
				LastRunnerName:    result.runnerName,
				LastContainerID:   result.containerID,
				LastContainerName: result.containerName,
				LastExitCode:      &result.exitCode,
				RestartCount:      restartCount,
			}); writeErr != nil {
				return writeErr
			}

			backoff = max(profile.Loop.BackoffSeconds, 1)
			if err := sleepContext(ctx, time.Duration(interval)*time.Second); err != nil {
				return err
			}
			continue
		}

		lastError := err.Error()
		if writeErr := s.writeState(profile, stateRecord{
			Profile:           profile.Name,
			Repo:              repoSlug(profile),
			State:             "backoff",
			Health:            "warning",
			LastTransitionAt:  time.Now().UTC().Format(time.RFC3339),
			LastRunnerName:    result.runnerName,
			LastContainerID:   result.containerID,
			LastContainerName: result.containerName,
			LastExitCode:      optionalInt(result.exitCode, result.exitCode >= 0),
			LastError:         &lastError,
			RestartCount:      restartCount,
		}); writeErr != nil {
			return writeErr
		}

		if err := sleepContext(ctx, time.Duration(backoff)*time.Second); err != nil {
			return err
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

type cycleResult struct {
	runnerName    string
	containerID   string
	containerName string
	exitCode      int
}

func (s Supervisor) runCycle(ctx context.Context, profile config.Profile, restartCount int) (cycleResult, error) {
	result := cycleResult{exitCode: -1}
	if err := s.writeState(profile, stateRecord{
		Profile:          profile.Name,
		Repo:             repoSlug(profile),
		State:            "cleaning",
		Health:           "running",
		LastTransitionAt: time.Now().UTC().Format(time.RFC3339),
		RestartCount:     restartCount,
	}); err != nil {
		return result, err
	}

	if _, err := s.Docker.CleanupExited(ctx, profile.Docker.ContainerNamePrefix); err != nil {
		return result, err
	}

	if err := s.writeState(profile, stateRecord{
		Profile:          profile.Name,
		Repo:             repoSlug(profile),
		State:            "registering",
		Health:           "running",
		LastTransitionAt: time.Now().UTC().Format(time.RFC3339),
		RestartCount:     restartCount,
	}); err != nil {
		return result, err
	}

	registrationToken, err := s.GitHub.CreateRegistrationToken(ctx, profile.Repo.Owner, profile.Repo.Name)
	if err != nil {
		return result, err
	}

	stamp := time.Now().UTC().Format("20060102-150405")
	runnerPrefix := profile.Runner.NamePrefix
	if runnerPrefix == "" {
		runnerPrefix = profile.Name
	}
	result.runnerName = runnerPrefix + "-" + stamp
	result.containerName = profile.Docker.ContainerNamePrefix + "-" + stamp

	if err := s.writeState(profile, stateRecord{
		Profile:           profile.Name,
		Repo:              repoSlug(profile),
		State:             "starting",
		Health:            "running",
		LastTransitionAt:  time.Now().UTC().Format(time.RFC3339),
		LastRunnerName:    result.runnerName,
		LastContainerName: result.containerName,
		RestartCount:      restartCount,
	}); err != nil {
		return result, err
	}

	containerID, err := s.Docker.RunDetached(ctx, dockerpkg.RunSpec{
		Name:    result.containerName,
		Image:   profile.Docker.Image,
		CPUs:    profile.Docker.CPUs,
		Memory:  profile.Docker.Memory,
		Volumes: profile.Docker.Volumes,
		Env:     runnerEnv(profile, result.runnerName, registrationToken),
	})
	if err != nil {
		return result, err
	}
	result.containerID = containerID

	if err := s.writeState(profile, stateRecord{
		Profile:           profile.Name,
		Repo:              repoSlug(profile),
		State:             "running-job",
		Health:            "running",
		LastTransitionAt:  time.Now().UTC().Format(time.RFC3339),
		LastRunnerName:    result.runnerName,
		LastContainerID:   result.containerID,
		LastContainerName: result.containerName,
		RestartCount:      restartCount,
	}); err != nil {
		return result, err
	}

	exitCode, err := s.Docker.Wait(ctx, result.containerName)
	result.exitCode = exitCode
	if err != nil {
		return result, err
	}

	if err := s.writeState(profile, stateRecord{
		Profile:           profile.Name,
		Repo:              repoSlug(profile),
		State:             "cleaning",
		Health:            "running",
		LastTransitionAt:  time.Now().UTC().Format(time.RFC3339),
		LastRunnerName:    result.runnerName,
		LastContainerID:   result.containerID,
		LastContainerName: result.containerName,
		LastExitCode:      &result.exitCode,
		RestartCount:      restartCount,
	}); err != nil {
		return result, err
	}

	if logText, logErr := s.Docker.ReadLogs(ctx, result.containerName); logErr == nil {
		_ = s.persistLogs(profile, result.containerName, stamp, logText)
	}
	if profile.Docker.RemoveAfterExit {
		_ = s.Docker.Remove(ctx, result.containerName, true)
	}

	if exitCode != 0 {
		return result, fmt.Errorf("runner container exited with code %d", exitCode)
	}
	return result, nil
}

func runnerEnv(profile config.Profile, runnerName, registrationToken string) map[string]string {
	env := make(map[string]string, len(profile.Docker.Env)+6)
	for key, value := range profile.Docker.Env {
		env[key] = value
	}
	env["RUNNER_NAME"] = runnerName
	env["RUNNER_TOKEN"] = registrationToken
	env["RUNNER_REPO_URL"] = fmt.Sprintf("https://github.com/%s/%s", profile.Repo.Owner, profile.Repo.Name)
	env["RUNNER_LABELS"] = strings.Join(profile.Runner.Labels, ",")
	env["RUNNER_WORKDIR"] = profile.Runner.Workdir
	env["RUNNER_EPHEMERAL"] = fmt.Sprintf("%t", profile.Runner.Ephemeral)
	return env
}

func (s Supervisor) writeState(profile config.Profile, record stateRecord) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	tempPath := profile.Loop.StateFile + ".tmp"
	if err := os.WriteFile(tempPath, append(data, '\n'), 0o640); err != nil {
		return err
	}
	return os.Rename(tempPath, profile.Loop.StateFile)
}

func (s Supervisor) persistLogs(profile config.Profile, containerName, stamp, content string) error {
	filename := fmt.Sprintf("%s-%s.log", containerName, stamp)
	path := filepath.Join(profile.Loop.LogDir, filename)
	return os.WriteFile(path, []byte(content), 0o640)
}

func repoSlug(profile config.Profile) string {
	return profile.Repo.Owner + "/" + profile.Repo.Name
}

func optionalInt(value int, ok bool) *int {
	if !ok {
		return nil
	}
	return &value
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
