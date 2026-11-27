package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"gha-runner-tui/internal/config"
)

type fleetTarget struct {
	entry        fleetPlanEntry
	doc          bundleDocument
	disk         diskProfile
	diskPresent  bool
	evidence     unitEvidence
	noStart      bool
	force        bool
	hasIntent    bool
	intent       fleetIntent
	unitPath     string
	renderedUnit []byte
}

func (t fleetTarget) result(action string) FleetResult {
	return FleetResult{Name: t.entry.Name, Action: action, RestartNeeded: t.entry.RestartNeeded, Outcome: FleetOutcomePlanned, Restart: FleetRestartNone, Message: t.entry.Message}
}

func (m RunnerManager) Apply(ctx context.Context, input ApplyInput) (FleetReport, error) {
	cfg, err := config.LoadGlobalConfig(m.ConfigPath)
	if err != nil {
		return FleetReport{}, err
	}
	owner, err := fleetOwner(input.Owner)
	if err != nil {
		return FleetReport{}, err
	}
	if input.Prune && owner != config.ManagedByFleet {
		return FleetReport{}, fmt.Errorf("%w: --prune requires --owner fleet", ErrFleetInput)
	}
	docs, err := parseBundle(cfg, input.Bundle)
	if err != nil {
		return FleetReport{Phase: FleetPhasePlan, Results: []FleetResult{}}, err
	}
	for _, doc := range docs {
		if doc.Profile.EffectiveManagedBy() != owner {
			return FleetReport{Phase: FleetPhasePlan, Results: []FleetResult{}}, fmt.Errorf("%w: profile %s has managed_by %s, want %s", ErrFleetInput, doc.Name, doc.Profile.EffectiveManagedBy(), owner)
		}
	}
	report := FleetReport{Phase: FleetPhasePlan, PlanID: "", Results: []FleetResult{}}
	err = m.withFleetLock(func() error {
		inv, err := readInventory(cfg.Paths.ProfilesDir)
		if err != nil {
			return err
		}
		intents, intentErrs := loadIntents(cfg)
		var targets []fleetTarget
		var results []FleetResult
		rejected := false
		reject := func(name, action, message string) {
			results = append(results, FleetResult{Name: name, Action: action, Outcome: FleetOutcomePlanned, Restart: FleetRestartNone, Message: message})
			rejected = true
		}
		for _, ie := range intentErrs {
			reject("", FleetActionError, ie.Error())
		}
		covered := map[string]bool{}
		for _, doc := range docs {
			covered[doc.Name] = true
			if doc.Profile.EffectiveManagedBy() != owner {
				reject(doc.Name, FleetActionConflict, fmt.Sprintf("profile managed_by is %s, want %s; use adopt first", doc.Profile.EffectiveManagedBy(), owner))
				continue
			}
			t, messages := m.prepareApplyTarget(ctx, cfg, inv, intents, doc, owner, input.NoStart)
			for _, message := range messages {
				reject(doc.Name, FleetActionConflict, message)
			}
			if len(messages) == 0 {
				targets = append(targets, t)
			}
		}
		if input.Prune && len(inv.Bad) > 0 {
			for _, bad := range inv.Bad {
				reject(profileNameFromPath(bad.Path), FleetActionError, "cannot decide ownership: "+bad.Error)
			}
		}
		if input.Prune && len(inv.Bad) == 0 {
			for _, disk := range inv.ByName {
				if covered[disk.Name] || disk.Profile.EffectiveManagedBy() != config.ManagedByFleet {
					continue
				}
				t, messages := m.prepareDeleteTarget(ctx, cfg, inv, intents, disk.Name, config.ManagedByFleet, false)
				covered[disk.Name] = true
				for _, message := range messages {
					reject(disk.Name, FleetActionConflict, message)
				}
				if len(messages) == 0 {
					targets = append(targets, t)
				}
			}
		}
		for name, in := range intents {
			if covered[name] {
				continue
			}
			switch in.Operation {
			case "delete":
				if in.Owner != config.ManagedByFleet || !input.Prune {
					reject(name, FleetActionError, incompleteIntentMessage(in))
					continue
				}
				t, messages := m.prepareDeleteTarget(ctx, cfg, inv, intents, name, config.ManagedByFleet, false)
				for _, message := range messages {
					reject(name, FleetActionConflict, message)
				}
				if len(messages) == 0 {
					targets = append(targets, t)
				}
			default:
				reject(name, FleetActionError, incompleteIntentMessage(in))
			}
		}
		targets = dedupeTargets(targets)
		if len(targets) > 1 {
			claims := map[string]string{}
			for _, t := range targets {
				if other, ok := claims[t.entry.Service]; ok {
					reject(t.entry.Name, FleetActionConflict, "duplicate unit claim with "+other)
				}
				claims[t.entry.Service] = t.entry.Name
			}
			// Drop targets rejected after the first duplicate.
			var kept []fleetTarget
			for _, t := range targets {
				bad := false
				for _, r := range results {
					if r.Name == t.entry.Name && r.Action == FleetActionConflict {
						bad = true
					}
				}
				if !bad {
					kept = append(kept, t)
				}
			}
			targets = kept
		}
		sort.Slice(targets, func(i, j int) bool { return targets[i].entry.Name < targets[j].entry.Name })
		report.Results = append([]FleetResult{}, results...)
		for _, t := range targets {
			report.Results = append(report.Results, t.result(t.entry.Action))
		}
		sort.SliceStable(report.Results, func(i, j int) bool { return report.Results[i].Name < report.Results[j].Name })
		planID, err := fleetPlanID(fleetPlanIdentity{Operation: applyOperation(input.Prune), Owner: owner, Prune: input.Prune, NoStart: input.NoStart, Targets: entriesOf(targets)})
		if err != nil {
			return err
		}
		report.PlanID = planID
		if rejected {
			report.Phase = FleetPhasePlan
			report.Complete = false
			return ErrFleetRejected
		}
		if input.ExpectPlan != "" && input.ExpectPlan != planID {
			for i := range report.Results {
				report.Results[i].Message = "plan_id does not match the approved plan; nothing was written"
			}
			report.Complete = false
			return ErrFleetRejected
		}
		if input.DryRun {
			report.Phase = FleetPhasePlan
			report.Complete = false
			return nil
		}
		ignoreTerminalHangup()
		report.Phase = FleetPhaseExecute
		report.Results = executeTargets(ctx, m, cfg, targets)
		report.Complete = true
		for _, r := range report.Results {
			if r.Outcome != FleetOutcomeDone {
				report.Complete = false
				break
			}
		}
		if !report.Complete {
			return ErrFleetPartial
		}
		return nil
	})
	if err != nil && (report.Phase == "" || errors.Is(err, ErrFleetLocked)) {
		report.Phase = FleetPhasePlan
		report.Complete = false
		if report.Results == nil {
			report.Results = []FleetResult{}
		}
	}
	return report, err
}

