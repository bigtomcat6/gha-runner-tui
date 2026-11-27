package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"gha-runner-tui/internal/config"
)

// Fleet report vocabulary from the node-center contract.
const (
	FleetPhasePlan    = "plan"
	FleetPhaseExecute = "execute"

	FleetActionCreate    = "create"
	FleetActionUpdate    = "update"
	FleetActionUnchanged = "unchanged"
	FleetActionRestart   = "restart"
	FleetActionDelete    = "delete"
	FleetActionAdopt     = "adopt"
	FleetActionConflict  = "conflict"
	FleetActionError     = "error"

	FleetOutcomePlanned = "planned"
	FleetOutcomeDone    = "done"
	FleetOutcomeFailed  = "failed"
	FleetOutcomeNotRun  = "not_run"

	FleetRestartNone     = "none"
	FleetRestartQueued   = "queued"
	FleetRestartFailed   = "failed"
	FleetRestartSkipped  = "skipped_inactive"
	DefaultApplyLockPath = "/run/gha-runner-tui/apply.lock"
)

var (
	// ErrFleetInput marks malformed arguments or bundle content (exit 2).
	ErrFleetInput = errors.New("invalid fleet input")
	// ErrFleetRejected marks a preflight rejection before any write (exit 1).
	ErrFleetRejected = errors.New("fleet operation rejected")
	// ErrFleetPartial marks an execution that wrote something and did not
	// finish (exit 3).
	ErrFleetPartial = errors.New("fleet operation partially executed")
	// ErrFleetLocked means another process holds the apply lock.
	ErrFleetLocked = errors.New("apply lock is held by another process")
)

type FleetResult struct {
	Name          string `json:"name"`
	Action        string `json:"action"`
	RestartNeeded bool   `json:"restart_needed"`
	Outcome       string `json:"outcome"`
	Restart       string `json:"restart"`
	Message       string `json:"message"`
}

type FleetReport struct {
	Phase    string        `json:"phase"`
	PlanID   string        `json:"plan_id,omitempty"`
	Complete bool          `json:"complete"`
	Results  []FleetResult `json:"results"`
}

type ExportEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Owner string `json:"owner"`
	YAML  string `json:"yaml"`
}

type ExportError struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

type ExportReport struct {
	Complete bool          `json:"complete"`
	Profiles []ExportEntry `json:"profiles"`
	Errors   []ExportError `json:"errors"`
}

type ExportInput struct {
	Names []string
	Owner string
}

type ApplyInput struct {
	Bundle     []byte
	Owner      string
	Prune      bool
	DryRun     bool
	ExpectPlan string
	NoStart    bool
}

type AdoptInput struct {
	Names      []string
	Match      string
	Owner      string
	DryRun     bool
	ExpectPlan string
}

type DeleteInput struct {
	Name  string
	Owner string
	Force bool
}

// fleetPlanEntry is the canonical, sorted unit of a plan. It deliberately
// carries no active-state or display text so plan_id is reproducible.
type fleetPlanEntry struct {
	Name          string `json:"name"`
	Path          string `json:"path"`
	Service       string `json:"service"`
	Action        string `json:"action"`
	RestartNeeded bool   `json:"restart_needed"`
	DiskSHA       string `json:"disk_sha"`
	InputSHA      string `json:"input_sha"`
	UnitEvidence  string `json:"unit_evidence"`
	RenderedUnit  string `json:"rendered_unit,omitempty"`
	Intent        string `json:"intent,omitempty"`
	Evidence      string `json:"evidence,omitempty"`
	Marker        string `json:"marker,omitempty"`
	LoopBinary    string `json:"loop_binary,omitempty"`
	Message       string `json:"-"`
}

type fleetPlanIdentity struct {
	Operation string           `json:"operation"`
	Owner     string           `json:"owner"`
	Prune     bool             `json:"prune"`
	NoStart   bool             `json:"no_start"`
	Targets   []fleetPlanEntry `json:"targets"`
}

