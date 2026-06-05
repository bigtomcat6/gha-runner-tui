package loop

import (
	"context"
	"testing"

	"gha-runner-tui/internal/config"
)

type fakeRegistrationTokenClient struct {
	repoToken string
	orgToken  string
	repoCalls []string
	orgCalls  []string
}

func (f *fakeRegistrationTokenClient) CreateRegistrationToken(_ context.Context, owner, repo string) (string, error) {
	f.repoCalls = append(f.repoCalls, owner+"/"+repo)
	return f.repoToken, nil
}

func (f *fakeRegistrationTokenClient) CreateOrgRegistrationToken(_ context.Context, org string) (string, error) {
	f.orgCalls = append(f.orgCalls, org)
	return f.orgToken, nil
}

func TestRunnerEnvForRepositoryProfile(t *testing.T) {
	t.Parallel()

	profile := config.Profile{
		Repo: config.RepoConfig{Owner: "bigtomcat6", Name: "remind-me"},
		Runner: config.RunnerConfig{
			Labels:    []string{"self-hosted", "linux"},
			Workdir:   "/tmp/actions-runner",
			Ephemeral: true,
		},
		Docker: config.DockerProfile{Env: map[string]string{"RUNNER_ALLOW_RUNASROOT": "1"}},
	}

	env := runnerEnv(profile, "remind-me-1", "token")
	if env["RUNNER_REPO_URL"] != "https://github.com/bigtomcat6/remind-me" {
		t.Fatalf("unexpected repo url: %q", env["RUNNER_REPO_URL"])
	}
	if env["REPO_URL"] != "https://github.com/bigtomcat6/remind-me" {
		t.Fatalf("unexpected legacy repo url: %q", env["REPO_URL"])
	}
	if env["RUNNER_TOKEN"] != "token" {
		t.Fatalf("unexpected runner token: %q", env["RUNNER_TOKEN"])
	}
	if env["REG_TOKEN"] != "token" {
		t.Fatalf("unexpected legacy registration token: %q", env["REG_TOKEN"])
	}
	if _, ok := env["RUNNER_GROUP"]; ok {
		t.Fatalf("did not expect RUNNER_GROUP, got %q", env["RUNNER_GROUP"])
	}
}

func TestRunnerEnvForOrganizationProfile(t *testing.T) {
	t.Parallel()

	profile := config.Profile{
		Target: config.TargetConfig{Scope: config.TargetScopeOrganization, Org: "Example Org"},
		RunnerGroup: config.RunnerGroupConfig{
			Name:       "example-org-swift",
			Create:     true,
			Visibility: "all",
		},
		Runner: config.RunnerConfig{
			Environment: "swift",
			Labels:      []string{"self-hosted", "linux", "swift"},
			Workdir:     "/tmp/actions-runner",
			Ephemeral:   true,
		},
		Docker: config.DockerProfile{Env: map[string]string{"RUNNER_ALLOW_RUNASROOT": "1"}},
	}

	env := runnerEnv(profile, "example-org-swift-1", "token")
	if env["RUNNER_REPO_URL"] != "https://github.com/example-org" {
		t.Fatalf("unexpected org url: %q", env["RUNNER_REPO_URL"])
	}
	if env["REPO_URL"] != "https://github.com/example-org" {
		t.Fatalf("unexpected legacy org url: %q", env["REPO_URL"])
	}
	if env["REG_TOKEN"] != "token" {
		t.Fatalf("unexpected legacy registration token: %q", env["REG_TOKEN"])
	}
	if env["RUNNER_GROUP"] != "example-org-swift" {
		t.Fatalf("unexpected runner group: %q", env["RUNNER_GROUP"])
	}
}

func TestRegistrationTokenForRepositoryProfile(t *testing.T) {
	t.Parallel()

	client := &fakeRegistrationTokenClient{repoToken: "repo-token"}
	profile := config.Profile{
		Repo: config.RepoConfig{Owner: "bigtomcat6", Name: "remind-me"},
	}

	token, err := registrationTokenForProfile(context.Background(), client, profile)
	if err != nil {
		t.Fatalf("registrationTokenForProfile returned error: %v", err)
	}
	if token != "repo-token" {
		t.Fatalf("expected repo-token, got %q", token)
	}
	if len(client.repoCalls) != 1 || client.repoCalls[0] != "bigtomcat6/remind-me" {
		t.Fatalf("expected repo call, got %v", client.repoCalls)
	}
	if len(client.orgCalls) != 0 {
		t.Fatalf("did not expect org calls, got %v", client.orgCalls)
	}
}

func TestRegistrationTokenForOrganizationProfile(t *testing.T) {
	t.Parallel()

	client := &fakeRegistrationTokenClient{orgToken: "org-token"}
	profile := config.Profile{
		Target: config.TargetConfig{Scope: config.TargetScopeOrganization, Org: "Example Org"},
		Runner: config.RunnerConfig{Environment: "swift"},
	}

	token, err := registrationTokenForProfile(context.Background(), client, profile)
	if err != nil {
		t.Fatalf("registrationTokenForProfile returned error: %v", err)
	}
	if token != "org-token" {
		t.Fatalf("expected org-token, got %q", token)
	}
	if len(client.orgCalls) != 1 || client.orgCalls[0] != "example-org" {
		t.Fatalf("expected org call, got %v", client.orgCalls)
	}
	if len(client.repoCalls) != 0 {
		t.Fatalf("did not expect repo calls, got %v", client.repoCalls)
	}
}