func (m RunnerManager) effectiveUnitDir() string {
	if m.SystemdUnitDir != "" {
		return m.SystemdUnitDir
	}
	return "/etc/systemd/system"
}

func dedupeTargets(targets []fleetTarget) []fleetTarget {
	seen := map[string]bool{}
	var kept []fleetTarget
	for _, t := range targets {
		if seen[t.entry.Name] {
			continue
		}
		seen[t.entry.Name] = true
		kept = append(kept, t)
	}
	return kept
}

func applyOperation(prune bool) string {
	if prune {
		return "apply-prune"
	}
	return "apply"
}

func deleteOperation(force bool) string {
	if force {
		return "delete-force"
	}
	return "delete"
}

func entriesOf(targets []fleetTarget) []fleetPlanEntry {
	entries := make([]fleetPlanEntry, 0, len(targets))
	for _, t := range targets {
		entries = append(entries, t.entry)
	}
	return entries
}

func inventoryBadFor(inv profileInventory, name string) string {
	for _, bad := range inv.Bad {
		if profileNameFromPath(bad.Path) == name {
			return "unparseable profile file for this name: " + bad.Error
		}
	}
	return ""
}

func profileNameFromPath(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(strings.TrimSuffix(base, ".yaml"), ".yml")
}

func incompleteIntentMessage(in fleetIntent) string {
	return fmt.Sprintf("incomplete %s intent for %s (digest %s); repair the profile or rerun the matching plan", in.Operation, in.Name, in.Digest)
}

func (m RunnerManager) prepareApplyTarget(ctx context.Context, cfg config.GlobalConfig, inv profileInventory, intents map[string]fleetIntent, doc bundleDocument, owner string, noStart bool) (fleetTarget, []string) {
	profilePath, err := confineJoin(cfg.Paths.ProfilesDir, doc.Name+".yaml")
	if err != nil {
		return fleetTarget{}, []string{err.Error()}
	}
	unitPath, err := confineJoin(m.effectiveUnitDir(), doc.Profile.Service.Name)
	if err != nil {
		return fleetTarget{}, []string{err.Error()}
	}
	if message := inventoryBadFor(inv, doc.Name); message != "" {
		return fleetTarget{}, []string{message}
	}
	t := fleetTarget{doc: doc, unitPath: unitPath, noStart: noStart,
		entry: fleetPlanEntry{Name: doc.Name, Path: profilePath, Service: doc.Profile.Service.Name, DiskSHA: "absent", InputSHA: doc.SHA, LoopBinary: cfg.Systemd.LoopBinaryPath, Marker: markerEvidence(cfg, doc.Name)}}
	disk, present := inv.ByName[doc.Name]
	t.disk, t.diskPresent = disk, present
	if present {
		t.entry.DiskSHA = disk.SHA
	}
	evidence, err := collectUnitEvidence(ctx, m, cfg, doc.Profile.Service.Name, profilePath)
	if err != nil {
		return t, []string{"unit evidence: " + err.Error()}
	}
	t.evidence = evidence
	t.entry.UnitEvidence = evidence.digest()
	in, hasIntent := intents[doc.Name]
	if hasIntent {
		switch in.Operation {
		case "apply-create", "apply-update":
			if in.Owner != owner || in.ProfilePath != profilePath || in.Service != doc.Profile.Service.Name {
				return t, []string{"another incomplete intent claims this profile; repair required"}
			}
			t.hasIntent, t.intent = true, in
			t.entry.Intent = in.Digest
		case "delete", "delete-force":
			return t, []string{"a " + in.Operation + " intent is incomplete; repair it before applying"}
		default:
			return t, []string{"another " + in.Operation + " intent is incomplete for this profile"}
		}
	}
	t.entry.LoopBinary = cfg.Systemd.LoopBinaryPath
	if t.hasIntent {
		if t.intent.ProfileSHA != doc.SHA {
			return t, []string{"pending " + t.intent.Operation + " intent was approved for different profile content; repair required"}
		}
		if t.intent.Operation == "apply-update" {
			// Restart-only recovery must still match the unit approved for the update.
			if t.intent.UnitPath != unitPath || t.intent.UnitEvidence != t.entry.UnitEvidence {
				return t, []string{"unit evidence changed since the pending update; repair required"}
			}
			if t.diskPresent && t.disk.SHA != t.intent.ProfileSHA && t.disk.SHA != t.intent.OriginalProfile {
				return t, []string{"profile changed outside the approved update; repair required"}
			}
		}
	}
	if t.hasIntent && t.intent.Operation == "apply-create" {
		if t.intent.NoStart != noStart {
			return t, []string{"pending create used a different --no-start value; repair required"}
		}
		if t.diskPresent {
			if t.disk.SHA != doc.SHA || t.disk.Path != profilePath {
				return t, []string{"profile exists with different content while an incomplete create is pending"}
			}
			if t.disk.Profile.EffectiveManagedBy() != owner {
				return t, []string{"profile ownership changed; use adopt first"}
			}
		}
		rendered, err := m.renderUnit(cfg, t)
		if err != nil {
			return t, []string{"unit template: " + err.Error()}
		}
		if shaOfBytes(rendered) != t.intent.UnitSHA {
			return t, []string{"rendered unit changed since the pending create"}
		}
		if evidence.DropInDir != "absent" {
			return t, []string{"unit has drop-ins while an incomplete create is pending"}
		}
		switch evidence.LoadState {
		case "loaded":
			if evidence.Fragment != unitPath {
				return t, []string{fmt.Sprintf("unit fragment is %s, not %s", evidence.Fragment, unitPath)}
			}
			if evidence.ConfigPath != profilePath {
				return t, []string{fmt.Sprintf("unit reads %s, not %s", evidence.ConfigPath, profilePath)}
			}
			if evidence.UnitFile == "symlink" || evidence.UnitFile == "not-regular" {
				return t, []string{"unit file is not a regular file"}
			}
		case "not-found":
			// A crash after writing the unit but before daemon-reload leaves a
			// not-found LoadState. The exact rendered bytes are the evidence.
			if evidence.UnitFile != "absent" {
				raw, readErr := os.ReadFile(unitPath)
				if readErr != nil || string(raw) != string(rendered) {
					return t, []string{"unit file does not match the pending create; repair required"}
				}
			}
		default:
			return t, []string{"unit LoadState is not supported: " + evidence.LoadState}
		}
		t.renderedUnit = rendered
		t.entry.RenderedUnit = shaOfBytes(rendered)
		t.entry.Action = FleetActionCreate
		return t, nil
	}
	if !t.diskPresent {
		if t.hasIntent && t.intent.Operation == "apply-update" {
			return t, []string{"update intent is pending but the profile is absent; repair required"}
		}
		for _, other := range inv.ByName {
			if other.Profile.Service.Name == doc.Profile.Service.Name {
				return t, []string{"duplicate unit claim with " + other.Name}
			}
		}
		if evidence.LoadState != "not-found" || evidence.UnitFile != "absent" || evidence.DropInDir != "absent" {
			return t, []string{"unit exists without profile"}
		}
		rendered, err := m.renderUnit(cfg, t)
		if err != nil {
			return t, []string{"unit template: " + err.Error()}
		}
		t.renderedUnit = rendered
		t.entry.RenderedUnit = shaOfBytes(rendered)
		t.entry.Action = FleetActionCreate
		return t, nil
	}
	if t.disk.Profile.EffectiveManagedBy() != owner {
		return t, []string{"profile belongs to " + t.disk.Profile.EffectiveManagedBy() + "; use adopt first"}
	}
	if t.disk.Path != profilePath {
		return t, []string{fmt.Sprintf("profile exists at %s, not %s", t.disk.Path, profilePath)}
	}
	if t.disk.Profile.Service.Name != doc.Profile.Service.Name {
		return t, []string{"service.name differs; delete before applying"}
	}
	for _, other := range inv.ByName {
		if other.Name != doc.Name && other.Profile.Service.Name == doc.Profile.Service.Name {
			return t, []string{"duplicate unit claim with " + other.Name}
		}
	}
	switch evidence.LoadState {
	case "loaded":
		if message := evidence.validate(); message != "" {
			return t, []string{"unit evidence is not safe: " + message}
		}
		if evidence.ConfigPath != profilePath {
			return t, []string{fmt.Sprintf("unit reads %s, not %s", evidence.ConfigPath, profilePath)}
		}
	case "not-found":
		if !evidence.confirmedAbsent() {
			return t, []string{"unit LoadState is not-found but unit paths are present"}
		}
		if t.entry.Marker != "" && !t.hasIntent {
			return t, []string{"unit not found but restart marker has no matching update intent; repair required"}
		}
		t.entry.Message = "unit not installed"
	default:
		return t, []string{"unit LoadState is not supported: " + evidence.LoadState}
	}
	oldProfile, newProfile := t.disk.Profile, doc.Profile
	oldProfile.Source, newProfile.Source = "", ""
	// Ownership was already checked above. An omitted managed_by and an
	// explicit local/fleet value mean the same owner, so do not rewrite or
	// restart just to add the line.
	oldProfile.ManagedBy, newProfile.ManagedBy = oldProfile.EffectiveManagedBy(), newProfile.EffectiveManagedBy()
	if reflect.DeepEqual(oldProfile, newProfile) {
		if t.disk.SHA == doc.SHA {
			switch {
			case RestartPending(cfg, doc.Name):
				t.entry.Action = FleetActionRestart
				t.entry.RestartNeeded = true
			case t.hasIntent:
				t.entry.Action = FleetActionUnchanged
				t.entry.Message = "previous steps completed; the pending intent will be cleared"
			default:
				t.entry.Action = FleetActionUnchanged
			}
		} else {
			t.entry.Action = FleetActionUpdate
			t.entry.RestartNeeded = false
		}
	} else {
		t.entry.Action = FleetActionUpdate
		t.entry.RestartNeeded = true
	}
	return t, nil
}

