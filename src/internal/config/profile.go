package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type Profile struct {
	Name        string            `yaml:"name"`
	Target      TargetConfig      `yaml:"target,omitempty"`
	Repo        RepoConfig        `yaml:"repo,omitempty"`
	Service     ServiceConfig     `yaml:"service"`
	RunnerGroup RunnerGroupConfig `yaml:"runner_group,omitempty"`
	Runner      RunnerConfig      `yaml:"runner"`
	Docker      DockerProfile     `yaml:"docker"`
	Loop        LoopConfig        `yaml:"loop"`
	Source      string            `yaml:"-"`
}

type RepoConfig struct {
	Owner string `yaml:"owner"`
	Name  string `yaml:"name"`
}

type TargetScope string

const (
	TargetScopeRepository   TargetScope = "repository"
	TargetScopeOrganization TargetScope = "organization"
)

const (
	RunnerGroupVisibilityAll     = "all"
	RunnerGroupVisibilityPrivate = "private"
)

type TargetConfig struct {
	Scope TargetScope `yaml:"scope"`
	Owner string      `yaml:"owner,omitempty"`
	Repo  string      `yaml:"repo,omitempty"`
	Org   string      `yaml:"org,omitempty"`
}

type RunnerGroupConfig struct {
	Name       string `yaml:"name"`
	Create     bool   `yaml:"create"`
	Visibility string `yaml:"visibility"`
}

type ResolvedTarget struct {
	Scope   TargetScope
	Owner   string
	Repo    string
	Org     string
	OrgSlug string
}

type DerivedOrganizationNames struct {
	ProfileName         string
	ServiceName         string
	ContainerNamePrefix string
	RunnerGroupName     string
	RunnerNamePrefix    string
	StateFile           string
	LogDir              string
}

type ServiceConfig struct {
	Name string `yaml:"name"`
}

type RunnerConfig struct {
	Ephemeral   bool     `yaml:"ephemeral"`
	Environment string   `yaml:"environment,omitempty"`
	NamePrefix  string   `yaml:"name_prefix"`
	Workdir     string   `yaml:"workdir"`
	Labels      []string `yaml:"labels"`
}

type DockerProfile struct {
	Image               string            `yaml:"image"`
	ContainerNamePrefix string            `yaml:"container_name_prefix"`
	CPUs                string            `yaml:"cpus"`
	Memory              string            `yaml:"memory"`
	RemoveAfterExit     bool              `yaml:"remove_after_exit"`
	Volumes             []string          `yaml:"volumes"`
	Env                 map[string]string `yaml:"env"`
}

type LoopConfig struct {
	IntervalSeconds   int    `yaml:"interval_seconds"`
	BackoffSeconds    int    `yaml:"backoff_seconds"`
	MaxBackoffSeconds int    `yaml:"max_backoff_seconds"`
	StateFile         string `yaml:"state_file"`
	LogDir            string `yaml:"log_dir"`
}

type ProfileLoadError struct {
	Path string
	Err  error
}

func (e ProfileLoadError) Error() string {
	return fmt.Sprintf("%s: %v", e.Path, e.Err)
}

func LoadProfiles(dir string) ([]Profile, []ProfileLoadError, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}

	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".yaml") && !strings.HasSuffix(entry.Name(), ".yml") {
			continue
		}
		files = append(files, filepath.Join(dir, entry.Name()))
	}
	sort.Strings(files)

	profiles := make([]Profile, 0, len(files))
	profileErrors := make([]ProfileLoadError, 0)
	for _, path := range files {
		profile, err := LoadProfile(path)
		if err != nil {
			profileErrors = append(profileErrors, ProfileLoadError{Path: path, Err: err})
			continue
		}
		profiles = append(profiles, profile)
	}

	return profiles, profileErrors, nil
}

func LoadProfile(path string) (Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Profile{}, err
	}

	var profile Profile
	if err := yaml.Unmarshal(data, &profile); err != nil {
		return Profile{}, err
	}
	profile.Source = path

	if err := profile.Validate(); err != nil {
		return Profile{}, err
	}

	return profile, nil
}

