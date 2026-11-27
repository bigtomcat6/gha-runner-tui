package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gha-runner-tui/internal/config"

	"gopkg.in/yaml.v3"
)

const maxBundleBytes = 1 << 20

func (m RunnerManager) Export(ctx context.Context, input ExportInput) (ExportReport, error) {
	_ = ctx
	cfg, err := config.LoadGlobalConfig(m.ConfigPath)
	if err != nil {
		return ExportReport{}, err
	}
	owner := input.Owner
	if owner == "" {
		owner = "all"
	}
	switch owner {
	case "all", config.ManagedByLocal, config.ManagedByFleet:
	default:
		return ExportReport{}, fmt.Errorf("%w: owner must be local, fleet, or all", ErrFleetInput)
	}
	inventory, err := readInventory(cfg.Paths.ProfilesDir)
	if err != nil {
		return ExportReport{}, err
	}
	report := ExportReport{Profiles: []ExportEntry{}, Errors: append([]ExportError{}, inventory.Bad...)}
	wanted := map[string]bool{}
	for _, name := range input.Names {
		if !safeManagedName(name) {
			return ExportReport{}, fmt.Errorf("%w: unsafe profile name %q", ErrFleetInput, name)
		}
		wanted[name] = true
	}
	var names []string
	for name := range inventory.ByName {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry := inventory.ByName[name]
		if len(wanted) > 0 && !wanted[name] {
			continue
		}
		if owner != "all" && entry.Profile.EffectiveManagedBy() != owner {
			continue
		}
		report.Profiles = append(report.Profiles, ExportEntry{Name: entry.Name, Path: entry.Path, Owner: entry.Profile.EffectiveManagedBy(), YAML: string(entry.Raw)})
	}
	for name := range wanted {
		if _, ok := inventory.ByName[name]; !ok {
			report.Errors = append(report.Errors, ExportError{Path: name, Error: "profile not found"})
		}
	}
	sort.Slice(report.Errors, func(i, j int) bool { return report.Errors[i].Path < report.Errors[j].Path })
	report.Complete = len(report.Errors) == 0
	return report, nil
}

func splitBundle(bundle []byte) ([][]byte, error) {
	if len(bundle) > maxBundleBytes {
		return nil, fmt.Errorf("%w: bundle exceeds 1 MiB", ErrFleetInput)
	}
	text := string(bundle)
	if text == "" {
		return nil, fmt.Errorf("%w: bundle is empty", ErrFleetInput)
	}
	var docs [][]byte
	var current []byte
	flush := func() error {
		if len(bytes.TrimSpace(current)) == 0 {
			current = nil
			return nil
		}
		doc := append([]byte{}, current...)
		if !bytes.HasSuffix(doc, []byte("\n")) {
			return fmt.Errorf("%w: every bundle document must end with a newline", ErrFleetInput)
		}
		docs = append(docs, doc)
		current = nil
		return nil
	}
	lines := strings.SplitAfter(text, "\n")
	for _, line := range lines {
		content := strings.TrimSuffix(line, "\n")
		content = strings.TrimSuffix(content, "\r")
		if content == "---" {
			if err := flush(); err != nil {
				return nil, err
			}
			continue
		}
		current = append(current, line...)
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("%w: bundle has no documents", ErrFleetInput)
	}
	return docs, nil
}

type bundleDocument struct {
	Name    string
	Raw     []byte
	Profile config.Profile
	SHA     string
}

func parseBundle(cfg config.GlobalConfig, bundle []byte) ([]bundleDocument, error) {
	documents, err := splitBundle(bundle)
	if err != nil {
		return nil, err
	}
	var result []bundleDocument
	seen := map[string]bool{}
	for _, doc := range documents {
		var profile config.Profile
		if err := yaml.Unmarshal(doc, &profile); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrFleetInput, err)
		}
		if err := profile.Validate(); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrFleetInput, err)
		}
		if seen[profile.Name] {
			return nil, fmt.Errorf("%w: duplicate profile name %q", ErrFleetInput, profile.Name)
		}
		seen[profile.Name] = true
		profilePath, err := confineJoin(cfg.Paths.ProfilesDir, profile.Name+".yaml")
		if err != nil {
			return nil, fmt.Errorf("%w: profile %s: %v", ErrFleetInput, profile.Name, err)
		}
		if _, err := confineManagedPath(cfg.Paths.StateDir, profile.Loop.StateFile); err != nil {
			return nil, fmt.Errorf("%w: profile %s: %v", ErrFleetInput, profile.Name, err)
		}
		if _, err := confineManagedPath(cfg.Paths.LogDir, profile.Loop.LogDir); err != nil {
			return nil, fmt.Errorf("%w: profile %s: %v", ErrFleetInput, profile.Name, err)
		}
		profile.Source = profilePath
		result = append(result, bundleDocument{Name: profile.Name, Raw: doc, Profile: profile, SHA: shaOfBytes(doc)})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func safeManagedName(name string) bool {
	if name == "" || name != strings.TrimSpace(name) {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-':
			if i == 0 || i == len(name)-1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func MatchProfileNames(dir, pattern string) ([]string, error) {
	return matchNames(dir, pattern)
}

func matchNames(dir, pattern string) ([]string, error) {
	if pattern == "" {
		return nil, fmt.Errorf("%w: match pattern is required", ErrFleetInput)
	}
	if _, err := filepath.Match(pattern, ""); err != nil {
		return nil, fmt.Errorf("%w: invalid match pattern: %v", ErrFleetInput, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		var name string
		switch {
		case strings.HasSuffix(entry.Name(), ".yaml"):
			name = strings.TrimSuffix(entry.Name(), ".yaml")
		case strings.HasSuffix(entry.Name(), ".yml"):
			name = strings.TrimSuffix(entry.Name(), ".yml")
		default:
			continue
		}
		ok, err := filepath.Match(pattern, name)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid match pattern: %v", ErrFleetInput, err)
		}
		if ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}