func (m RunnerManager) renderUnit(cfg config.GlobalConfig, t fleetTarget) ([]byte, error) {
	return renderServiceFileWithConfig(cfg, serviceTemplateData{
		ProfileName:       t.doc.Name,
		GitHubEnvFile:     t.doc.Profile.GitHub.EnvFile,
		LoopBinaryPath:    cfg.Systemd.LoopBinaryPath,
		ProfileConfigPath: t.entry.Path,
	})
}

func executeTargets(ctx context.Context, m RunnerManager, cfg config.GlobalConfig, targets []fleetTarget) []FleetResult {
	results := make([]FleetResult, 0, len(targets))
	stop := false
	for _, t := range targets {
		if stop {
			results = append(results, FleetResult{Name: t.entry.Name, Action: t.entry.Action, RestartNeeded: t.entry.RestartNeeded, Outcome: FleetOutcomeNotRun, Restart: FleetRestartNone, Message: "not run after an earlier failure"})
			continue
		}
		var result FleetResult
		var err error
		switch t.entry.Action {
		case FleetActionCreate:
			result, err = m.executeCreateTarget(ctx, cfg, t)
		case FleetActionUpdate:
			result, err = m.executeUpdateTarget(ctx, cfg, t)
		case FleetActionRestart:
			result, err = m.executeRestartTarget(ctx, cfg, t)
		case FleetActionUnchanged:
			result, err = m.executeUnchangedTarget(ctx, cfg, t)
		case FleetActionDelete:
			result, err = m.executeDeleteTarget(ctx, cfg, t)
		default:
			result, err = t.result(t.entry.Action), fmt.Errorf("unsupported action %s", t.entry.Action)
		}
		if err != nil {
			result.Outcome = FleetOutcomeFailed
			if result.Restart == "" {
				result.Restart = FleetRestartNone
			}
			if err.Error() != "" {
				result.Message = strings.TrimSpace(result.Message + " " + err.Error())
			}
			stop = true
		}
		results = append(results, result)
	}
	return results
}

