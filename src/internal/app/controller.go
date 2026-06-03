package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"

	"gha-runner-tui/internal/command"
	"gha-runner-tui/internal/config"
	dockerpkg "gha-runner-tui/internal/docker"
	gh "gha-runner-tui/internal/github"
	"gha-runner-tui/internal/state"
	systemdpkg "gha-runner-tui/internal/systemd"
	tmplpkg "gha-runner-tui/templates"
)

var ErrNoCurrentContainer = errors.New("no current container exists for this profile")

type GitHubAdminClient interface {
	ListOrgRunnerGroups(ctx context.Context, org string) ([]gh.RunnerGroup, error)
	ListOrgRunnerGroupRunners(ctx context.Context, org string, id int64) ([]gh.Runner, error)
	CreateOrgRunnerGroup(ctx context.Context, org, name, visibility string) (gh.RunnerGroup, error)
	UpdateOrgRunnerGroup(ctx context.Context, org string, id int64, name, visibility string) (gh.RunnerGroup, error)
	DeleteOrgRunnerGroup(ctx context.Context, org string, id int64) error
	ListOrgRunners(ctx context.Context, org string) ([]gh.Runner, error)
}

type RunnerManager struct {
	ConfigPath       string
	SystemdUnitDir   string
	LegacyEnvDir     string
	LegacyTokenFile  string
	LegacyLoopBinary string
	Runner           command.Runner
	Service          Service
	Systemd          systemdpkg.Client
	Docker           dockerpkg.Client
	GitHub           gh.Client
	GitHubAdmin      GitHubAdminClient
}

type CreateProfileInput struct {
	Scope               config.TargetScope
	Org                 string
	Environment         string
	Name                string
	RepoOwner           string
	RepoName            string
	RunnerLabels        []string
	DockerImage         string
	ServiceName         string
	ContainerNamePrefix string
	CPUs                string
	Memory              string
	Ephemeral           bool
}

func NewRunnerManager(configPath string, systemd systemdpkg.Client, docker dockerpkg.Client, github gh.Client) RunnerManager {
	service := Service{
		ConfigPath: configPath,
		Systemd:    systemd,
		Docker:     docker,
		GitHub:     github,
	}
	return RunnerManager{
		ConfigPath:       configPath,
		SystemdUnitDir:   "/etc/systemd/system",
		LegacyEnvDir:     "/etc/gha-runner",
		LegacyTokenFile:  "/etc/gha-runner/github_pat",
		LegacyLoopBinary: "/usr/local/bin/gha-ephemeral-loop",
		Runner:           nil,
		Service:          service,
		Systemd:          systemd,
		Docker:           docker,
		GitHub:           github,
		GitHubAdmin:      github,
	}
}

func (m RunnerManager) Dashboard(ctx context.Context) (Dashboard, error) {
	return m.Service.LoadDashboard(ctx)
}

func (m RunnerManager) SyncRunnerGroup(ctx context.Context, profile config.Profile) error {
	if m.GitHubAdmin == nil {
		return errors.New("github admin client is not configured")
	}

	target, err := profile.ResolveTarget()
	if err != nil {
		return err
	}
	if target.Scope != config.TargetScopeOrganization {
		return nil
	}
	desiredVisibility := profile.OrganizationRunnerGroupVisibility()

	groups, err := m.GitHubAdmin.ListOrgRunnerGroups(ctx, target.OrgSlug)
	if err != nil {
		return err
	}

	for _, group := range groups {
		if group.Name != profile.RunnerGroup.Name {
			continue
		}
		if group.Visibility == desiredVisibility && !group.AllowsPublicRepositories {
			return nil
		}
		_, err := m.GitHubAdmin.UpdateOrgRunnerGroup(ctx, target.OrgSlug, group.ID, group.Name, desiredVisibility)
		return err
	}

	if !profile.RunnerGroup.Create {
		return fmt.Errorf("runner group %q does not exist", profile.RunnerGroup.Name)
	}

	_, err = m.GitHubAdmin.CreateOrgRunnerGroup(ctx, target.OrgSlug, profile.RunnerGroup.Name, desiredVisibility)
	return err
}

func (m RunnerManager) SyncProfilePath(ctx context.Context, profilePath string) error {
	profile, err := config.LoadProfile(profilePath)
	if err != nil {
		return err
	}
	return m.SyncRunnerGroup(ctx, profile)
}

