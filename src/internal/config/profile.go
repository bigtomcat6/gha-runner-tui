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
	Name    string        `yaml:"name"`
	Repo    RepoConfig    `yaml:"repo"`
	Service ServiceConfig `yaml:"service"`
	Runner  RunnerConfig  `yaml:"runner"`
	Docker  DockerProfile `yaml:"docker"`
	Loop    LoopConfig    `yaml:"loop"`
	Source  string        `yaml:"-"`
}

type RepoConfig struct {
	Owner string `yaml:"owner"`
	Name  string `yaml:"name"`
}

type ServiceConfig struct {
	Name string `yaml:"name"`
}

type RunnerConfig struct {
	Ephemeral  bool     `yaml:"ephemeral"`
	NamePrefix string   `yaml:"name_prefix"`
	Workdir    string   `yaml:"workdir"`
	Labels     []string `yaml:"labels"`
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
	switch {
	case p.Name == "":
		return errors.New("name is required")
	case p.Repo.Owner == "":
		return errors.New("repo.owner is required")
	case p.Repo.Name == "":
		return errors.New("repo.name is required")
	case p.Service.Name == "":
		return errors.New("service.name is required")
	case p.Docker.ContainerNamePrefix == "":
		return errors.New("docker.container_name_prefix is required")
	}

	return nil
}