func (m RunnerManager) executeUnchangedTarget(ctx context.Context, cfg config.GlobalConfig, t fleetTarget) (FleetResult, error) {
	if t.hasIntent {
		if err := m.clearApplyIntent(ctx, cfg, t); err != nil {
			return t.result(FleetActionUnchanged), err
		}
	}
	return FleetResult{Name: t.entry.Name, Action: FleetActionUnchanged, Outcome: FleetOutcomeDone, Restart: FleetRestartNone, Message: t.entry.Message}, nil
}

func (m RunnerManager) executeCreateTarget(ctx context.Context, cfg config.GlobalConfig, t fleetTarget) (FleetResult, error) {
	rendered := t.renderedUnit
	if rendered == nil {
		var err error
		rendered, err = m.renderUnit(cfg, t)
		if err != nil {
			return t.result(FleetActionCreate), err
		}
	}
	resumeMessage := ""
	if t.hasIntent {
		if t.intent.ProfileSHA != t.doc.SHA || t.intent.UnitSHA != shaOfBytes(rendered) || t.intent.NoStart != t.noStart {
			return t.result(FleetActionCreate), errors.New("pending create intent no longer matches the approved inputs")
		}
		resumeMessage = "resumed incomplete create"
	} else {
		in := fleetIntent{Operation: "apply-create", Name: t.entry.Name, Owner: t.doc.Profile.EffectiveManagedBy(),
			ProfilePath: t.entry.Path, UnitPath: t.unitPath, Service: t.entry.Service, NoStart: t.noStart,
			ProfileSHA: t.doc.SHA, UnitSHA: shaOfBytes(rendered), UnitEvidence: t.entry.UnitEvidence, OriginalProfile: "absent"}
		if err := writeIntent(cfg, in); err != nil {
			return t.result(FleetActionCreate), err
		}
	}
	if t.diskPresent {
		if err := verifyExistingContent(t.entry.Path, t.doc.Raw); err != nil {
			return t.result(FleetActionCreate), err
		}
	} else {
		if err := m.writeNewManagedFile(ctx, t.entry.Path, t.doc.Raw, 0o640); err != nil {
			return t.result(FleetActionCreate), err
		}
	}
	if fileExists(t.unitPath) {
		if err := verifyExistingContent(t.unitPath, rendered); err != nil {
			return t.result(FleetActionCreate), err
		}
	} else {
		if err := m.writeNewManagedFile(ctx, t.unitPath, rendered, 0o644); err != nil {
			return t.result(FleetActionCreate), err
		}
	}
	if err := m.Systemd.DaemonReload(ctx); err != nil {
		return t.result(FleetActionCreate), err
	}
	if err := m.Systemd.Enable(ctx, t.entry.Service); err != nil {
		return t.result(FleetActionCreate), err
	}
	if !t.noStart {
		if err := m.Systemd.Start(ctx, t.entry.Service); err != nil {
			return t.result(FleetActionCreate), err
		}
	}
	if err := removeIntent(cfg, t.entry.Name); err != nil {
		return t.result(FleetActionCreate), err
	}
	return FleetResult{Name: t.entry.Name, Action: FleetActionCreate, Outcome: FleetOutcomeDone, Restart: FleetRestartNone, Message: resumeMessage}, nil
}

func verifyExistingContent(path string, want []byte) error {
	got, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if string(got) != string(want) {
		return fmt.Errorf("%s already exists with different content; repair required", path)
	}
	return nil
}

func (m RunnerManager) executeUpdateTarget(ctx context.Context, cfg config.GlobalConfig, t fleetTarget) (FleetResult, error) {
	if !t.hasIntent {
		in := fleetIntent{Operation: "apply-update", Name: t.entry.Name, Owner: t.doc.Profile.EffectiveManagedBy(),
			ProfilePath: t.entry.Path, UnitPath: t.unitPath, Service: t.entry.Service,
			ProfileSHA: t.doc.SHA, OriginalProfile: t.disk.SHA, UnitEvidence: t.entry.UnitEvidence}
		if err := writeIntent(cfg, in); err != nil {
			return t.result(FleetActionUpdate), err
		}
	}
	if t.entry.RestartNeeded {
		if err := writeRestartMarker(cfg, t.entry.Name); err != nil {
			return t.result(FleetActionUpdate), err
		}
	}
	if !t.diskPresent {
		return t.result(FleetActionUpdate), errors.New("profile disappeared before update")
	}
	if _, err := backupProfile(cfg, t.entry.Name, t.disk.Raw); err != nil {
		return t.result(FleetActionUpdate), err
	}
	if err := writeFileAtomic(filepath.Dir(t.entry.Path), filepath.Base(t.entry.Path), t.doc.Raw, 0o640); err != nil {
		return t.result(FleetActionUpdate), err
	}
	result := FleetResult{Name: t.entry.Name, Action: FleetActionUpdate, RestartNeeded: t.entry.RestartNeeded, Outcome: FleetOutcomeDone, Restart: FleetRestartNone}
	if !t.entry.RestartNeeded {
		return result, m.clearApplyIntent(ctx, cfg, t)
	}
	return m.finishRestart(ctx, cfg, t, result)
}

func (e unitEvidence) confirmedAbsent() bool {
	return e.LoadState == "not-found" && e.UnitFile == "absent" && e.DropInDir == "absent" &&
		e.Fragment == "" && e.ConfigPath == "" && len(e.dropInPaths) == 0
}

func (m RunnerManager) verifyApplyUnitAbsent(ctx context.Context, cfg config.GlobalConfig, t fleetTarget) error {
	evidence, err := collectUnitEvidence(ctx, m, cfg, t.entry.Service, t.entry.Path)
	if err != nil {
		return fmt.Errorf("cannot confirm %s is still absent: %w", t.entry.Service, err)
	}
	if !evidence.confirmedAbsent() {
		return fmt.Errorf("%s is no longer confirmed absent; repair required", t.entry.Service)
	}
	return nil
}

func (m RunnerManager) clearApplyIntent(ctx context.Context, cfg config.GlobalConfig, t fleetTarget) error {
	if t.evidence.confirmedAbsent() {
		if err := m.verifyApplyUnitAbsent(ctx, cfg, t); err != nil {
			return err
		}
	}
	return removeIntent(cfg, t.entry.Name)
}