func (m RunnerManager) SyncConfigProfiles(ctx context.Context) error {
	cfg, err := config.LoadGlobalConfig(m.ConfigPath)
	if err != nil {
		return err
	}

	profiles, profileErrors, err := config.LoadProfiles(cfg.Paths.ProfilesDir)
	if err != nil {
		return err
	}
	if len(profileErrors) > 0 {
		return profileErrors[0]
	}
	for _, profile := range profiles {
		if err := m.SyncRunnerGroup(ctx, profile); err != nil {
			return err
		}
	}
	return nil
}

func (m RunnerManager) DeleteRunnerGroup(ctx context.Context, profile config.Profile) error {
	if m.GitHubAdmin == nil {
		return errors.New("github admin client is not configured")
	}

	target, err := profile.ResolveTarget()
	if err != nil {
		return err
	}
	if target.Scope != config.TargetScopeOrganization {
		return errors.New("runner group deletion only supports organization profiles")
	}

	groups, err := m.GitHubAdmin.ListOrgRunnerGroups(ctx, target.OrgSlug)
	if err != nil {
		return err
	}

	var targetGroup *gh.RunnerGroup
	for i := range groups {
		if groups[i].Name == profile.RunnerGroup.Name {
			targetGroup = &groups[i]
			break
		}
	}
	if targetGroup == nil {
		return fmt.Errorf("runner group %q does not exist", profile.RunnerGroup.Name)
	}

	runners, err := m.GitHubAdmin.ListOrgRunnerGroupRunners(ctx, target.OrgSlug, targetGroup.ID)
	if err != nil {
		return err
	}
	for _, runner := range runners {
		if runner.Busy || runner.Status == state.GitHubOnline {
			return fmt.Errorf("runner group %q still has active runner %q", targetGroup.Name, runner.Name)
		}
	}

	return m.GitHubAdmin.DeleteOrgRunnerGroup(ctx, target.OrgSlug, targetGroup.ID)
}

func (m RunnerManager) StartLoop(ctx context.Context, profile config.Profile) error {
	return m.Systemd.Start(ctx, profile.Service.Name)
}

func (m RunnerManager) StopLoop(ctx context.Context, snapshot ProfileSnapshot) error {
	if err := m.stopContainerIfRunning(ctx, snapshot); err != nil {
		return err
	}
	return m.Systemd.Stop(ctx, snapshot.Profile.Service.Name)
}

func (m RunnerManager) RestartLoop(ctx context.Context, snapshot ProfileSnapshot) error {
	if err := m.stopContainerIfRunning(ctx, snapshot); err != nil {
		return err
	}
	return m.Systemd.Restart(ctx, snapshot.Profile.Service.Name)
}

func (m RunnerManager) SystemdLogs(ctx context.Context, profile config.Profile, tail int, follow bool) (string, error) {
	return m.Systemd.Logs(ctx, profile.Service.Name, tail, follow)
}

func (m RunnerManager) DockerLogs(ctx context.Context, snapshot ProfileSnapshot, tail int, follow bool) (string, error) {
	container := snapshot.Container.Name
	if container == "" {
		container = snapshot.Loop.LastContainerName
	}
	if container == "" {
		return "", ErrNoCurrentContainer
	}
	return m.Docker.Logs(ctx, container, tail, follow)
}

func (m RunnerManager) KillContainer(ctx context.Context, snapshot ProfileSnapshot) error {
	containers, err := m.runningContainersForSnapshot(ctx, snapshot)
	if err != nil {
		return err
	}
	if len(containers) > 0 {
		return m.killContainers(ctx, containers)
	}

	container := snapshot.Container.ID
	if container == "" {
		container = snapshot.Container.Name
	}
	if container == "" {
		container = snapshot.Loop.LastContainerName
	}
	if container == "" {
		return ErrNoCurrentContainer
	}
	return m.Docker.Kill(ctx, container)
}

func (m RunnerManager) CleanupExited(ctx context.Context, profile config.Profile) ([]string, error) {
	return m.Docker.CleanupExited(ctx, profile.Docker.ContainerNamePrefix)
}