func fleetPlanID(ids fleetPlanIdentity) (string, error) {
	data, err := json.Marshal(ids)
	if err != nil {
		return "", err
	}
	return "sha256:" + shaOfBytes(data), nil
}

func shaOfBytes(data []byte) string {
	return hex.EncodeToString(hashBytes(data))
}

func hashBytes(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

func fleetOwner(value string) (string, error) {
	switch value {
	case "", config.ManagedByLocal:
		return config.ManagedByLocal, nil
	case config.ManagedByFleet:
		return config.ManagedByFleet, nil
	default:
		return "", fmt.Errorf("%w: owner must be local or fleet", ErrFleetInput)
	}
}

func fleetLockPath(m RunnerManager) string {
	if m.LockPath != "" {
		return m.LockPath
	}
	// Production runs as root through sudo and uses the documented host path.
	// An unprivileged process (local tests or a dry-run) cannot write managed
	// configuration anyway, so it uses a per-user lock instead of failing on
	// the root-only /run directory.
	if os.Geteuid() == 0 {
		return DefaultApplyLockPath
	}
	// A non-root process cannot manage the root-owned production files. Keep
	// an isolated per-config lock so parallel local tests and dry-runs do not
	// queue behind each other or fail on the root-only /run directory.
	return filepath.Join(os.TempDir(), "gha-runner-tui-"+shaOfBytes([]byte(m.ConfigPath))[:16]+".apply.lock")
}

func (m RunnerManager) withFleetLock(fn func() error) error {
	lockPath := fleetLockPath(m)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return fmt.Errorf("%w: cannot create lock directory: %v", ErrFleetRejected, err)
	}
	if info, err := os.Lstat(lockPath); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: lock path is not a regular file: %s", ErrFleetRejected, lockPath)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: cannot inspect lock: %v", ErrFleetRejected, err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("%w: cannot open lock: %v", ErrFleetRejected, err)
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: lock is not a regular file", ErrFleetRejected)
	}
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("%w: cannot secure lock permissions: %v", ErrFleetRejected, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return ErrFleetLocked
		}
		return fmt.Errorf("%w: cannot lock: %v", ErrFleetRejected, err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}

// --- durable intents -------------------------------------------------------

type fleetIntent struct {
	Version         int    `json:"version"`
	Operation       string `json:"operation"`
	Digest          string `json:"digest"`
	CreatedAt       string `json:"created_at"`
	Name            string `json:"name"`
	Owner           string `json:"owner"`
	ProfilePath     string `json:"profile_path"`
	UnitPath        string `json:"unit_path"`
	Service         string `json:"service"`
	NoStart         bool   `json:"no_start"`
	RestartNeeded   bool   `json:"restart_needed,omitempty"`
	ProfileSHA      string `json:"profile_sha,omitempty"`
	UnitSHA         string `json:"unit_sha,omitempty"`
	UnitEvidence    string `json:"unit_evidence,omitempty"`
	OriginalProfile string `json:"original_profile,omitempty"`
	OriginalUnit    string `json:"original_unit,omitempty"`
	OriginalDropIns string `json:"original_dropins,omitempty"`
	HadUnit         bool   `json:"had_unit,omitempty"`
	HadDropIns      bool   `json:"had_dropins,omitempty"`
	HadMarker       bool   `json:"had_marker,omitempty"`
	ArchiveDir      string `json:"archive_dir,omitempty"`
	FragmentPath    string `json:"fragment_path,omitempty"`
	EffectiveConfig string `json:"effective_config,omitempty"`
}

type intentKey struct {
	Operation       string `json:"operation"`
	Name            string `json:"name"`
	Owner           string `json:"owner"`
	ProfilePath     string `json:"profile_path"`
	UnitPath        string `json:"unit_path"`
	Service         string `json:"service"`
	NoStart         bool   `json:"no_start"`
	RestartNeeded   bool   `json:"restart_needed"`
	ProfileSHA      string `json:"profile_sha"`
	UnitSHA         string `json:"unit_sha"`
	UnitEvidence    string `json:"unit_evidence"`
	OriginalProfile string `json:"original_profile"`
	OriginalUnit    string `json:"original_unit"`
	OriginalDropIns string `json:"original_dropins"`
	HadUnit         bool   `json:"had_unit"`
	HadDropIns      bool   `json:"had_dropins"`
	HadMarker       bool   `json:"had_marker"`
	ArchiveDir      string `json:"archive_dir"`
}

