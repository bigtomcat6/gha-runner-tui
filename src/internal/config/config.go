package config

import (
	"errors"
	"os"

	"gopkg.in/yaml.v3"
)

const (
	defaultProfilesDir = "/etc/gha-runner-tui/profiles"
	defaultStateDir    = "/var/lib/gha-runner-tui/state"
	defaultLogDir      = "/var/log/gha-runner-tui"
	defaultTokenEnv    = "GITHUB_TOKEN"
	defaultEnvFile     = "/etc/gha-runner-tui/github.env"
	defaultAPIBaseURL  = "https://api.github.com"
	defaultSvcPrefix   = "gha-"
)

type GlobalConfig struct {
	GitHub  GitHubConfig  `yaml:"github"`
	Paths   PathsConfig   `yaml:"paths"`
	Systemd SystemdConfig `yaml:"systemd"`
	Docker  DockerConfig  `yaml:"docker"`
}

type GitHubConfig struct {
	TokenEnv   string `yaml:"token_env"`
	EnvFile    string `yaml:"env_file"`
	APIBaseURL string `yaml:"api_base_url"`
}

type PathsConfig struct {
	ProfilesDir string `yaml:"profiles_dir"`
	StateDir    string `yaml:"state_dir"`
	LogDir      string `yaml:"log_dir"`
}

type SystemdConfig struct {
	ServicePrefix string `yaml:"service_prefix"`
}

type DockerConfig struct {
	UseCLI bool `yaml:"use_cli"`
}

func DefaultGlobalConfig() GlobalConfig {
	return GlobalConfig{
		GitHub: GitHubConfig{
			TokenEnv:   defaultTokenEnv,
			EnvFile:    defaultEnvFile,
			APIBaseURL: defaultAPIBaseURL,
		},
		Paths: PathsConfig{
			ProfilesDir: defaultProfilesDir,
			StateDir:    defaultStateDir,
			LogDir:      defaultLogDir,
		},
		Systemd: SystemdConfig{
			ServicePrefix: defaultSvcPrefix,
		},
		Docker: DockerConfig{
			UseCLI: true,
		},
	}
}

func LoadGlobalConfig(path string) (GlobalConfig, error) {
	cfg := DefaultGlobalConfig()
	if path == "" {
		return cfg, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return GlobalConfig{}, err
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return GlobalConfig{}, err
	}

	cfg.applyDefaults()
	return cfg, nil
}

func (c *GlobalConfig) applyDefaults() {
	if c.GitHub.TokenEnv == "" {
		c.GitHub.TokenEnv = defaultTokenEnv
	}
	if c.GitHub.EnvFile == "" {
		c.GitHub.EnvFile = defaultEnvFile
	}
	if c.GitHub.APIBaseURL == "" {
		c.GitHub.APIBaseURL = defaultAPIBaseURL
	}
	if c.Paths.ProfilesDir == "" {
		c.Paths.ProfilesDir = defaultProfilesDir
	}
	if c.Paths.StateDir == "" {
		c.Paths.StateDir = defaultStateDir
	}
	if c.Paths.LogDir == "" {
		c.Paths.LogDir = defaultLogDir
	}
	if c.Systemd.ServicePrefix == "" {
		c.Systemd.ServicePrefix = defaultSvcPrefix
	}
	if !c.Docker.UseCLI {
		c.Docker.UseCLI = true
	}
}
