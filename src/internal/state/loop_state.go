package state

import (
	"encoding/json"
	"errors"
	"os"
	"time"
)

type LoopStatus string

const (
	LoopDisabled    LoopStatus = "disabled"
	LoopStopped     LoopStatus = "stopped"
	LoopActive      LoopStatus = "active"
	LoopSleeping    LoopStatus = "sleeping"
	LoopRegistering LoopStatus = "registering"
	LoopStarting    LoopStatus = "starting"
	LoopRunningJob  LoopStatus = "running-job"
	LoopCleaning    LoopStatus = "cleaning"
	LoopBackoff     LoopStatus = "backoff"
	LoopFailed      LoopStatus = "failed"
	LoopUnknown     LoopStatus = "unknown"
)

type LoopState struct {
	Profile           string     `json:"profile"`
	Repo              string     `json:"repo"`
	State             LoopStatus `json:"state"`
	Health            string     `json:"health"`
	LastTransitionAt  time.Time  `json:"last_transition_at"`
	LastRunnerName    string     `json:"last_runner_name"`
	LastContainerID   string     `json:"last_container_id"`
	LastContainerName string     `json:"last_container_name"`
	LastExitCode      *int       `json:"last_exit_code"`
	LastError         *string    `json:"last_error"`
	RestartCount      int        `json:"restart_count"`
}

func LoadLoopState(path string) (LoopState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return LoopState{}, err
	}
	return ParseLoopState(data)
}

func ParseLoopState(data []byte) (LoopState, error) {
	var raw struct {
		Profile           string  `json:"profile"`
		Repo              string  `json:"repo"`
		State             string  `json:"state"`
		Health            string  `json:"health"`
		LastTransitionAt  string  `json:"last_transition_at"`
		LastRunnerName    string  `json:"last_runner_name"`
		LastContainerID   string  `json:"last_container_id"`
		LastContainerName string  `json:"last_container_name"`
		LastExitCode      *int    `json:"last_exit_code"`
		LastError         *string `json:"last_error"`
		RestartCount      int     `json:"restart_count"`
	}

	if err := json.Unmarshal(data, &raw); err != nil {
		return LoopState{}, err
	}

	state := NormalizeLoopStatus(raw.State)
	if state == LoopUnknown && raw.State != "" {
		return LoopState{}, errors.New("unknown loop state")
	}

	result := LoopState{
		Profile:           raw.Profile,
		Repo:              raw.Repo,
		State:             state,
		Health:            raw.Health,
		LastRunnerName:    raw.LastRunnerName,
		LastContainerID:   raw.LastContainerID,
		LastContainerName: raw.LastContainerName,
		LastExitCode:      raw.LastExitCode,
		LastError:         raw.LastError,
		RestartCount:      raw.RestartCount,
	}

	if raw.LastTransitionAt != "" {
		ts, err := time.Parse(time.RFC3339, raw.LastTransitionAt)
		if err != nil {
			return LoopState{}, err
		}
		result.LastTransitionAt = ts
	}

	return result, nil
}

func NormalizeLoopStatus(value string) LoopStatus {
	switch LoopStatus(value) {
	case LoopDisabled, LoopStopped, LoopActive, LoopSleeping, LoopRegistering,
		LoopStarting, LoopRunningJob, LoopCleaning, LoopBackoff, LoopFailed:
		return LoopStatus(value)
	default:
		return LoopUnknown
	}
}