func (in fleetIntent) key() intentKey {
	return intentKey{
		Operation: in.Operation, Name: in.Name, Owner: in.Owner,
		ProfilePath: in.ProfilePath, UnitPath: in.UnitPath, Service: in.Service,
		NoStart: in.NoStart, RestartNeeded: in.RestartNeeded, ProfileSHA: in.ProfileSHA, UnitSHA: in.UnitSHA,
		UnitEvidence: in.UnitEvidence, OriginalProfile: in.OriginalProfile,
		OriginalUnit: in.OriginalUnit, OriginalDropIns: in.OriginalDropIns,
		HadUnit: in.HadUnit, HadDropIns: in.HadDropIns, HadMarker: in.HadMarker,
		ArchiveDir: in.ArchiveDir,
	}
}

func (in fleetIntent) computeDigest() (string, error) {
	data, err := json.Marshal(in.key())
	if err != nil {
		return "", err
	}
	return shaOfBytes(data), nil
}

func fleetIntentDir(cfg config.GlobalConfig) string {
	return filepath.Join(filepath.Dir(cfg.Paths.StateDir), "fleet-intents")
}

func intentPath(cfg config.GlobalConfig, name string) string {
	return filepath.Join(fleetIntentDir(cfg), name+".json")
}

func loadIntent(cfg config.GlobalConfig, name string) (fleetIntent, error) {
	data, err := os.ReadFile(intentPath(cfg, name))
	if errors.Is(err, os.ErrNotExist) {
		return fleetIntent{}, os.ErrNotExist
	}
	if err != nil {
		return fleetIntent{}, err
	}
	var in fleetIntent
	if err := json.Unmarshal(data, &in); err != nil {
		return fleetIntent{}, fmt.Errorf("invalid fleet intent for %s: %w", name, err)
	}
	if in.Version != 1 || in.Name != name || in.Operation == "" || in.Digest == "" {
		return fleetIntent{}, fmt.Errorf("invalid fleet intent for %s", name)
	}
	computed, err := in.computeDigest()
	if err != nil {
		return fleetIntent{}, err
	}
	if computed != in.Digest {
		return fleetIntent{}, fmt.Errorf("fleet intent for %s was modified or is incomplete", name)
	}
	return in, nil
}

func loadIntents(cfg config.GlobalConfig) (map[string]fleetIntent, []error) {
	intents := map[string]fleetIntent{}
	dir := fleetIntentDir(cfg)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return intents, nil
	}
	if err != nil {
		return intents, []error{err}
	}
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".json")
		in, err := loadIntent(cfg, name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		intents[name] = in
	}
	return intents, errs
}