func (m RunnerManager) stopContainerIfRunning(ctx context.Context, snapshot ProfileSnapshot) error {
	containers, err := m.runningContainersForSnapshot(ctx, snapshot)
	if err != nil {
		return err
	}
	if len(containers) > 0 {
		return m.killContainers(ctx, containers)
	}

	if snapshot.Container.State != state.ContainerRunning {
		return nil
	}
	container := snapshot.Container.ID
	if container == "" {
		container = snapshot.Container.Name
	}
	if container == "" {
		return nil
	}
	return m.Docker.Kill(ctx, container)
}

func (m RunnerManager) runningContainersForSnapshot(ctx context.Context, snapshot ProfileSnapshot) ([]dockerpkg.ContainerInfo, error) {
	_, matches, err := m.Service.matchContainers(ctx, snapshot.Profile, snapshot.Loop)
	if err != nil {
		return nil, err
	}

	containers := make([]dockerpkg.ContainerInfo, 0, len(matches))
	seen := map[string]struct{}{}
	for _, container := range matches {
		if container.State != state.ContainerRunning {
			continue
		}
		key := container.ID
		if key == "" {
			key = container.Name
		}
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		containers = append(containers, container)
	}
	return containers, nil
}

func (m RunnerManager) killContainers(ctx context.Context, containers []dockerpkg.ContainerInfo) error {
	for _, container := range containers {
		target := container.ID
		if target == "" {
			target = container.Name
		}
		if target == "" {
			continue
		}
		if err := m.Docker.Kill(ctx, target); err != nil {
			return err
		}
	}
	return nil
}

func (m RunnerManager) CreateProfile(ctx context.Context, input CreateProfileInput) error {
	if input.Scope == config.TargetScopeOrganization {
		return m.createOrganizationProfile(ctx, input)
	}

	if m.shouldUseLegacyCreate() {
		return m.createLegacyProfile(ctx, input)
	}

	cfg, err := config.LoadGlobalConfig(m.ConfigPath)
	if err != nil {
		return err
	}

	profile := config.Profile{
		Name: input.Name,
		Repo: config.RepoConfig{
			Owner: input.RepoOwner,
			Name:  input.RepoName,
		},
		Service: config.ServiceConfig{
			Name: input.ServiceName,
		},
		Runner: config.RunnerConfig{
			Ephemeral:  input.Ephemeral,
			NamePrefix: input.Name,
			Workdir:    "/tmp/actions-runner",
			Labels:     input.RunnerLabels,
		},
		Docker: config.DockerProfile{
			Image:               input.DockerImage,
			ContainerNamePrefix: input.ContainerNamePrefix,
			CPUs:                input.CPUs,
			Memory:              input.Memory,
			RemoveAfterExit:     true,
			Volumes:             []string{"/var/run/docker.sock:/var/run/docker.sock"},
			Env: map[string]string{
				"RUNNER_ALLOW_RUNASROOT": "1",
			},
		},
		Loop: config.LoopConfig{
			IntervalSeconds:   5,
			BackoffSeconds:    30,
			MaxBackoffSeconds: 300,
			StateFile:         filepath.Join(cfg.Paths.StateDir, input.Name+".json"),
			LogDir:            filepath.Join(cfg.Paths.LogDir, input.Name),
		},
	}

	if err := profile.Validate(); err != nil {
		return err
	}

	if err := m.ensureDir(ctx, cfg.Paths.ProfilesDir); err != nil {
		return err
	}
	if err := m.ensureDir(ctx, cfg.Paths.StateDir); err != nil {
		return err
	}
	if err := m.ensureDir(ctx, cfg.Paths.LogDir); err != nil {
		return err
	}
	if err := m.ensureDir(ctx, m.SystemdUnitDir); err != nil {
		return err
	}

	profilePath := filepath.Join(cfg.Paths.ProfilesDir, profile.Name+".yaml")
	profileData, err := renderProfileYAML(profile)
	if err != nil {
		return err
	}
	if err := m.writeManagedFile(ctx, profilePath, profileData, 0o640); err != nil {
		return err
	}

	servicePath := filepath.Join(m.SystemdUnitDir, profile.Service.Name)
	serviceData, err := renderServiceFile(serviceTemplateData{
		ProfileName:       profile.Name,
		GitHubEnvFile:     cfg.GitHub.EnvFile,
		LoopBinaryPath:    cfg.Systemd.LoopBinaryPath,
		ProfileConfigPath: profilePath,
	})
	if err != nil {
		return err
	}
	if err := m.writeManagedFile(ctx, servicePath, serviceData, 0o644); err != nil {
		return err
	}

	if err := m.Systemd.DaemonReload(ctx); err != nil {
		return err
	}
	if err := m.Systemd.Enable(ctx, profile.Service.Name); err != nil {
		return err
	}
	return m.Systemd.Start(ctx, profile.Service.Name)
}