func (m RunnerManager) finishRestart(ctx context.Context, cfg config.GlobalConfig, t fleetTarget, result FleetResult) (FleetResult, error) {
	if t.evidence.confirmedAbsent() {
		if err := m.verifyApplyUnitAbsent(ctx, cfg, t); err != nil {
			result.Restart = FleetRestartFailed
			return result, fmt.Errorf("restart marker retained: %w", err)
		}
		result.Restart = FleetRestartSkipped
		result.Message = t.entry.Message
	} else {
		restart, err := m.restartLoadedUnit(ctx, t.entry.Service)
		result.Restart = restart
		if err != nil {
			return result, err
		}
	}
	if err := removeRestartMarker(cfg, t.entry.Name); err != nil {
		result.Restart = FleetRestartFailed
		return result, err
	}
	if err := removeIntent(cfg, t.entry.Name); err != nil {
		result.Restart = FleetRestartFailed
		return result, err
	}
	return result, nil
}

func (m RunnerManager) restartLoadedUnit(ctx context.Context, service string) (string, error) {
	info, err := m.Systemd.Show(ctx, service)
	if err != nil {
		return FleetRestartFailed, fmt.Errorf("cannot read current ActiveState for %s; restart marker retained: %w", service, err)
	}
	if info.LoadState != "loaded" {
		return FleetRestartFailed, fmt.Errorf("unexpected LoadState %q for %s; restart marker retained", info.LoadState, service)
	}
	switch info.ActiveState {
	case "active", "activating", "reloading":
		if err := m.Systemd.RestartNoBlock(ctx, service); err != nil {
			return FleetRestartFailed, err
		}
		return FleetRestartQueued, nil
	case "inactive", "failed", "deactivating":
		return FleetRestartSkipped, nil
	default:
		return FleetRestartFailed, fmt.Errorf("unexpected ActiveState %q for %s; restart marker retained", info.ActiveState, service)
	}
}

func (m RunnerManager) executeRestartTarget(ctx context.Context, cfg config.GlobalConfig, t fleetTarget) (FleetResult, error) {
	result := FleetResult{Name: t.entry.Name, Action: FleetActionRestart, RestartNeeded: true, Outcome: FleetOutcomeDone, Restart: FleetRestartNone}
	if !RestartPending(cfg, t.entry.Name) {
		if t.hasIntent {
			if err := m.clearApplyIntent(ctx, cfg, t); err != nil {
				result.Restart = FleetRestartFailed
				return result, err
			}
		}
		return result, nil
	}
	return m.finishRestart(ctx, cfg, t, result)
}

// --- delete ---------------------------------------------------------------

func (m RunnerManager) prepareDeleteTarget(ctx context.Context, cfg config.GlobalConfig, inv profileInventory, intents map[string]fleetIntent, name, owner string, force bool) (fleetTarget, []string) {
	in, hasIntent := intents[name]
	if hasIntent {
		expected := deleteOperation(force)
		if in.Operation != expected {
			if in.Operation == "delete" || in.Operation == "delete-force" {
				return fleetTarget{}, []string{"a delete with a different --force value is incomplete; rerun it first"}
			}
			return fleetTarget{}, []string{"another " + in.Operation + " intent is incomplete for this profile"}
		}
		if in.Owner != owner {
			return fleetTarget{}, []string{"delete intent belongs to " + in.Owner + "; repair required"}
		}
	}
	if message := inventoryBadFor(inv, name); message != "" {
		return fleetTarget{}, []string{message}
	}
	disk, present := inv.ByName[name]
	if hasIntent && present && (disk.Path != in.ProfilePath || disk.Profile.Service.Name != in.Service) {
		return fleetTarget{}, []string{"live profile does not match the delete intent; repair required"}
	}
	if !present {
		if !hasIntent {
			return fleetTarget{}, []string{"profile not found"}
		}
		disk = diskProfile{Name: name, Path: in.ProfilePath, SHA: in.ProfileSHA,
			Profile: config.Profile{Name: name, Service: config.ServiceConfig{Name: in.Service}, ManagedBy: in.Owner}}
	}
	t := fleetTarget{disk: disk, diskPresent: present, force: force,
		entry: fleetPlanEntry{Name: name, Path: disk.Path, Service: disk.Profile.Service.Name, DiskSHA: disk.SHA, InputSHA: "absent", Action: FleetActionDelete}}
	if hasIntent {
		t.hasIntent, t.intent = true, in
		t.entry.Intent = in.Digest
		t.entry.Path, t.entry.Service = in.ProfilePath, in.Service
		if in.ProfileSHA != "" {
			t.entry.DiskSHA = in.ProfileSHA
		}
		if in.UnitEvidence != "" {
			t.entry.UnitEvidence = in.UnitEvidence
		}
	}
	unitDir := m.effectiveUnitDir()
	unitPath, err := confineJoin(unitDir, disk.Profile.Service.Name)
	if err != nil {
		return t, []string{err.Error()}
	}
	if hasIntent && in.UnitPath != unitPath {
		return t, []string{"unit path does not match the delete intent; repair required"}
	}
	t.unitPath = unitPath
	evidence, err := collectUnitEvidence(ctx, m, cfg, disk.Profile.Service.Name, disk.Path)
	if err != nil {
		return t, []string{"unit evidence: " + err.Error()}
	}
	t.evidence = evidence
	t.entry.UnitEvidence = evidence.digest()
	t.entry.Marker = markerEvidence(cfg, name)
	if !hasIntent {
		t.intent = fleetIntent{ArchiveDir: newDeleteArchiveDir(cfg, name, evidence.digest())}
	}
	t.entry.Evidence = deleteEvidenceDigest(t)
	if present && disk.Profile.EffectiveManagedBy() != owner {
		return t, []string{"profile belongs to " + disk.Profile.EffectiveManagedBy() + "; use adopt first"}
	}
	for _, other := range inv.ByName {
		if other.Name != name && other.Profile.Service.Name == disk.Profile.Service.Name {
			return t, []string{"duplicate unit claim with " + other.Name}
		}
	}
	// A crash can leave any subset of the profile, unit, drop-in directory,
	// and restart marker live or archived. Verify each artifact independently
	// against the durable intent before touching systemd again.
	if hasIntent {
		if message := deleteArchiveDirProblem(cfg, in.ArchiveDir); message != "" {
			return t, []string{message}
		}
		state, err := inspectDeleteArtifacts(cfg, in, evidence, true)
		if err != nil {
			return t, []string{err.Error()}
		}
		if state.allArchived() {
			t.entry.Message = "resuming archived delete; completing daemon-reload"
		} else {
			t.entry.Message = "resuming interrupted delete"
		}
		return t, nil
	}
	switch evidence.LoadState {
	case "not-found":
		if evidence.UnitFile != "absent" || evidence.DropInDir != "absent" {
			return t, []string{"unit files exist while LoadState is not-found"}
		}
		if present {
			t.entry.Message = "unit not installed; profile-only delete"
		}
	case "loaded":
		if message := evidence.validate(); message != "" {
			return t, []string{"unit evidence is not safe: " + message}
		}
		if evidence.Fragment != unitPath {
			return t, []string{fmt.Sprintf("unit fragment is %s, not %s", evidence.Fragment, unitPath)}
		}
		if evidence.UnitFile == "symlink" || evidence.UnitFile == "not-regular" {
			return t, []string{"unit file is not a regular file"}
		}
		if evidence.ConfigPath != disk.Path {
			return t, []string{fmt.Sprintf("unit reads %s, not %s", evidence.ConfigPath, disk.Path)}
		}
		if message := evidence.dropInsInOwnDir(unitPath); message != "" {
			return t, []string{message}
		}
	default:
		return t, []string{"unit LoadState is not supported: " + evidence.LoadState}
	}
	return t, nil
}

