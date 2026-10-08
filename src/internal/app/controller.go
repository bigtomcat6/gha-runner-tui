package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
var ErrProfileNotFound = errors.New("profile not found")
var ErrInvalidCreateInput = errors.New("invalid create input")

func (m RunnerManager) LookupProfile(name string) (config.Profile, error) {
	cfg, err := config.LoadGlobalConfig(m.ConfigPath)
	if err != nil {
		return config.Profile{}, err
	}
	service := m.Service
	if m.SystemdUnitDir != "" {
		service.LegacyServiceDir = m.SystemdUnitDir
	}
	profiles, profileErrors := service.loadProfiles(cfg)
	for _, profile := range profiles {
		if profile.Name == name {
			return profile, nil
		}
	}
	errs := []error{fmt.Errorf("%w: %q", ErrProfileNotFound, name)}
	for _, profileErr := range profileErrors {
		errs = append(errs, profileErr)
	}
	return config.Profile{}, errors.Join(errs...)
}

func (m RunnerManager) Migrate(ctx context.Context) ([]config.ProfileMigrationResult, error) {
	cfg, err := config.LoadGlobalConfig(m.ConfigPath)
	if err != nil {
		return nil, err
	}
	results, accessErr := config.MigrateProfilesAccessMode(cfg.Paths.ProfilesDir)
	githubResults, githubErr := config.MigrateProfilesGitHubConfig(cfg.Paths.ProfilesDir, config.GitHubProfile{TokenEnv: cfg.GitHub.TokenEnv, EnvFile: cfg.GitHub.EnvFile})
	results = append(results, githubResults...)
	errs := []error{accessErr, githubErr}
	for _, result := range results {
		if result.Status == config.ProfileMigrationFailed {
			errs = append(errs, fmt.Errorf("%s: %s", result.Path, result.Message))
		}
	}
	return results, errors.Join(errs...)
}

func (m RunnerManager) ProfileSlotHolders(ctx context.Context, p config.Profile) ([]dockerpkg.ContainerInfo, error) {
	if p.Name == "" {
		return nil, errors.New("profile name is required")
	}
	containers, err := m.Docker.ListManaged(ctx, p.Name)
	if err != nil {
		return nil, err
	}
	holders := make([]dockerpkg.ContainerInfo, 0, len(containers))
	for _, container := range containers {
		if container.Profile == p.Name && dockerpkg.OccupiesSlot(container) {
			holders = append(holders, container)
		}
	}
	return holders, nil
}

func (m RunnerManager) ForceRemoveProfile(ctx context.Context, p config.Profile) ([]string, error) {
	holders, err := m.ProfileSlotHolders(ctx, p)
	if err != nil {
		return nil, err
	}
	removed := make([]string, 0, len(holders))
	for _, container := range holders {
		target := container.ID
		if target == "" {
			target = container.Name
		}
		if target == "" {
			return removed, errors.New("slot holder has no container ID or name")
		}
		if err := m.Docker.Remove(ctx, target, true); err != nil {
			return removed, err
		}
		removed = append(removed, target)
	}
	return removed, nil
}

type GitHubAdminClient interface {
	ListOrgRunnerGroups(ctx context.Context, org string) ([]gh.RunnerGroup, error)
	ListOrgRunnerGroupRunners(ctx context.Context, org string, id int64) ([]gh.Runner, error)
	CreateOrgRunnerGroup(ctx context.Context, org, name, visibility string) (gh.RunnerGroup, error)
	UpdateOrgRunnerGroup(ctx context.Context, org string, id int64, name, visibility string) (gh.RunnerGroup, error)
	DeleteOrgRunnerGroup(ctx context.Context, org string, id int64) error
	ListOrgRunners(ctx context.Context, org string) ([]gh.Runner, error)
}

type RunnerManager struct {
	ConfigPath     string
	SystemdUnitDir string
	Runner         command.Runner
	Service        Service
	Systemd        systemdpkg.Client
	Docker         dockerpkg.Client
	GitHub         gh.Client
	GitHubAdmin    GitHubAdminClient
	openNewFile    func(string, int, os.FileMode) (*os.File, error)
}

type CreateProfileInput struct {
	Scope               config.TargetScope
	Org                 string
	Environment         string
	DockerAccess        string
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
	GitHubEnvFile       string
	WatchRepositories   []string
	NoStart             bool
}

