package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type AdmissionMode int

const (
	AdmissionPaused AdmissionMode = iota
	AdmissionNormal
	AdmissionDelayed
)

// AdmissionPolicy is a local override, independent of the profile YAML and
// loop's diagnostic state. Missing overrides preserve normal admission.
type AdmissionPolicy struct {
	Mode         AdmissionMode `json:"mode"`
	DelaySeconds int           `json:"delay_seconds"`
}

func DefaultAdmissionPolicy() AdmissionPolicy {
	return AdmissionPolicy{Mode: AdmissionNormal, DelaySeconds: 60}
}

func (p AdmissionPolicy) Validate() error {
	if p.Mode < AdmissionPaused || p.Mode > AdmissionDelayed {
		return errors.New("mode must be 0 (paused), 1 (normal), or 2 (delayed)")
	}
	if p.DelaySeconds < 1 || p.DelaySeconds > 86400 {
		return errors.New("delay-seconds must be between 1 and 86400")
	}
	return nil
}

func LoadAdmissionPolicy(stateFile string) (AdmissionPolicy, error) {
	policy := DefaultAdmissionPolicy()
	if stateFile == "" {
		return policy, nil
	}
	data, err := os.ReadFile(stateFile + ".admission.json")
	if errors.Is(err, os.ErrNotExist) {
		return policy, nil
	}
	if err != nil {
		return AdmissionPolicy{}, fmt.Errorf("read admission policy: %w", err)
	}
	var raw struct {
		Mode         *AdmissionMode `json:"mode"`
		DelaySeconds *int           `json:"delay_seconds"`
	}
	if err := json.Unmarshal(data, &raw); err != nil || raw.Mode == nil {
		return AdmissionPolicy{}, errors.New("invalid admission policy: a numeric mode is required")
	}
	policy.Mode = *raw.Mode
	if raw.DelaySeconds != nil {
		policy.DelaySeconds = *raw.DelaySeconds
	}
	if err := policy.Validate(); err != nil {
		return AdmissionPolicy{}, fmt.Errorf("invalid admission policy: %w", err)
	}
	return policy, nil
}

// SaveAdmissionPolicy atomically replaces only the override. In particular,
// it never touches the loop state written concurrently by a running job.
func SaveAdmissionPolicy(stateFile string, policy AdmissionPolicy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	if stateFile == "" {
		return errors.New("profile loop.state_file is required for admission control")
	}
	path := stateFile + ".admission.json"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".admission-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o644); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