func (m RunnerManager) createOrganizationProfile(ctx context.Context, input CreateProfileInput) error {
	cfg, err := config.LoadGlobalConfig(m.ConfigPath)
	if err != nil {
		return err
	}

	names, err := config.DeriveOrganizationEnvironmentNames(input.Org, input.Environment, cfg.Paths.StateDir, cfg.Paths.LogDir)
	if err != nil {
		return err
	}

	profile := config.Profile{
		Name: names.ProfileName,
		Target: config.TargetConfig{
			Scope: config.TargetScopeOrganization,
			Org:   input.Org,
		},
		Service: config.ServiceConfig{
			Name: names.ServiceName,
		},
		RunnerGroup: config.RunnerGroupConfig{
			Name:       names.RunnerGroupName,
			Create:     true,
			Visibility: config.RunnerGroupVisibilityPrivate,
		},
		Runner: config.RunnerConfig{
			Ephemeral:   input.Ephemeral,
			Environment: input.Environment,
			NamePrefix:  names.RunnerNamePrefix,
			Workdir:     "/tmp/actions-runner",
			Labels:      input.RunnerLabels,
		},
		Docker: config.DockerProfile{
			Image:               input.DockerImage,
			ContainerNamePrefix: names.ContainerNamePrefix,
			CPUs:                input.CPUs,
			Memory:              input.Memory,
			RemoveAfterExit:     true,
			Volumes:             []string{"/var/run/docker.sock:/var/run/docker.sock"},
			Env: map[string]string{
				"RUNNER_ALLOW_RUNASROOT": "1",
			},
		},
		Loop: config.LoopConfig{
			IntervalSeconds:   5,
			BackoffSeconds:    30,
			MaxBackoffSeconds: 300,
			StateFile:         names.StateFile,
			LogDir:            names.LogDir,
		},
	}

	if err := profile.Validate(); err != nil {
		return err
	}

	if err := m.ensureDir(ctx, cfg.Paths.ProfilesDir); err != nil {
		return err
	}
	if err := m.ensureDir(ctx, cfg.Paths.StateDir); err != nil {
		return err
	}
	if err := m.ensureDir(ctx, cfg.Paths.LogDir); err != nil {
		return err
	}
	if err := m.ensureDir(ctx, m.SystemdUnitDir); err != nil {
		return err
	}

	profilePath := filepath.Join(cfg.Paths.ProfilesDir, profile.Name+".yaml")
	profileData, err := renderProfileYAML(profile)
	if err != nil {
		return err
	}
	if err := m.writeManagedFile(ctx, profilePath, profileData, 0o640); err != nil {
		return err
	}

	servicePath := filepath.Join(m.SystemdUnitDir, profile.Service.Name)
	serviceData, err := renderServiceFile(serviceTemplateData{
		ProfileName:       profile.Name,
		GitHubEnvFile:     cfg.GitHub.EnvFile,
		LoopBinaryPath:    cfg.Systemd.LoopBinaryPath,
		ProfileConfigPath: profilePath,
	})
	if err != nil {
		return err
	}
	if err := m.writeManagedFile(ctx, servicePath, serviceData, 0o644); err != nil {
		return err
	}

	if err := m.Systemd.DaemonReload(ctx); err != nil {
		return err
	}
	if err := m.Systemd.Enable(ctx, profile.Service.Name); err != nil {
		return err
	}
	return m.Systemd.Start(ctx, profile.Service.Name)
}

func (m RunnerManager) shouldUseLegacyCreate() bool {
	if m.LegacyEnvDir == "" || m.LegacyLoopBinary == "" {
		return false
	}
	_, err := os.Stat(m.LegacyTokenFile)
	return err == nil
}