func deleteArchiveDirProblem(cfg config.GlobalConfig, dir string) string {
	if dir == "" {
		return "delete intent has no archive destination; repair required"
	}
	root := backupDir(cfg)
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "delete intent archive destination is outside profile-backups; repair required"
	}
	return ""
}

type deleteArtifactState struct {
	profileLive     bool
	unitLive        bool
	dropInsLive     bool
	markerLive      bool
	duplicates      []string
	foreignFragment string
}

func (s deleteArtifactState) allArchived() bool {
	return !s.profileLive && !s.unitLive && !s.dropInsLive && !s.markerLive && len(s.duplicates) == 0
}

func verifyFileSHA(path, want, label string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file; repair required", label)
	}
	got, err := fileSHA(path)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if got != want {
		return fmt.Errorf("%s changed after the delete plan; repair required", label)
	}
	return nil
}

func unitContentKnown(value string) bool {
	return value != "" && value != "absent" && value != "symlink" && value != "not-regular"
}

// inspectDeleteArtifacts verifies each planned artifact independently. Every
// artifact must be exactly live or archived; a live copy whose verified
// archive copy already exists is a duplicate safe to remove. Once everything
// is archived, a verified foreign fragment can remain loaded.
func inspectDeleteArtifacts(cfg config.GlobalConfig, in fleetIntent, evidence unitEvidence, allowForeign bool) (deleteArtifactState, error) {
	var state deleteArtifactState
	if in.ArchiveDir == "" {
		return state, errors.New("delete intent has no archive destination; repair required")
	}
	archivePath := func(name string) string { return filepath.Join(in.ArchiveDir, name) }

	profileArchive := archivePath(filepath.Base(in.ProfilePath))
	profileLive := fileExists(in.ProfilePath)
	profileArchived := fileExists(profileArchive)
	if in.ProfileSHA == "" || in.ProfileSHA == "absent" {
		if profileLive || profileArchived {
			return state, errors.New("delete intent expected no profile but one exists; repair required")
		}
	} else {
		if profileLive {
			if err := verifyFileSHA(in.ProfilePath, in.ProfileSHA, "profile"); err != nil {
				return state, err
			}
		}
		if profileArchived {
			if err := verifyFileSHA(profileArchive, in.ProfileSHA, "archived profile"); err != nil {
				return state, err
			}
		}
		if !profileLive && !profileArchived {
			return state, errors.New("profile is neither live nor archived; repair required")
		}
	}
	state.profileLive = profileLive
	if profileLive && profileArchived {
		state.profileLive = false
		state.duplicates = append(state.duplicates, in.ProfilePath)
	}

	unitArchive := archivePath(filepath.Base(in.UnitPath))
	unitLive := fileExists(in.UnitPath)
	unitArchived := fileExists(unitArchive)
	if in.HadUnit {
		if !unitContentKnown(in.OriginalUnit) {
			return state, errors.New("delete intent does not record the planned unit content; repair required")
		}
		if unitLive && evidence.UnitFile != in.OriginalUnit {
			return state, errors.New("unit file changed after the delete plan; repair required")
		}
		if unitArchived {
			if err := verifyFileSHA(unitArchive, in.OriginalUnit, "archived unit"); err != nil {
				return state, err
			}
		}
		if !unitLive && !unitArchived {
			return state, errors.New("unit is neither live nor archived; repair required")
		}
	} else if unitLive || unitArchived {
		return state, errors.New("unit appeared after the delete plan; repair required")
	}
	state.unitLive = unitLive
	if unitLive && unitArchived {
		state.unitLive = false
		state.duplicates = append(state.duplicates, in.UnitPath)
	}

	dropInDir := in.UnitPath + ".d"
	dropInArchive := archivePath(filepath.Base(dropInDir))
	dropInsLive := fileExists(dropInDir)
	dropInsArchived := fileExists(dropInArchive)
	if in.HadDropIns {
		if in.OriginalDropIns == "" || in.OriginalDropIns == "absent" {
			return state, errors.New("delete intent does not record the planned drop-ins; repair required")
		}
		if dropInsLive && evidence.DropInDir != in.OriginalDropIns {
			return state, errors.New("live drop-ins changed after the delete plan; repair required")
		}
		if dropInsArchived {
			digest, lines, err := readDropInEvidence(dropInArchive)
			if err != nil {
				return state, fmt.Errorf("archived drop-ins: %w", err)
			}
			if digest != in.OriginalDropIns {
				return state, errors.New("archived drop-ins changed after the delete plan; repair required")
			}
			if !dropInsLive {
				evidence.DropInDir, evidence.DropIns = digest, lines
			}
		}
		if !dropInsLive && !dropInsArchived {
			return state, errors.New("drop-ins are neither live nor archived; repair required")
		}
	} else if dropInsLive || dropInsArchived {
		return state, errors.New("drop-ins appeared after the delete plan; repair required")
	}
	state.dropInsLive = dropInsLive
	if dropInsLive && dropInsArchived {
		state.dropInsLive = false
		state.duplicates = append(state.duplicates, dropInDir)
	}
	markerLive := RestartPending(cfg, in.Name)
	markerArchived := fileExists(archivePath(in.Name))
	if in.HadMarker {
		if !markerLive && !markerArchived {
			return state, errors.New("restart marker is neither live nor archived; repair required")
		}
	} else if markerLive || markerArchived {
		return state, errors.New("restart marker appeared after the delete plan; repair required")
	}
	state.markerLive = markerLive
	if markerLive && markerArchived {
		state.markerLive = false
		state.duplicates = append(state.duplicates, restartPendingPath(cfg, in.Name))
	}
	if allowForeign && state.allArchived() && in.HadUnit && evidence.LoadState == "loaded" && evidence.Fragment != in.UnitPath {
		if err := verifyForeignDeleteFragment(cfg, in, evidence); err != nil {
			return state, err
		}
		state.foreignFragment = evidence.Fragment
	} else if message := deleteUnitIdentityProblem(in, evidence, unitLive); message != "" {
		return state, errors.New(message)
	}
	return state, nil
}

