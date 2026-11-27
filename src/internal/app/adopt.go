package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"

	"gha-runner-tui/internal/config"

	"gopkg.in/yaml.v3"
)

func (m RunnerManager) Adopt(ctx context.Context, input AdoptInput) (FleetReport, error) {
	cfg, err := config.LoadGlobalConfig(m.ConfigPath)
	if err != nil {
		return FleetReport{}, err
	}
	owner, err := fleetOwner(input.Owner)
	if err != nil {
		return FleetReport{}, err
	}
	if len(input.Names) > 0 && input.Match != "" {
		return FleetReport{}, fmt.Errorf("%w: use names or --match, not both", ErrFleetInput)
	}
	if len(input.Names) == 0 && input.Match == "" {
		return FleetReport{}, fmt.Errorf("%w: a profile name or --match is required", ErrFleetInput)
	}
	for _, name := range input.Names {
		if !safeManagedName(name) {
			return FleetReport{}, fmt.Errorf("%w: unsafe profile name %q", ErrFleetInput, name)
		}
	}
	report := FleetReport{Phase: FleetPhasePlan, Results: []FleetResult{}}
	err = m.withFleetLock(func() error {
		names := append([]string{}, input.Names...)
		if input.Match != "" {
			matched, err := matchNames(cfg.Paths.ProfilesDir, input.Match)
			if err != nil {
				return err
			}
			names = matched
			if len(names) == 0 {
				report.Results = []FleetResult{}
				report.Complete = false
				return ErrFleetRejected
			}
		}
		sort.Strings(names)
		inv, err := readInventory(cfg.Paths.ProfilesDir)
		if err != nil {
			return err
		}
		badByName := map[string]ExportError{}
		for _, bad := range inv.Bad {
			badByName[profileNameFromPath(bad.Path)] = bad
		}
		type adoptTarget struct {
			name, path string
			raw, next  []byte
			newSHA     string
			evidence   unitEvidence
			action     string
			message    string
		}
		var targets []adoptTarget
		rejected := false
		var results []FleetResult
		for _, name := range names {
			if bad, exists := badByName[name]; exists {
				results = append(results, FleetResult{Name: name, Action: FleetActionError, Outcome: FleetOutcomePlanned, Restart: FleetRestartNone, Message: bad.Error})
				rejected = true
				continue
			}
			entry, ok := inv.ByName[name]
			if !ok {
				results = append(results, FleetResult{Name: name, Action: FleetActionError, Outcome: FleetOutcomePlanned, Restart: FleetRestartNone, Message: "profile not found"})
				rejected = true
				continue
			}
			path, raw, profile := entry.Path, entry.Raw, entry.Profile
			evidence, err := collectUnitEvidence(ctx, m, cfg, profile.Service.Name, path)
			if err != nil {
				results = append(results, FleetResult{Name: name, Action: FleetActionError, Outcome: FleetOutcomePlanned, Restart: FleetRestartNone, Message: "unit evidence: " + err.Error()})
				rejected = true
				continue
			}
			if evidence.LoadState == "loaded" {
				if message := evidence.validate(); message != "" {
					results = append(results, FleetResult{Name: name, Action: FleetActionConflict, Outcome: FleetOutcomePlanned, Restart: FleetRestartNone, Message: "unit evidence is not safe: " + message})
					rejected = true
					continue
				}
				if evidence.ConfigPath != path {
					results = append(results, FleetResult{Name: name, Action: FleetActionConflict, Outcome: FleetOutcomePlanned, Restart: FleetRestartNone, Message: fmt.Sprintf("unit reads %s, not %s", evidence.ConfigPath, path)})
					rejected = true
					continue
				}
			} else if evidence.LoadState != "not-found" {
				results = append(results, FleetResult{Name: name, Action: FleetActionConflict, Outcome: FleetOutcomePlanned, Restart: FleetRestartNone, Message: "unit LoadState is not supported: " + evidence.LoadState})
				rejected = true
				continue
			} else if evidence.UnitFile != "absent" || evidence.DropInDir != "absent" {
				results = append(results, FleetResult{Name: name, Action: FleetActionConflict, Outcome: FleetOutcomePlanned, Restart: FleetRestartNone, Message: "unit LoadState is not-found but files exist"})
				rejected = true
				continue
			}
			next, err := setManagedByLine(raw, owner)
			if err != nil {
				results = append(results, FleetResult{Name: name, Action: FleetActionError, Outcome: FleetOutcomePlanned, Restart: FleetRestartNone, Message: err.Error()})
				rejected = true
				continue
			}
			var updated config.Profile
			if err := yaml.Unmarshal(next, &updated); err != nil {
				results = append(results, FleetResult{Name: name, Action: FleetActionError, Outcome: FleetOutcomePlanned, Restart: FleetRestartNone, Message: "rewritten profile does not parse: " + err.Error()})
				rejected = true
				continue
			}
			if err := updated.Validate(); err != nil {
				results = append(results, FleetResult{Name: name, Action: FleetActionError, Outcome: FleetOutcomePlanned, Restart: FleetRestartNone, Message: err.Error()})
				rejected = true
				continue
			}
			if updated.ManagedBy != owner {
				results = append(results, FleetResult{Name: name, Action: FleetActionError, Outcome: FleetOutcomePlanned, Restart: FleetRestartNone, Message: "rewritten managed_by is not " + owner})
				rejected = true
				continue
			}
			before, after := profile, updated
			before.Source, after.Source = "", ""
			before.ManagedBy, after.ManagedBy = "", ""
			if !reflect.DeepEqual(before, after) {
				results = append(results, FleetResult{Name: name, Action: FleetActionError, Outcome: FleetOutcomePlanned, Restart: FleetRestartNone, Message: "adopt would change fields other than managed_by"})
				rejected = true
				continue
			}
			action := FleetActionAdopt
			message := ""
			if evidence.LoadState == "not-found" {
				message = "unit not installed"
			}
			if profile.EffectiveManagedBy() == owner {
				action = FleetActionUnchanged
				if message != "" {
					message += "; "
				}
				message += "already " + owner
			}
			targets = append(targets, adoptTarget{name: name, path: path, raw: raw, next: next, newSHA: shaOfBytes(next), evidence: evidence, action: action, message: message})
		}
		if rejected {
			report.Results = append(report.Results, results...)
			report.Complete = false
			return ErrFleetRejected
		}
		identity := fleetPlanIdentity{Operation: "adopt", Owner: owner}
		for _, t := range targets {
			identity.Targets = append(identity.Targets, fleetPlanEntry{Name: t.name, Path: t.path, Service: t.evidence.Service, Action: t.action, DiskSHA: shaOfBytes(t.raw), InputSHA: t.newSHA, UnitEvidence: t.evidence.digest(), Message: t.message})
		}
		planID, err := fleetPlanID(identity)
		if err != nil {
			return err
		}
		report.PlanID = planID
		for _, t := range targets {
			report.Results = append(report.Results, FleetResult{Name: t.name, Action: t.action, Outcome: FleetOutcomePlanned, Restart: FleetRestartNone, Message: t.message})
		}
		if input.ExpectPlan != "" && input.ExpectPlan != planID {
			for i := range report.Results {
				report.Results[i].Message = "plan_id does not match the approved plan; nothing was written"
			}
			report.Complete = false
			return ErrFleetRejected
		}
		if input.DryRun {
			report.Complete = false
			return nil
		}
		ignoreTerminalHangup()
		report.Phase = FleetPhaseExecute
		complete := true
		for i := range targets {
			result := FleetResult{Name: targets[i].name, Action: targets[i].action, Outcome: FleetOutcomeDone, Restart: FleetRestartNone, Message: targets[i].message}
			if targets[i].action == FleetActionAdopt {
				if _, err := backupProfile(cfg, targets[i].name, targets[i].raw); err != nil {
					result.Outcome, result.Message = FleetOutcomeFailed, err.Error()
					report.Results[i] = result
					complete = false
					for j := i + 1; j < len(targets); j++ {
						report.Results[j] = FleetResult{Name: targets[j].name, Action: targets[j].action, Outcome: FleetOutcomeNotRun, Restart: FleetRestartNone, Message: "not run after an earlier failure"}
					}
					break
				}
				if err := writeFileAtomic(filepath.Dir(targets[i].path), filepath.Base(targets[i].path), targets[i].next, 0o640); err != nil {
					result.Outcome, result.Message = FleetOutcomeFailed, err.Error()
					report.Results[i] = result
					complete = false
					for j := i + 1; j < len(targets); j++ {
						report.Results[j] = FleetResult{Name: targets[j].name, Action: targets[j].action, Outcome: FleetOutcomeNotRun, Restart: FleetRestartNone, Message: "not run after an earlier failure"}
					}
					break
				}
			}
			report.Results[i] = result
		}
		report.Complete = complete
		if !complete {
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

// setManagedByLine changes exactly one top-level managed_by line. Multiple
// occurrences, quoted keys/values, flow mappings, or a non-top-level position
// are refused so the rewrite cannot silently alter other YAML.
func setManagedByLine(raw []byte, owner string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("profile root is not a YAML mapping")
	}
	root := doc.Content[0]
	if root.Style&yaml.FlowStyle != 0 {
		return nil, errors.New("profile uses a flow mapping; adopt cannot rewrite it safely")
	}
	var key, value *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "managed_by" {
			if key != nil {
				return nil, errors.New("profile has more than one top-level managed_by key")
			}
			key, value = root.Content[i], root.Content[i+1]
		}
	}
	lines := bytes.Split(raw, []byte("\n"))
	var matched []int
	for i, line := range lines {
		if bytes.HasPrefix(line, []byte("managed_by:")) {
			matched = append(matched, i)
		}
	}
	if key != nil {
		if len(matched) != 1 || matched[0] != key.Line-1 || key.Column != 1 || key.Style != 0 {
			return nil, errors.New("managed_by is written in an unsupported form")
		}
		if value.Kind != yaml.ScalarNode || value.Style != 0 {
			return nil, errors.New("managed_by value uses quotes or a non-scalar form")
		}
		lines[matched[0]] = []byte("managed_by: " + owner)
		return bytes.Join(lines, []byte("\n")), nil
	}
	if len(matched) > 0 {
		return nil, errors.New("managed_by appears without a valid top-level key")
	}
	if bytes.Contains(raw, []byte("managed_by:")) || bytes.Contains(raw, []byte(`"managed_by"`)) {
		return nil, errors.New("managed_by appears nested or quoted; adopt cannot rewrite it safely")
	}
	next := append([]byte{}, raw...)
	trimmed := bytes.TrimRight(append([]byte{}, raw...), "\r\n")
	lastLine := trimmed
	if idx := bytes.LastIndexByte(lastLine, '\n'); idx >= 0 {
		lastLine = lastLine[idx+1:]
	}
	if string(bytes.TrimSpace(lastLine)) == "..." {
		return nil, errors.New("profile ends with a YAML document terminator")
	}
	if len(next) > 0 && !bytes.HasSuffix(next, []byte("\n")) {
		next = append(next, '\n')
	}
	next = append(next, []byte("managed_by: "+owner+"\n")...)
	return next, nil
}
