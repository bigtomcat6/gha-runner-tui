package config

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const legacyServiceDir = "/etc/systemd/system"

func DiscoverLegacyProfiles(serviceDir string) ([]Profile, []ProfileLoadError, error) {
	if serviceDir == "" {
		serviceDir = legacyServiceDir
	}

	entries, err := os.ReadDir(serviceDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}

	unitFiles := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "gha-") || !strings.HasSuffix(name, ".service") {
			continue
		}
		unitFiles = append(unitFiles, filepath.Join(serviceDir, name))
	}
	sort.Strings(unitFiles)

	profiles := make([]Profile, 0, len(unitFiles))
	profileErrors := make([]ProfileLoadError, 0)
	for _, unitPath := range unitFiles {
		profile, err := loadLegacyProfile(unitPath)
		if err != nil {
			profileErrors = append(profileErrors, ProfileLoadError{Path: unitPath, Err: err})
			continue
		}
		profiles = append(profiles, profile)
	}
	return profiles, profileErrors, nil
}

func loadLegacyProfile(unitPath string) (Profile, error) {
	data, err := os.ReadFile(unitPath)
	if err != nil {
		return Profile{}, err
	}

	envPath := ""
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "EnvironmentFile=") {
			continue
		}
		envPath = strings.TrimSpace(strings.TrimPrefix(line, "EnvironmentFile="))
		break
	}
	if envPath == "" {
		return Profile{}, errors.New("legacy unit missing EnvironmentFile")
	}

	values, err := parseEnvFile(envPath)
	if err != nil {
		return Profile{}, err
	}

	profileName := strings.TrimSuffix(filepath.Base(envPath), filepath.Ext(envPath))
	repoName := values["REPO_NAME"]
	containerPrefix := ""
	if repoName != "" {
		containerPrefix = "gha-" + repoName + "-"
	}

	profile := Profile{
		Name: profileName,
		Repo: RepoConfig{
			Owner: values["REPO_OWNER"],
			Name:  repoName,
		},
		Service: ServiceConfig{
			Name: filepath.Base(unitPath),
		},
		Runner: RunnerConfig{
			Ephemeral:  true,
			NamePrefix: values["RUNNER_NAME"],
			Labels:     splitAndTrim(values["RUNNER_LABELS"]),
		},
		Docker: DockerProfile{
			Image:               values["IMAGE"],
			ContainerNamePrefix: containerPrefix,
		},
		Source: unitPath,
	}
	if err := profile.Validate(); err != nil {
		return Profile{}, err
	}
	return profile, nil
}

func parseEnvFile(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		values[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

func splitAndTrim(value string) []string {
	parts := strings.Split(value, ",")
	items := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			items = append(items, part)
		}
	}
	return items
}