func verifyForeignDeleteFragment(cfg config.GlobalConfig, in fleetIntent, evidence unitEvidence) error {
	path := evidence.Fragment
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) != in.Service || evidence.FragmentSHA == "" {
		return fmt.Errorf("cannot verify foreign fragment %q; repair required", path)
	}
	fragment, err := os.Lstat(path)
	if err != nil || !fragment.Mode().IsRegular() {
		return fmt.Errorf("cannot verify foreign fragment %q; repair required", path)
	}
	archived, err := os.Lstat(filepath.Join(in.ArchiveDir, filepath.Base(in.UnitPath)))
	if err != nil || os.SameFile(fragment, archived) {
		return fmt.Errorf("cannot distinguish foreign fragment %q from the archive; repair required", path)
	}
	// Absence here must be confirmed, not inferred from a failed stat.
	for _, live := range []string{in.ProfilePath, in.UnitPath, in.UnitPath + ".d", restartPendingPath(cfg, in.Name)} {
		if _, err := os.Lstat(live); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("cannot confirm %s is absent after archive; repair required", live)
		}
	}
	return nil
}

func deleteUnitIdentityProblem(in fleetIntent, evidence unitEvidence, unitLive bool) string {
	if !unitLive && evidence.LoadState == "not-found" {
		return ""
	}
	if !in.HadUnit || evidence.LoadState != "loaded" {
		return fmt.Sprintf("unexpected unit LoadState %q during delete; repair required", evidence.LoadState)
	}
	if evidence.Fragment != in.UnitPath {
		return fmt.Sprintf("unit fragment is %s, not %s", evidence.Fragment, in.UnitPath)
	}
	if !unitLive {
		// Before reload, systemd can still report the previous unit. Match
		// its cached identity using the already verified archived fragment.
		evidence.UnitFile, evidence.FragmentSHA = in.OriginalUnit, in.OriginalUnit
	}
	if in.EffectiveConfig != "" && evidence.ConfigPath != in.EffectiveConfig {
		return fmt.Sprintf("unit reads %s, not %s", evidence.ConfigPath, in.EffectiveConfig)
	}
	if message := evidence.validate(); message != "" {
		return "unit evidence is not safe: " + message
	}
	if in.UnitEvidence != "" && evidence.digest() != in.UnitEvidence {
		return "unit evidence changed after the delete plan; repair required"
	}
	return evidence.dropInsInOwnDir(in.UnitPath)
}

func removeLiveDuplicate(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		err = os.RemoveAll(path)
	} else {
		err = os.Remove(path)
	}
	if err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func deleteEvidenceDigest(t fleetTarget) string {
	data, err := json.Marshal(struct {
		ProfileSHA       string   `json:"profile_sha"`
		ProfilePath      string   `json:"profile_path"`
		UnitPath         string   `json:"unit_path"`
		UnitFile         string   `json:"unit_file"`
		DropInDir        string   `json:"dropin_dir"`
		DropIns          []string `json:"dropins"`
		EffectiveDropIns []string `json:"effective_dropins"`
		ConfigPath       string   `json:"config_path"`
		ArchiveDir       string   `json:"archive_dir"`
		Marker           string   `json:"marker"`
	}{t.entry.DiskSHA, t.entry.Path, t.unitPath, t.evidence.UnitFile, t.evidence.DropInDir, t.evidence.DropIns, t.evidence.dropInPaths, t.evidence.ConfigPath, t.intent.ArchiveDir, t.entry.Marker})
	if err != nil {
		return ""
	}
	return shaOfBytes(data)
}

func (e unitEvidence) dropInsInOwnDir(unitPath string) string {
	dir := filepath.Clean(unitPath + ".d")
	for _, path := range e.dropInPaths {
		clean := filepath.Clean(path)
		if !filepath.IsAbs(clean) || !strings.HasPrefix(clean, dir+string(os.PathSeparator)) {
			return "unit uses a drop-in outside its own directory: " + path
		}
	}
	return ""
}

func (m RunnerManager) Delete(ctx context.Context, input DeleteInput) (FleetReport, error) {
	cfg, err := config.LoadGlobalConfig(m.ConfigPath)
	if err != nil {
		return FleetReport{}, err
	}
	if !safeManagedName(input.Name) {
		return FleetReport{}, fmt.Errorf("%w: unsafe profile name", ErrFleetInput)
	}
	owner, err := fleetOwner(input.Owner)
	if err != nil {
		return FleetReport{}, err
	}
	report := FleetReport{Phase: FleetPhasePlan, Results: []FleetResult{}}
	err = m.withFleetLock(func() error {
		inv, err := readInventory(cfg.Paths.ProfilesDir)
		if err != nil {
			return err
		}
		intents, intentErrs := loadIntents(cfg)
		var rejected bool
		for _, ie := range intentErrs {
			report.Results = append(report.Results, FleetResult{Name: input.Name, Action: FleetActionError, Outcome: FleetOutcomePlanned, Restart: FleetRestartNone, Message: ie.Error()})
			rejected = true
		}
		for name, in := range intents {
			if name != input.Name {
				report.Results = append(report.Results, FleetResult{Name: name, Action: FleetActionError, Outcome: FleetOutcomePlanned, Restart: FleetRestartNone, Message: incompleteIntentMessage(in)})
				rejected = true
			}
		}
		target, messages := m.prepareDeleteTarget(ctx, cfg, inv, intents, input.Name, owner, input.Force)
		if len(messages) > 0 || rejected {
			for _, message := range messages {
				report.Results = append(report.Results, FleetResult{Name: input.Name, Action: FleetActionConflict, Outcome: FleetOutcomePlanned, Restart: FleetRestartNone, Message: message})
			}
			report.Complete = false
			return ErrFleetRejected
		}
		planID, err := fleetPlanID(fleetPlanIdentity{Operation: deleteOperation(input.Force), Owner: owner, Targets: []fleetPlanEntry{target.entry}})
		if err != nil {
			return err
		}
		report.PlanID = planID
		report.Results = []FleetResult{target.result(FleetActionDelete)}
		ignoreTerminalHangup()
		report.Phase = FleetPhaseExecute
		result, execErr := m.executeDeleteTarget(ctx, cfg, target)
		if execErr != nil {
			result.Outcome = FleetOutcomeFailed
			if execErr.Error() != "" {
				result.Message = strings.TrimSpace(result.Message + " " + execErr.Error())
			}
		}
		report.Results = []FleetResult{result}
		report.Complete = result.Outcome == FleetOutcomeDone && execErr == nil
		if execErr != nil {
			return execErr
		}
		if !report.Complete {
			return ErrFleetPartial
		}
		return nil
	})
	if err != nil && report.Phase == "" {
		report.Phase = FleetPhasePlan
		report.Complete = false
		if report.Results == nil {
			report.Results = []FleetResult{}
		}
	}
	return report, err
}

