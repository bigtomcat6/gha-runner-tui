package config

import "testing"

func TestResolveTargetKeepsLegacyRepositoryProfile(t *testing.T) {
	t.Parallel()

	profile := Profile{
		Name:    "remind-me-swift",
		Repo:    RepoConfig{Owner: "bigtomcat6", Name: "remind-me"},
		Service: ServiceConfig{Name: "gha-remind-me-swift.service"},
		Docker:  DockerProfile{ContainerNamePrefix: "gha-remind-me-swift"},
	}

	target, err := profile.ResolveTarget()
	if err != nil {
		t.Fatalf("ResolveTarget returned error: %v", err)
	}
	if target.Scope != TargetScopeRepository {
		t.Fatalf("expected repository target, got %q", target.Scope)
	}
	if target.Owner != "bigtomcat6" || target.Repo != "remind-me" {
		t.Fatalf("unexpected target: %+v", target)
	}
	if target.GitHubURL() != "https://github.com/bigtomcat6/remind-me" {
		t.Fatalf("unexpected url: %q", target.GitHubURL())
	}
}

func TestResolveTargetSupportsOrganizationProfile(t *testing.T) {
	t.Parallel()

	profile := Profile{
		Name: "example-org-swift",
		Target: TargetConfig{
			Scope: TargetScopeOrganization,
			Org:   "Example Org",
		},
		RunnerGroup: RunnerGroupConfig{
			Name:       "example-org-swift",
			Create:     true,
			Visibility: RunnerGroupVisibilityPrivate,
		},
		Runner:  RunnerConfig{Environment: "swift"},
		Service: ServiceConfig{Name: "gha-example-org-swift.service"},
		Docker:  DockerProfile{ContainerNamePrefix: "gha-example-org-swift"},
	}

	target, err := profile.ResolveTarget()
	if err != nil {
		t.Fatalf("ResolveTarget returned error: %v", err)
	}
	if target.Scope != TargetScopeOrganization {
		t.Fatalf("expected organization target, got %q", target.Scope)
	}
	if target.OrgSlug != "example-org" {
		t.Fatalf("expected normalized org slug, got %q", target.OrgSlug)
	}
	if target.GitHubURL() != "https://github.com/example-org" {
		t.Fatalf("unexpected url: %q", target.GitHubURL())
	}
}

func TestNormalizeOrgSlug(t *testing.T) {
	t.Parallel()

	got, err := NormalizeOrgSlug("  Example   Org  ")
	if err != nil {
		t.Fatalf("NormalizeOrgSlug returned error: %v", err)
	}
	if got != "example-org" {
		t.Fatalf("expected example-org, got %q", got)
	}
}

func TestDeriveOrganizationEnvironmentNames(t *testing.T) {
	t.Parallel()

	names, err := DeriveOrganizationEnvironmentNames("Example Org", "swift", "/state", "/logs")
	if err != nil {
		t.Fatalf("DeriveOrganizationEnvironmentNames returned error: %v", err)
	}
	if names.ProfileName != "example-org-swift" {
		t.Fatalf("unexpected profile name: %q", names.ProfileName)
	}
	if names.ServiceName != "gha-example-org-swift.service" {
		t.Fatalf("unexpected service name: %q", names.ServiceName)
	}
	if names.ContainerNamePrefix != "gha-example-org-swift" {
		t.Fatalf("unexpected container prefix: %q", names.ContainerNamePrefix)
	}
	if names.RunnerGroupName != "example-org-swift" {
		t.Fatalf("unexpected runner group: %q", names.RunnerGroupName)
	}
	if names.StateFile != "/state/example-org-swift.json" {
		t.Fatalf("unexpected state file: %q", names.StateFile)
	}
	if names.LogDir != "/logs/example-org-swift" {
		t.Fatalf("unexpected log dir: %q", names.LogDir)
	}
}

func TestOrganizationRunnerGroupVisibilityDefaultsToPrivate(t *testing.T) {
	t.Parallel()

	profile := Profile{}
	if got := profile.OrganizationRunnerGroupVisibility(); got != RunnerGroupVisibilityPrivate {
		t.Fatalf("expected private default, got %q", got)
	}
}