func writeIntent(cfg config.GlobalConfig, in fleetIntent) error {
	in.Version = 1
	if in.CreatedAt == "" {
		in.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	digest, err := in.computeDigest()
	if err != nil {
		return err
	}
	in.Digest = digest
	data, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := fleetIntentDir(cfg)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// One atomic, fsynced replacement: the intent exists either complete and
	// verifiable or not at all.
	return writeFileAtomic(dir, in.Name+".json", data, 0o600)
}

func removeIntent(cfg config.GlobalConfig, name string) error {
	if err := os.Remove(intentPath(cfg, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDir(fleetIntentDir(cfg))
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()
	return f.Sync()
}

// --- restart-pending markers ----------------------------------------------

func restartPendingDir(cfg config.GlobalConfig) string {
	return filepath.Join(filepath.Dir(cfg.Paths.StateDir), "restart-pending")
}

func restartPendingPath(cfg config.GlobalConfig, name string) string {
	return filepath.Join(restartPendingDir(cfg), name)
}

func RestartPending(cfg config.GlobalConfig, name string) bool {
	_, err := os.Lstat(restartPendingPath(cfg, name))
	return err == nil
}

func writeRestartMarker(cfg config.GlobalConfig, name string) error {
	path := restartPendingPath(cfg, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".pending-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o640); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write([]byte(name + "\n")); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func removeRestartMarker(cfg config.GlobalConfig, name string) error {
	path := restartPendingPath(cfg, name)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func markerEvidence(cfg config.GlobalConfig, name string) string {
	path := restartPendingPath(cfg, name)
	info, err := os.Lstat(path)
	if err != nil {
		return ""
	}
	return shaOfBytes([]byte(fmt.Sprintf("marker:%s:%d:%d", name, info.Size(), info.ModTime().UnixNano())))
}

// --- profile inventory -----------------------------------------------------

type diskProfile struct {
	Name    string
	Path    string
	Raw     []byte
	SHA     string
	Profile config.Profile
}

type profileInventory struct {
	ByName map[string]diskProfile
	Bad    []ExportError
}

func readInventory(dir string) (profileInventory, error) {
	inventory := profileInventory{ByName: map[string]diskProfile{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return inventory, nil
		}
		return inventory, err
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || (!strings.HasSuffix(entry.Name(), ".yaml") && !strings.HasSuffix(entry.Name(), ".yml")) {
			continue
		}
		paths = append(paths, filepath.Join(dir, entry.Name()))
	}
	sort.Strings(paths)
	for _, path := range paths {
		info, err := os.Lstat(path)
		if err != nil {
			inventory.Bad = append(inventory.Bad, ExportError{Path: path, Error: err.Error()})
			continue
		}
		if !info.Mode().IsRegular() {
			inventory.Bad = append(inventory.Bad, ExportError{Path: path, Error: "profile file is not a regular file"})
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			inventory.Bad = append(inventory.Bad, ExportError{Path: path, Error: err.Error()})
			continue
		}
		profile, err := config.LoadProfile(path)
		if err != nil {
			inventory.Bad = append(inventory.Bad, ExportError{Path: path, Error: err.Error()})
			continue
		}
		if _, exists := inventory.ByName[profile.Name]; exists {
			inventory.Bad = append(inventory.Bad, ExportError{Path: path, Error: "duplicate profile name " + profile.Name})
			continue
		}
		inventory.ByName[profile.Name] = diskProfile{Name: profile.Name, Path: path, Raw: raw, SHA: shaOfBytes(raw), Profile: profile}
	}
	return inventory, nil
}

// --- unit evidence ---------------------------------------------------------

type unitEvidence struct {
	Service     string
	LoadState   string
	ActiveState string
	Fragment    string
	FragmentSHA string
	ConfigPath  string
	ConfigErr   string
	UnitFile    string
	DropInDir   string
	DropIns     []string
	Valid       bool
	Message     string
	dropInPaths []string
}

func (e unitEvidence) digest() string {
	data, _ := json.Marshal(struct {
		Service     string   `json:"service"`
		LoadState   string   `json:"load_state"`
		Fragment    string   `json:"fragment"`
		FragmentSHA string   `json:"fragment_sha"`
		ConfigPath  string   `json:"config_path"`
		ConfigErr   string   `json:"config_err"`
		UnitFile    string   `json:"unit_file"`
		DropInDir   string   `json:"dropin_dir"`
		DropIns     []string `json:"dropins"`
	}{e.Service, e.LoadState, e.Fragment, e.FragmentSHA, e.ConfigPath, e.ConfigErr, e.UnitFile, e.DropInDir, e.DropIns})
	return shaOfBytes(data)
}

func collectUnitEvidence(ctx context.Context, m RunnerManager, cfg config.GlobalConfig, service, profilePath string) (unitEvidence, error) {
	unitDir := m.SystemdUnitDir
	if unitDir == "" {
		unitDir = "/etc/systemd/system"
	}
	unitPath, err := confineJoin(unitDir, service)
	if err != nil {
		return unitEvidence{}, err
	}
	if info, err := os.Lstat(unitDir); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return unitEvidence{}, fmt.Errorf("systemd unit directory %q is not a directory", unitDir)
		}
	}
	evidence := unitEvidence{Service: service}
	info, showErr := m.Systemd.Show(ctx, service)
	if showErr != nil {
		return evidence, showErr
	}
	evidence.LoadState = info.LoadState
	evidence.ActiveState = info.ActiveState
	evidence.Fragment = info.FragmentPath
	if info.FragmentPath != "" {
		if raw, err := os.ReadFile(info.FragmentPath); err == nil {
			evidence.FragmentSHA = shaOfBytes(raw)
		}
	}
	evidence.dropInPaths = info.DropInPaths
	if cfgPath, err := info.ConfigPath(); err == nil {
		evidence.ConfigPath = cfgPath
	} else {
		evidence.ConfigErr = err.Error()
	}
	if fragment, err := os.Lstat(unitPath); err == nil {
		switch {
		case fragment.Mode()&os.ModeSymlink != 0:
			evidence.UnitFile = "symlink"
		case fragment.Mode().IsRegular():
			raw, err := os.ReadFile(unitPath)
			if err != nil {
				return evidence, err
			}
			evidence.UnitFile = shaOfBytes(raw)
			if evidence.Fragment != "" && evidence.Fragment != unitPath {
				if rawFragment, err := os.ReadFile(evidence.Fragment); err == nil && shaOfBytes(rawFragment) != evidence.UnitFile {
					return evidence, fmt.Errorf("FragmentPath %q does not contain the configured unit file", evidence.Fragment)
				}
			}
		default:
			evidence.UnitFile = "not-regular"
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return evidence, err
	}
	if evidence.UnitFile == "" {
		evidence.UnitFile = "absent"
	}
	dropInDir := unitPath + ".d"
	entries, err := os.ReadDir(dropInDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return evidence, err
	}
	if errors.Is(err, os.ErrNotExist) {
		evidence.DropInDir = "absent"
	} else {
		var lines []string
		for _, entry := range entries {
			path := filepath.Join(dropInDir, entry.Name())
			entryInfo, err := entry.Info()
			if err != nil {
				return evidence, err
			}
			kind := "file"
			sha := ""
			if entryInfo.Mode()&os.ModeSymlink != 0 {
				kind = "symlink"
			} else if !entryInfo.Mode().IsRegular() {
				kind = "other"
			} else {
				raw, err := os.ReadFile(path)
				if err != nil {
					return evidence, err
				}
				sha = shaOfBytes(raw)
			}
			lines = append(lines, entry.Name()+"\x00"+kind+"\x00"+sha)
		}
		sort.Strings(lines)
		evidence.DropIns = lines
		evidence.DropInDir = shaOfBytes([]byte(strings.Join(lines, "\n")))
	}
	evidence.Valid = true
	evidence.Message = evidence.validate()
	return evidence, nil
}

func (e unitEvidence) validate() string {
	switch e.LoadState {
	case "loaded":
		if e.Fragment == "" {
			return "unit has no FragmentPath"
		}
		if !filepath.IsAbs(e.Fragment) {
			return "unit FragmentPath is not absolute"
		}
		if e.UnitFile == "absent" {
			return "unit is loaded but its file is absent"
		}
	case "not-found":
		if e.UnitFile != "absent" || e.DropInDir != "absent" {
			return "unit LoadState is not-found but files exist"
		}
		return "unit not installed"
	default:
		return fmt.Sprintf("unit LoadState %q is not supported", e.LoadState)
	}
	for _, entry := range e.DropIns {
		parts := strings.Split(entry, "\x00")
		if len(parts) != 3 || parts[1] != "file" {
			return "unit drop-in is not a regular file"
		}
	}
	return ""
}