func normalizeCreateInput(cfg config.GlobalConfig, input CreateProfileInput) (CreateProfileInput, error) {
	if input.GitHubEnvFile == "" {
		input.GitHubEnvFile = cfg.GitHub.EnvFile
	}
	for field, value := range map[string]string{
		"scope": string(input.Scope), "name": input.Name, "repo.owner": input.RepoOwner, "repo.name": input.RepoName,
		"org": input.Org, "environment": input.Environment, "docker_access": input.DockerAccess,
		"docker.image": input.DockerImage, "service.name": input.ServiceName, "docker.container_name_prefix": input.ContainerNamePrefix,
		"cpus": input.CPUs, "memory": input.Memory, "github.env_file": input.GitHubEnvFile,
	} {
		if strings.ContainsAny(value, "\r\n\x00") {
			return input, fmt.Errorf("%w: %s must not contain CR, LF, or NUL", ErrInvalidCreateInput, field)
		}
	}
	for _, values := range [][]string{input.RunnerLabels, input.WatchRepositories} {
		for _, value := range values {
			if strings.ContainsAny(value, "\r\n\x00") {
				return input, fmt.Errorf("%w: labels/watch repositories must not contain CR, LF, or NUL", ErrInvalidCreateInput)
			}
		}
	}
	for field, value := range map[string]string{"docker.image": input.DockerImage, "cpus": input.CPUs, "memory": input.Memory} {
		if strings.TrimSpace(value) == "" {
			return input, fmt.Errorf("%w: %s is required", ErrInvalidCreateInput, field)
		}
	}
	if len(input.RunnerLabels) == 0 {
		return input, fmt.Errorf("%w: runner labels are required", ErrInvalidCreateInput)
	}
	for _, label := range input.RunnerLabels {
		if strings.TrimSpace(label) == "" {
			return input, fmt.Errorf("%w: runner labels must not be empty", ErrInvalidCreateInput)
		}
	}
	switch strings.TrimSpace(input.DockerAccess) {
	case "", "default", string(config.DockerAccessModeRootless), string(config.DockerAccessModeHostSocket):
	default:
		return input, fmt.Errorf("%w: unsupported docker access mode %q", ErrInvalidCreateInput, input.DockerAccess)
	}
	if input.Scope == "" {
		input.Scope = config.TargetScopeRepository
	}
	var groupName string
	switch input.Scope {
	case config.TargetScopeRepository:
		input.RepoOwner = strings.TrimSpace(input.RepoOwner)
		input.RepoName = strings.TrimSpace(input.RepoName)
	case config.TargetScopeOrganization:
		names, err := config.DeriveOrganizationEnvironmentNames(input.Org, input.Environment, cfg.Paths.StateDir, cfg.Paths.LogDir)
		if err != nil {
			return input, fmt.Errorf("%w: %v", ErrInvalidCreateInput, err)
		}
		groupName = names.RunnerGroupName
		if input.Name == "" {
			input.Name = names.ProfileName
		}
	default:
		return input, fmt.Errorf("%w: unsupported target.scope %q", ErrInvalidCreateInput, input.Scope)
	}
	if input.ServiceName == "" {
		input.ServiceName = "gha-" + input.Name + ".service"
	}
	if input.ContainerNamePrefix == "" {
		input.ContainerNamePrefix = "gha-" + input.Name
	}
	// Reuse the existing target and managed-name rules without adding loop-only validation.
	profile := config.Profile{
		Name: input.Name, Target: config.TargetConfig{Scope: input.Scope, Org: input.Org},
		Repo:        config.RepoConfig{Owner: input.RepoOwner, Name: input.RepoName},
		Service:     config.ServiceConfig{Name: input.ServiceName},
		Docker:      config.DockerProfile{ContainerNamePrefix: input.ContainerNamePrefix},
		Runner:      config.RunnerConfig{Environment: input.Environment},
		RunnerGroup: config.RunnerGroupConfig{Name: groupName},
	}
	if err := profile.Validate(); err != nil {
		return input, fmt.Errorf("%w: %v", ErrInvalidCreateInput, err)
	}
	input.Ephemeral = true
	return input, nil
}