func (m RunnerManager) createLegacyProfile(ctx context.Context, input CreateProfileInput) error {
	if err := m.ensureDir(ctx, m.LegacyEnvDir); err != nil {
		return err
	}
	if err := m.ensureDir(ctx, m.SystemdUnitDir); err != nil {
		return err
	}

	envPath := filepath.Join(m.LegacyEnvDir, input.Name+".env")
	envData := renderLegacyEnvFile(input)
	if err := m.writeManagedFile(ctx, envPath, envData, 0o644); err != nil {
		return err
	}

	servicePath := filepath.Join(m.SystemdUnitDir, input.ServiceName)
	serviceData, err := renderLegacyServiceFile(legacyServiceTemplateData{
		ProfileName:     input.Name,
		EnvironmentFile: envPath,
		LoopBinaryPath:  m.LegacyLoopBinary,
	})
	if err != nil {
		return err
	}
	if err := m.writeManagedFile(ctx, servicePath, serviceData, 0o644); err != nil {
		return err
	}

	if err := m.Systemd.DaemonReload(ctx); err != nil {
		return err
	}
	if err := m.Systemd.Enable(ctx, input.ServiceName); err != nil {
		return err
	}
	return m.Systemd.Start(ctx, input.ServiceName)
}

type serviceTemplateData struct {
	ProfileName       string
	GitHubEnvFile     string
	LoopBinaryPath    string
	ProfileConfigPath string
}

type legacyServiceTemplateData struct {
	ProfileName     string
	EnvironmentFile string
	LoopBinaryPath  string
}

func renderProfileYAML(profile config.Profile) ([]byte, error) {
	return yaml.Marshal(profile)
}

func renderServiceFile(data serviceTemplateData) ([]byte, error) {
	raw, err := tmplpkg.Files.ReadFile("systemd.service.tmpl")
	if err != nil {
		return nil, err
	}

	tmpl, err := template.New("systemd-service").Parse(string(raw))
	if err != nil {
		return nil, err
	}

	buf := bytes.NewBuffer(nil)
	if err := tmpl.Execute(buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func renderLegacyServiceFile(data legacyServiceTemplateData) ([]byte, error) {
	raw, err := tmplpkg.Files.ReadFile("systemd.legacy.service.tmpl")
	if err != nil {
		return nil, err
	}

	tmpl, err := template.New("legacy-systemd-service").Parse(string(raw))
	if err != nil {
		return nil, err
	}

	buf := bytes.NewBuffer(nil)
	if err := tmpl.Execute(buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func renderLegacyEnvFile(input CreateProfileInput) []byte {
	lines := []string{
		"REPO_OWNER=" + input.RepoOwner,
		"REPO_NAME=" + input.RepoName,
		"RUNNER_NAME=" + strings.TrimSuffix(input.ServiceName, ".service"),
		"RUNNER_LABELS=" + strings.Join(input.RunnerLabels, ","),
		"IMAGE=" + input.DockerImage,
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

func (m RunnerManager) ensureDir(ctx context.Context, path string) error {
	if err := os.MkdirAll(path, 0o755); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrPermission) && !errors.Is(err, os.ErrNotExist) {
		// Fall through to privileged path only for access issues.
		if !os.IsPermission(err) {
			return err
		}
	}
	if m.Runner == nil {
		return os.MkdirAll(path, 0o755)
	}
	_, err := m.Runner.Run(ctx, "mkdir", "-p", path)
	return err
}

func (m RunnerManager) writeManagedFile(ctx context.Context, path string, data []byte, perm os.FileMode) error {
	if err := os.WriteFile(path, data, perm); err == nil {
		return nil
	} else if !os.IsPermission(err) {
		return err
	}
	if m.Runner == nil {
		return os.WriteFile(path, data, perm)
	}

	tempFile, err := os.CreateTemp("", "gha-runner-tui-*")
	if err != nil {
		return err
	}
	tempPath := tempFile.Name()
	defer os.Remove(tempPath)
	if _, err := tempFile.Write(data); err != nil {
		tempFile.Close()
		return err
	}
	if err := tempFile.Close(); err != nil {
		return err
	}

	mode := fmt.Sprintf("%03o", perm.Perm())
	_, err = m.Runner.Run(ctx, "install", "-m", mode, tempPath, path)
	return err
}

func FormatCleanupResult(removed []string) string {
	if len(removed) == 0 {
		return "no exited containers matched this profile"
	}
	return fmt.Sprintf("removed %d exited container(s)", len(removed))
}
