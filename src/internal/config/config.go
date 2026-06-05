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
	defaultLoopBinary  = "/usr/local/bin/gha-ephemeral-loop"
	defaultHostSocket  = "/var/run/docker.sock"
)

type DockerAccessMode string

const (
	DockerAccessModeRootless   DockerAccessMode = "rootless"
	DockerAccessModeHostSocket DockerAccessMode = "host-socket"
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
	ServicePrefix  string `yaml:"service_prefix"`
	LoopBinaryPath string `yaml:"loop_binary_path"`
}

type DockerConfig struct {
	UseCLI                   bool             `yaml:"use_cli"`
	DefaultAccessMode        DockerAccessMode `yaml:"default_access_mode"`
	RootlessSocketPath       string           `yaml:"rootless_socket_path"`
	AutoDetectRootlessSocket bool             `yaml:"auto_detect_rootless_socket"`
	AllowHostSocketOptIn     bool             `yaml:"allow_host_socket_opt_in"`
	HostSocketPath           string           `yaml:"host_socket_path"`
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
			ServicePrefix:  defaultSvcPrefix,
			LoopBinaryPath: defaultLoopBinary,
		},
		Docker: DockerConfig{
			UseCLI:                   true,
			DefaultAccessMode:        DockerAccessModeRootless,
			AutoDetectRootlessSocket: true,
			AllowHostSocketOptIn:     true,
			HostSocketPath:           defaultHostSocket,
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
	if c.Systemd.LoopBinaryPath == "" {
		c.Systemd.LoopBinaryPath = defaultLoopBinary
	}
	if !c.Docker.UseCLI {
		c.Docker.UseCLI = true
	}
	if c.Docker.DefaultAccessMode == "" {
		c.Docker.DefaultAccessMode = DockerAccessModeRootless
	}
	if c.Docker.HostSocketPath == "" {
		c.Docker.HostSocketPath = defaultHostSocket
	}
}