type resolvedDockerAccess struct {
	Mode    config.DockerAccessMode
	Volumes []string
	Env     map[string]string
}

func NewRunnerManager(configPath string, systemd systemdpkg.Client, docker dockerpkg.Client, github gh.Client) RunnerManager {
	service := Service{
		ConfigPath: configPath,
		Systemd:    systemd,
		Docker:     docker,
		GitHubForProfile: func(cfg config.GlobalConfig, p config.Profile) GitHubClient {
			return github.ForProfile(cfg.GitHub, p.GitHub)
		},
	}
	return RunnerManager{
		ConfigPath:     configPath,
		SystemdUnitDir: "/etc/systemd/system",
		Runner:         nil,
		Service:        service,
		Systemd:        systemd,
		Docker:         docker,
		GitHub:         github,
		GitHubAdmin:    github,
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
	if _, err := config.MigrateProfileAccessMode(profilePath); err != nil {
		return err
	}
	cfg, err := config.LoadGlobalConfig(m.ConfigPath)
	if err != nil {
		return err
	}
	if _, err := config.MigrateProfileGitHubConfig(profilePath, config.GitHubProfile{
		TokenEnv: cfg.GitHub.TokenEnv,
		EnvFile:  cfg.GitHub.EnvFile,
	}); err != nil {
		return err
	}
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

	if _, err := config.MigrateProfilesAccessMode(cfg.Paths.ProfilesDir); err != nil {
		return err
	}
	if _, err := config.MigrateProfilesGitHubConfig(cfg.Paths.ProfilesDir, config.GitHubProfile{
		TokenEnv: cfg.GitHub.TokenEnv,
		EnvFile:  cfg.GitHub.EnvFile,
	}); err != nil {
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
	return m.Systemd.Stop(ctx, snapshot.Profile.Service.Name)
}

func (m RunnerManager) RestartLoop(ctx context.Context, snapshot ProfileSnapshot) error {
	return m.Systemd.Restart(ctx, snapshot.Profile.Service.Name)
}

func (m RunnerManager) SystemdLogs(ctx context.Context, profile config.Profile, tail int, follow bool) (string, error) {
	return m.Systemd.Logs(ctx, profile.Service.Name, tail, follow)
}

func (m RunnerManager) DockerLogs(ctx context.Context, snapshot ProfileSnapshot, tail int, follow bool) (string, error) {
	container, err := m.Service.matchContainer(ctx, snapshot.Profile, snapshot.Loop)
	if err != nil {
		return "", err
	}
	target := container.Name
	if target == "" {
		target = container.ID
	}
	if target == "" {
		return "", ErrNoCurrentContainer
	}
	return m.Docker.Logs(ctx, target, tail, follow)
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

func (m RunnerManager) CreateProfile(ctx context.Context, input CreateProfileInput) (err error) {
	var created []string
	defer func() {
		if err == nil {
			return
		}
		if len(created) > 0 {
			err = fmt.Errorf("created files retained (%s); resolve these files explicitly before retrying: %w", strings.Join(created, ", "), err)
		}
		if errors.Is(err, os.ErrPermission) {
			err = fmt.Errorf("%w; permission denied; run create with sudo (sudo gha-runner-tui create ...)", err)
		}
	}()
	cfg, err := config.LoadGlobalConfig(m.ConfigPath)
	if err != nil {
		return err
	}
	input, err = normalizeCreateInput(cfg, input)
	if err != nil {
		return err
	}

	dockerAccess, err := resolveDockerAccessForCreate(cfg, input.DockerAccess)
	if err != nil {
		return err
	}

	profile := config.Profile{
		Name: input.Name,
		Repo: config.RepoConfig{
			Owner: input.RepoOwner,
			Name:  input.RepoName,
		},
		GitHub: config.GitHubProfile{
			TokenEnv: cfg.GitHub.TokenEnv,
			EnvFile:  input.GitHubEnvFile,
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
			AccessMode:          dockerAccess.Mode,
			Image:               input.DockerImage,
			ContainerNamePrefix: input.ContainerNamePrefix,
			CPUs:                input.CPUs,
			Memory:              input.Memory,
			RemoveAfterExit:     true,
			Volumes:             dockerAccess.Volumes,
			Env:                 dockerAccess.Env,
		},
		Loop: config.LoopConfig{
			IntervalSeconds:     5,
			BackoffSeconds:      30,
			MaxBackoffSeconds:   300,
			PollIntervalSeconds: 30,
			IdleTimeoutSeconds:  180,
			StateFile:           filepath.Join(cfg.Paths.StateDir, input.Name+".json"),
			LogDir:              filepath.Join(cfg.Paths.LogDir, input.Name),
		},
	}
	profile.Runner.WatchRepositories = input.WatchRepositories
	if input.Scope == config.TargetScopeOrganization {
		names, err := config.DeriveOrganizationEnvironmentNames(input.Org, input.Environment, cfg.Paths.StateDir, cfg.Paths.LogDir)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidCreateInput, err)
		}
		profile.Repo = config.RepoConfig{}
		profile.Target = config.TargetConfig{Scope: config.TargetScopeOrganization, Org: input.Org}
		profile.Runner.Environment = input.Environment
		profile.RunnerGroup = config.RunnerGroupConfig{Name: names.RunnerGroupName, Create: true, Visibility: config.RunnerGroupVisibilityPrivate}
	}

	if err := profile.Validate(); err != nil {
		return err
	}

	stateFile, err := confineManagedPath(cfg.Paths.StateDir, profile.Loop.StateFile)
	if err != nil {
		return err
	}
	logDir, err := confineManagedPath(cfg.Paths.LogDir, profile.Loop.LogDir)
	if err != nil {
		return err
	}
	profilePath, err := confineJoin(cfg.Paths.ProfilesDir, profile.Name+".yaml")
	if err != nil {
		return err
	}
	servicePath, err := confineJoin(m.SystemdUnitDir, profile.Service.Name)
	if err != nil {
		return err
	}
	profile.Loop.StateFile = stateFile
	profile.Loop.LogDir = logDir

	profileData, err := renderProfileYAML(profile)
	if err != nil {
		return err
	}
	serviceData, err := renderServiceFile(serviceTemplateData{
		ProfileName:       profile.Name,
		GitHubEnvFile:     profile.GitHub.EnvFile,
		LoopBinaryPath:    cfg.Systemd.LoopBinaryPath,
		ProfileConfigPath: profilePath,
	})
	if err != nil {
		return err
	}
	for _, path := range []string{profilePath, servicePath} {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("refusing to create existing file %q: %w", path, os.ErrExist)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("check new file %q: %w", path, err)
		}
	}
	for _, dir := range []string{cfg.Paths.ProfilesDir, cfg.Paths.StateDir, cfg.Paths.LogDir, m.SystemdUnitDir} {
		if err := m.ensureDir(ctx, dir); err != nil {
			return fmt.Errorf("ensure directory %q: %w", dir, err)
		}
	}
	if err := m.writeNewManagedFile(ctx, profilePath, profileData, 0o640); err != nil {
		return fmt.Errorf("YAML creation: %w", err)
	}
	created = append(created, profilePath)
	if err := m.writeNewManagedFile(ctx, servicePath, serviceData, 0o644); err != nil {
		return fmt.Errorf("unit creation: %w", err)
	}
	created = append(created, servicePath)

	if err := m.Systemd.DaemonReload(ctx); err != nil {
		return err
	}
	if err := m.Systemd.Enable(ctx, profile.Service.Name); err != nil {
		return err
	}
	if input.NoStart {
		return nil
	}
	return m.Systemd.Start(ctx, profile.Service.Name)
}

