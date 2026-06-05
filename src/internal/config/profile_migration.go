package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type ProfileMigrationStatus string

const (
	ProfileMigrationUpdated ProfileMigrationStatus = "updated"
	ProfileMigrationSkipped ProfileMigrationStatus = "skipped"
	ProfileMigrationFailed  ProfileMigrationStatus = "failed"
)

type ProfileMigrationResult struct {
	Path    string
	Status  ProfileMigrationStatus
	Message string
}

func MigrateProfilesAccessMode(dir string) ([]ProfileMigrationResult, error) {
	return migrateProfiles(dir, func(path string) (ProfileMigrationResult, error) {
		return MigrateProfileAccessMode(path)
	})
}

func MigrateProfilesGitHubConfig(dir string, defaults GitHubProfile) ([]ProfileMigrationResult, error) {
	return migrateProfiles(dir, func(path string) (ProfileMigrationResult, error) {
		return MigrateProfileGitHubConfig(path, defaults)
	})
}

func migrateProfiles(dir string, migrate func(string) (ProfileMigrationResult, error)) ([]ProfileMigrationResult, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
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

	results := make([]ProfileMigrationResult, 0, len(files))
	for _, path := range files {
		result, err := migrate(path)
		if err != nil {
			results = append(results, ProfileMigrationResult{
				Path:    path,
				Status:  ProfileMigrationFailed,
				Message: err.Error(),
			})
			continue
		}
		if result.Status != "" {
			results = append(results, result)
		}
	}
	return results, nil
}

func MigrateProfileAccessMode(path string) (ProfileMigrationResult, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ProfileMigrationResult{}, err
	}

	var profile Profile
	if err := yaml.Unmarshal(raw, &profile); err != nil {
		return ProfileMigrationResult{}, err
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return ProfileMigrationResult{}, err
	}
	if len(doc.Content) == 0 {
		return ProfileMigrationResult{
			Path:    path,
			Status:  ProfileMigrationSkipped,
			Message: "empty YAML document",
		}, nil
	}

	root := doc.Content[0]
	dockerNode := mappingValue(root, "docker")
	if dockerNode == nil || dockerNode.Kind != yaml.MappingNode {
		return ProfileMigrationResult{
			Path:    path,
			Status:  ProfileMigrationSkipped,
			Message: "docker config missing or invalid",
		}, nil
	}
	if mappingValue(dockerNode, "access_mode") != nil {
		return ProfileMigrationResult{
			Path:    path,
			Status:  ProfileMigrationSkipped,
			Message: "docker access_mode already present",
		}, nil
	}

	mode := profile.DockerAccessMode()
	if mode == "" {
		return ProfileMigrationResult{
			Path:    path,
			Status:  ProfileMigrationSkipped,
			Message: "docker access mode is ambiguous",
		}, nil
	}

	if err := ensureMigrationBackup(path, raw); err != nil {
		return ProfileMigrationResult{}, err
	}

	addMappingValue(dockerNode, "access_mode", string(mode))

	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(&doc); err != nil {
		return ProfileMigrationResult{}, err
	}
	if err := encoder.Close(); err != nil {
		return ProfileMigrationResult{}, err
	}

	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		return ProfileMigrationResult{}, err
	}

	return ProfileMigrationResult{
		Path:    path,
		Status:  ProfileMigrationUpdated,
		Message: fmt.Sprintf("set docker.access_mode=%s", mode),
	}, nil
}

func MigrateProfileGitHubConfig(path string, defaults GitHubProfile) (ProfileMigrationResult, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ProfileMigrationResult{}, err
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return ProfileMigrationResult{}, err
	}
	if len(doc.Content) == 0 {
		return ProfileMigrationResult{
			Path:    path,
			Status:  ProfileMigrationSkipped,
			Message: "empty YAML document",
		}, nil
	}
	if strings.TrimSpace(defaults.TokenEnv) == "" && strings.TrimSpace(defaults.EnvFile) == "" && strings.TrimSpace(defaults.TokenFile) == "" {
		return ProfileMigrationResult{
			Path:    path,
			Status:  ProfileMigrationSkipped,
			Message: "github defaults are empty",
		}, nil
	}

	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return ProfileMigrationResult{
			Path:    path,
			Status:  ProfileMigrationSkipped,
			Message: "top-level YAML must be a mapping",
		}, nil
	}

	githubNode := mappingValue(root, "github")
	if githubNode == nil {
		githubNode = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		addMappingNode(root, "github", githubNode)
	}
	if githubNode.Kind != yaml.MappingNode {
		return ProfileMigrationResult{
			Path:    path,
			Status:  ProfileMigrationSkipped,
			Message: "github config exists but is invalid",
		}, nil
	}

	updated := false
	if strings.TrimSpace(defaults.TokenEnv) != "" && mappingValue(githubNode, "token_env") == nil {
		addMappingValue(githubNode, "token_env", defaults.TokenEnv)
		updated = true
	}
	if strings.TrimSpace(defaults.EnvFile) != "" && mappingValue(githubNode, "env_file") == nil {
		addMappingValue(githubNode, "env_file", defaults.EnvFile)
		updated = true
	}
	if strings.TrimSpace(defaults.TokenFile) != "" && mappingValue(githubNode, "token_file") == nil {
		addMappingValue(githubNode, "token_file", defaults.TokenFile)
		updated = true
	}
	if !updated {
		return ProfileMigrationResult{
			Path:    path,
			Status:  ProfileMigrationSkipped,
			Message: "github config already present",
		}, nil
	}

	if err := ensureMigrationBackup(path, raw); err != nil {
		return ProfileMigrationResult{}, err
	}

	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(&doc); err != nil {
		return ProfileMigrationResult{}, err
	}
	if err := encoder.Close(); err != nil {
		return ProfileMigrationResult{}, err
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		return ProfileMigrationResult{}, err
	}

	return ProfileMigrationResult{
		Path:    path,
		Status:  ProfileMigrationUpdated,
		Message: "set explicit github credential config",
	}, nil
}

func ensureMigrationBackup(path string, content []byte) error {
	backupPath := path + ".bak"
	if _, err := os.Stat(backupPath); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.WriteFile(backupPath, content, 0o600)
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(node.Content)-1; i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func addMappingValue(node *yaml.Node, key, value string) {
	node.Content = append(node.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
	)
}

func addMappingNode(node *yaml.Node, key string, value *yaml.Node) {
	node.Content = append(node.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		value,
	)
}
