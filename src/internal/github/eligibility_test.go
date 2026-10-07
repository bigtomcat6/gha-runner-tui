package github

import (
	"context"
	"gha-runner-tui/internal/config"
	"testing"
)

func orgProfile() config.Profile {
	return config.Profile{Target: config.TargetConfig{Scope: config.TargetScopeOrganization, Org: "org"}, RunnerGroup: config.RunnerGroupConfig{Name: "group"}, GitHub: config.GitHubProfile{CredentialID: "test", ResourceOwner: "org"}, Scheduler: config.ProfileSchedulerConfig{Repositories: []config.RepoRef{testRepo}}}
}

const repoA = `{"id":1,"name":"a","owner":{"login":"org"},"visibility":"private","private":true}`

func TestScopeCompleteAccess(t *testing.T) {
	for _, tc := range []struct {
		name, group, repos string
		complete           bool
	}{
		{"selected", `{"id":4,"name":"group","visibility":"selected","allows_public_repositories":false,"restricted_to_workflows":false}`, repoA, true},
		{"selected uncovered", `{"id":4,"name":"group","visibility":"selected","allows_public_repositories":false,"restricted_to_workflows":false}`, repoA + `,{"id":2,"name":"b","owner":{"login":"org"},"visibility":"private","private":true}`, false},
		{"public unknown", `{"id":4,"name":"group","visibility":"selected","restricted_to_workflows":false}`, repoA, false},
		{"restricted unproved", `{"id":4,"name":"group","visibility":"selected","allows_public_repositories":false,"restricted_to_workflows":true,"selected_workflows":["org/a/.github/workflows/build.yml@main"]}`, repoA, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := []restStep{{key: "GET /orgs/org/actions/runner-groups?page=1&per_page=100", body: `{"runner_groups":[` + tc.group + `],"total_count":1}`}, {key: "GET /orgs/org/actions/runner-groups/4", body: tc.group}}
			if tc.name != "public unknown" && tc.name != "restricted unproved" {
				steps = append(steps, restStep{key: "GET /orgs/org/actions/runner-groups/4/repositories?page=1&per_page=100", body: `{"repositories":[` + tc.repos + `],"total_count":` + map[bool]string{true: "1", false: "2"}[tc.complete] + `}`})
			}
			if tc.complete {
				steps = append(steps, restStep{key: "GET /repos/org/a", body: repoA})
			}
			c := scopedFixture(t, recordedREST(t, steps), nil)
			proof, err := c.EffectiveEligible(context.Background(), orgProfile())
			if proof.Auth != c.Auth() || proof.Complete != tc.complete || (tc.complete && (err != nil || proof.Digest == "" || len(proof.Repositories) != 1)) || (!tc.complete && err == nil) {
				t.Fatalf("proof=%+v err=%v", proof, err)
			}
		})
	}
}

func TestScopeAllPrivateNewRepoAndPaging(t *testing.T) {
	for _, visibility := range []string{"all", "private"} {
		t.Run(visibility+" unqualified catalog", func(t *testing.T) {
			group := `{"id":4,"name":"group","visibility":"` + visibility + `","allows_public_repositories":false,"restricted_to_workflows":false}`
			steps := []restStep{
				{key: "GET /orgs/org/actions/runner-groups?page=1&per_page=100", body: `{"runner_groups":[],"total_count":1}`, link: `<https://example.test/orgs/org/actions/runner-groups?page=2&per_page=100>; rel="next"`},
				{key: "GET /orgs/org/actions/runner-groups?page=2&per_page=100", body: `{"runner_groups":[` + group + `],"total_count":1}`},
				{key: "GET /orgs/org/actions/runner-groups/4", body: group},
			}
			c := scopedFixture(t, recordedREST(t, steps), nil)
			proof, err := c.EffectiveEligible(context.Background(), orgProfile())
			if proof.Auth != c.Auth() || proof.Complete || err == nil {
				t.Fatalf("scope shrunk to mapped healthy repo: %+v %v", proof, err)
			}
		})
	}
}

func TestScopeFixedGenerationAndRestrictions(t *testing.T) {
	group := `{"id":4,"name":"group","visibility":"selected","allows_public_repositories":false,"restricted_to_workflows":true,"selected_workflows":["org/a/.github/workflows/build.yml@main"]}`
	steps := []restStep{{key: "GET /orgs/org/actions/runner-groups?page=1&per_page=100", body: `{"runner_groups":[` + group + `],"total_count":1}`}, {key: "GET /orgs/org/actions/runner-groups/4", body: group}, {key: "GET /orgs/org/actions/runner-groups/4/repositories?page=1&per_page=100", body: `{"repositories":[` + repoA + `],"total_count":1}`}, {key: "GET /repos/org/a", body: repoA}}
	p := orgProfile()
	p.Scheduler.Routes = []config.RouteProof{{WorkflowRef: "org/a/.github/workflows/build.yml@main", GroupID: 4, Labels: []string{"self-hosted"}, EvidenceDigest: "test-evidence"}}
	x := scopedFixture(t, recordedREST(t, append(append([]restStep{}, steps...), steps...)), nil)
	y := scopedFixture(t, recordedREST(t, steps), nil)
	a, err := x.EffectiveEligible(context.Background(), p)
	if err != nil || !a.Complete {
		t.Fatalf("%+v %v", a, err)
	}
	b, err := x.EffectiveEligible(context.Background(), p)
	if err != nil || b.Digest != a.Digest {
		t.Fatal("unstable proof")
	}
	d, err := y.EffectiveEligible(context.Background(), p)
	if err != nil || d.Digest == a.Digest || d.Auth == a.Auth {
		t.Fatal("proof crossed generation")
	}
}

func TestScopeRejectsChangedCredentialRef(t *testing.T) {
	c := scopedFixture(t, recordedREST(t, nil), nil)
	p := orgProfile()
	p.GitHub.TokenEnv = "OTHER_TEST_TOKEN"
	proof, err := c.EffectiveEligible(context.Background(), p)
	if err == nil || proof.Complete || proof.Auth != c.Auth() {
		t.Fatalf("profile change reused frozen auth %+v %v", proof, err)
	}
}

func TestScopeDetailCannotInheritMissingPolicy(t *testing.T) {
	group := `{"id":4,"name":"group","visibility":"selected","allows_public_repositories":false,"restricted_to_workflows":false}`
	c := scopedFixture(t, recordedREST(t, []restStep{{key: "GET /orgs/org/actions/runner-groups?page=1&per_page=100", body: `{"runner_groups":[` + group + `],"total_count":1}`}, {key: "GET /orgs/org/actions/runner-groups/4", body: `{"id":4,"name":"group","visibility":"selected"}`}}), nil)
	proof, err := c.EffectiveEligible(context.Background(), orgProfile())
	if err == nil || proof.Complete {
		t.Fatal("missing policy inherited from stale list")
	}
}