func resolveDockerAccessForCreate(cfg config.GlobalConfig, requested string) (resolvedDockerAccess, error) {
	mode := strings.TrimSpace(requested)
	if mode == "" || mode == "default" {
		mode = string(cfg.Docker.DefaultAccessMode)
	}

	containerSocketPath := "/var/run/docker.sock"
	switch config.DockerAccessMode(mode) {
	case config.DockerAccessModeRootless:
		socketPath := strings.TrimSpace(cfg.Docker.RootlessSocketPath)
		if socketPath == "" && cfg.Docker.AutoDetectRootlessSocket {
			var err error
			socketPath, err = detectRootlessSocket()
			if err != nil {
				return resolvedDockerAccess{}, fmt.Errorf("rootless Docker is required for this profile, but no rootless socket is configured or detectable; set docker.rootless_socket_path in config.yaml or choose host-socket explicitly: %w", err)
			}
		}
		if socketPath == "" {
			return resolvedDockerAccess{}, errors.New("rootless Docker is required for this profile, but no rootless socket is configured or detectable; set docker.rootless_socket_path in config.yaml or choose host-socket explicitly")
		}
		if !pathExists(socketPath) {
			return resolvedDockerAccess{}, fmt.Errorf("configured rootless Docker socket does not exist: %s", socketPath)
		}
		return resolvedDockerAccess{
			Mode:    config.DockerAccessModeRootless,
			Volumes: []string{socketPath + ":" + containerSocketPath},
			Env: map[string]string{
				"DOCKER_HOST":            "unix://" + containerSocketPath,
				"RUNNER_ALLOW_RUNASROOT": "1",
			},
		}, nil
	case config.DockerAccessModeHostSocket:
		if !cfg.Docker.AllowHostSocketOptIn {
			return resolvedDockerAccess{}, errors.New("host-socket Docker access is disabled by configuration")
		}
		hostSocketPath := strings.TrimSpace(cfg.Docker.HostSocketPath)
		if hostSocketPath == "" {
			hostSocketPath = "/var/run/docker.sock"
		}
		return resolvedDockerAccess{
			Mode:    config.DockerAccessModeHostSocket,
			Volumes: []string{hostSocketPath + ":" + containerSocketPath},
			Env: map[string]string{
				"DOCKER_HOST":            "unix://" + containerSocketPath,
				"RUNNER_ALLOW_RUNASROOT": "1",
			},
		}, nil
	default:
		return resolvedDockerAccess{}, fmt.Errorf("unsupported docker access mode %q", requested)
	}
}