func (p Profile) Validate() error {
	target, err := p.ResolveTarget()
	if err != nil {
		return err
	}

	switch {
	case p.Name == "":
		return errors.New("name is required")
	case p.Service.Name == "":
		return errors.New("service.name is required")
	case p.Docker.ContainerNamePrefix == "":
		return errors.New("docker.container_name_prefix is required")
	}

	if target.Scope == TargetScopeOrganization {
		switch {
		case p.Runner.Environment == "":
			return errors.New("runner.environment is required for organization profiles")
		case p.RunnerGroup.Name == "":
			return errors.New("runner_group.name is required for organization profiles")
		case p.RunnerGroup.Visibility != "" && p.RunnerGroup.Visibility != RunnerGroupVisibilityAll && p.RunnerGroup.Visibility != RunnerGroupVisibilityPrivate:
			return errors.New("runner_group.visibility must be all or private")
		}
	}

	return nil
}

func (p Profile) ResolveTarget() (ResolvedTarget, error) {
	scope := p.Target.Scope
	if scope == "" {
		scope = TargetScopeRepository
	}

	switch scope {
	case TargetScopeRepository:
		owner := firstNonEmpty(p.Target.Owner, p.Repo.Owner)
		repo := firstNonEmpty(p.Target.Repo, p.Repo.Name)
		switch {
		case owner == "":
			return ResolvedTarget{}, errors.New("repo.owner is required")
		case repo == "":
			return ResolvedTarget{}, errors.New("repo.name is required")
		}
		return ResolvedTarget{
			Scope: TargetScopeRepository,
			Owner: owner,
			Repo:  repo,
		}, nil
	case TargetScopeOrganization:
		slug, err := NormalizeOrgSlug(p.Target.Org)
		if err != nil {
			return ResolvedTarget{}, err
		}
		return ResolvedTarget{
			Scope:   TargetScopeOrganization,
			Org:     strings.TrimSpace(p.Target.Org),
			OrgSlug: slug,
		}, nil
	default:
		return ResolvedTarget{}, fmt.Errorf("unsupported target.scope %q", scope)
	}
}

func (t ResolvedTarget) GitHubURL() string {
	if t.Scope == TargetScopeOrganization {
		return "https://github.com/" + t.OrgSlug
	}
	return "https://github.com/" + t.Owner + "/" + t.Repo
}

func NormalizeOrgSlug(value string) (string, error) {
	slug := normalizeNameComponent(value)
	if slug == "" {
		return "", errors.New("target.org normalizes to empty slug")
	}
	return slug, nil
}

func (p Profile) OrganizationRunnerGroupVisibility() string {
	if strings.TrimSpace(p.RunnerGroup.Visibility) != "" {
		return p.RunnerGroup.Visibility
	}
	return RunnerGroupVisibilityPrivate
}

func DeriveOrganizationEnvironmentNames(org, environment, stateDir, logDir string) (DerivedOrganizationNames, error) {
	orgSlug, err := NormalizeOrgSlug(org)
	if err != nil {
		return DerivedOrganizationNames{}, err
	}
	envSlug := normalizeNameComponent(environment)
	if envSlug == "" {
		return DerivedOrganizationNames{}, errors.New("runner.environment normalizes to empty slug")
	}

	name := orgSlug + "-" + envSlug
	return DerivedOrganizationNames{
		ProfileName:         name,
		ServiceName:         "gha-" + name + ".service",
		ContainerNamePrefix: "gha-" + name,
		RunnerGroupName:     name,
		RunnerNamePrefix:    name,
		StateFile:           filepath.Join(stateDir, name+".json"),
		LogDir:              filepath.Join(logDir, name),
	}, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func normalizeNameComponent(value string) string {
	parts := strings.Fields(strings.ToLower(strings.TrimSpace(value)))
	return strings.Join(parts, "-")
}