func (m RunnerManager) executeDeleteTarget(ctx context.Context, cfg config.GlobalConfig, t fleetTarget) (FleetResult, error) {
	result := FleetResult{Name: t.entry.Name, Action: FleetActionDelete, Outcome: FleetOutcomeDone, Restart: FleetRestartNone, Message: t.entry.Message}
	if t.intent.ArchiveDir == "" {
		return result, errors.New("delete intent has no archive destination")
	}
	if !t.hasIntent {
		t.intent = fleetIntent{Operation: deleteOperation(t.force), Name: t.entry.Name, Owner: t.disk.Profile.EffectiveManagedBy(),
			ProfilePath: t.entry.Path, UnitPath: t.unitPath, Service: t.entry.Service,
			ProfileSHA: t.entry.DiskSHA, UnitEvidence: t.entry.UnitEvidence,
			OriginalUnit: t.evidence.UnitFile, OriginalDropIns: t.evidence.DropInDir,
			HadUnit: t.evidence.LoadState == "loaded", HadDropIns: t.evidence.DropInDir != "absent",
			HadMarker: t.entry.Marker != "", ArchiveDir: t.intent.ArchiveDir,
			FragmentPath: t.evidence.Fragment, EffectiveConfig: t.evidence.ConfigPath}
	}
	// Re-read systemd and every artifact immediately before the destructive
	// steps. An earlier create in the same apply may have reloaded an edited
	// drop-in, so the plan-time ExecStart is not sufficient evidence here.
	evidence, err := collectUnitEvidence(ctx, m, cfg, t.entry.Service, t.entry.Path)
	if err != nil {
		return result, err
	}
	state, err := inspectDeleteArtifacts(cfg, t.intent, evidence, t.hasIntent)
	if err != nil {
		return result, err
	}
	if !t.hasIntent {
		if err := writeIntent(cfg, t.intent); err != nil {
			return result, err
		}
	}
	if state.unitLive || slices.Contains(state.duplicates, t.intent.UnitPath) {
		stopCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
		err := m.Systemd.Stop(stopCtx, t.entry.Service)
		cancel()
		if err != nil {
			result.Message = "stop failed or timed out; nothing was archived"
			return result, err
		}
		if err := m.Systemd.Disable(ctx, t.entry.Service); err != nil {
			result.Message = "disable failed; nothing was archived"
			return result, err
		}
	}
	for _, path := range state.duplicates {
		if err := removeLiveDuplicate(path); err != nil {
			return result, err
		}
	}
	artifacts := []struct {
		live bool
		path string
	}{
		{state.profileLive, t.intent.ProfilePath},
		{state.unitLive, t.intent.UnitPath},
		{state.dropInsLive, t.intent.UnitPath + ".d"},
		{state.markerLive, restartPendingPath(cfg, t.intent.Name)},
	}
	for _, artifact := range artifacts {
		if !artifact.live {
			continue
		}
		if err := os.MkdirAll(t.intent.ArchiveDir, 0o700); err != nil {
			return result, err
		}
		if err := movePath(artifact.path, filepath.Join(t.intent.ArchiveDir, filepath.Base(artifact.path))); err != nil {
			return result, err
		}
	}
	postEvidence := evidence
	if t.intent.HadUnit {
		if err := m.Systemd.DaemonReload(ctx); err != nil {
			return result, err
		}
		postEvidence, err = collectUnitEvidence(ctx, m, cfg, t.entry.Service, t.entry.Path)
		if err != nil {
			return result, err
		}
	}
	post, err := inspectDeleteArtifacts(cfg, t.intent, postEvidence, true)
	if err != nil {
		return result, err
	}
	if !post.allArchived() {
		return result, errors.New("delete did not archive every artifact; repair required")
	}
	if t.intent.HadUnit && postEvidence.LoadState != "not-found" && post.foreignFragment == "" {
		return result, fmt.Errorf("unit LoadState is %s after archive; repair required", postEvidence.LoadState)
	}
	if post.foreignFragment != "" {
		result.Message = strings.TrimSpace(result.Message + fmt.Sprintf(" foreign unit at %s left untouched", post.foreignFragment))
	}
	if t.force {
		if _, err := m.ForceRemoveProfile(ctx, t.disk.Profile); err != nil {
			return result, err
		}
	}
	if err := removeIntent(cfg, t.entry.Name); err != nil {
		return result, err
	}
	return result, nil
}

func newDeleteArchiveDir(cfg config.GlobalConfig, name, evidence string) string {
	if len(evidence) > 12 {
		evidence = evidence[:12]
	}
	base := filepath.Join(backupDir(cfg), "deleted-"+name+"-"+evidence)
	candidate := base
	for i := 1; fileExists(candidate); i++ {
		candidate = fmt.Sprintf("%s-%d", base, i)
	}
	return candidate
}

func movePath(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return syncDir(filepath.Dir(dst))
	}
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case info.Mode().IsRegular():
		if err := copyFile(src, dst, info.Mode().Perm()); err != nil {
			return err
		}
	case info.IsDir():
		if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := movePath(filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name())); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("refusing to move non-regular path %s", src)
	}
	return os.Remove(src)
}