func detectRootlessSocket() (string, error) {
	candidates := make([]string, 0, 2)
	if dockerHost := strings.TrimSpace(os.Getenv("DOCKER_HOST")); strings.HasPrefix(dockerHost, "unix://") {
		socketPath := strings.TrimPrefix(dockerHost, "unix://")
		if pathExists(socketPath) {
			candidates = append(candidates, socketPath)
		}
	}

	paths, err := filepath.Glob("/run/user/*/docker.sock")
	if err != nil {
		return "", err
	}
	for _, path := range paths {
		if pathExists(path) && !containsString(candidates, path) {
			candidates = append(candidates, path)
		}
	}

	switch len(candidates) {
	case 1:
		return candidates[0], nil
	case 0:
		return "", errors.New("no usable rootless Docker socket found")
	default:
		return "", fmt.Errorf("multiple rootless Docker sockets detected: %s", strings.Join(candidates, ", "))
	}
}

func pathExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func confineJoin(root, leaf string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("managed root is required")
	}
	if strings.Contains(leaf, string(os.PathSeparator)) {
		return "", fmt.Errorf("managed leaf %q must be a basename", leaf)
	}
	return confineManagedPath(root, filepath.Join(root, leaf))
}

func confineManagedPath(root, target string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("managed root is required")
	}
	cleanRoot := filepath.Clean(root)
	cleanTarget := filepath.Clean(target)
	rel, err := filepath.Rel(cleanRoot, cleanTarget)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("managed path escapes root %q: %s", cleanRoot, cleanTarget)
	}
	return cleanTarget, nil
}

type serviceTemplateData struct {
	ProfileName       string
	GitHubEnvFile     string
	LoopBinaryPath    string
	ProfileConfigPath string
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

func (m RunnerManager) writeNewManagedFile(_ context.Context, path string, data []byte, perm os.FileMode) error {
	open := m.openNewFile
	if open == nil {
		open = os.OpenFile
	}
	f, err := open(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("create %q: %w", path, err)
	}
	err = f.Chmod(perm)
	if err == nil {
		var n int
		n, err = f.Write(data)
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
	}
	closeErr := f.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return fmt.Errorf("created %q; write/permissions/close failed; file retained: %w", path, err)
	}
	return nil
}

func FormatCleanupResult(removed []string) string {
	if len(removed) == 0 {
		return "no exited containers matched this profile"
	}
	return fmt.Sprintf("removed %d exited container(s)", len(removed))
}
