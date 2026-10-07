package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestReviewScopeAuthorityUnavailable(t *testing.T) {
	for _, visibility := range []string{"all", "private"} {
		t.Run(visibility, func(t *testing.T) {
			group := `{"id":4,"name":"group","visibility":"` + visibility + `","allows_public_repositories":false,"restricted_to_workflows":false}`
			// Both token-filtered catalogs see only A; eligible private B is hidden.
			steps := []restStep{{key: "GET /orgs/org/actions/runner-groups?page=1&per_page=100", body: `{"runner_groups":[` + group + `],"total_count":1}`}, {key: "GET /orgs/org/actions/runner-groups/4", body: group}}
			c := scopedFixture(t, recordedREST(t, steps), nil)
			proof, err := c.EffectiveEligible(context.Background(), orgProfile())
			if !errors.Is(err, ErrIncomplete) || !strings.Contains(fmt.Sprint(err), "scope authority unavailable") || proof.Complete || proof.Digest != "" || proof.Auth != c.Auth() {
				t.Fatalf("unqualified full scope certified: %+v %v", proof, err)
			}
		})
	}
}

func TestReviewInvalidatedCompletedCursor(t *testing.T) {
	now := time.Unix(0, 0)
	steps := basicSteps()
	steps[5].after = func() { now = now.Add(61 * time.Second) }
	c := scopedFixture(t, recordedREST(t, repoSteps(steps)), func() time.Time { return now })
	obs, err := c.ObserveRepo(context.Background(), testRepo)
	if err == nil || obs.Complete || len(c.observations.first) != 1 {
		t.Fatalf("missing budget reproduction: %+v %v", obs, err)
	}
	ref := c.transport.ref
	if !errors.Is(c.CheckCurrent(ref, CredentialIO{LookupEnv: func(string) (string, bool) { return "changed", true }}), ErrAuthChanged) {
		t.Fatal("not invalidated")
	}
	obs, err = c.ObserveRepo(context.Background(), testRepo)
	if !errors.Is(err, ErrAuthChanged) || obs.Complete || len(obs.Jobs) != 0 || len(c.observations.cursors) != 0 || len(c.observations.first) != 0 {
		t.Fatalf("invalidated cursor returned stale result: %+v %v", obs, err)
	}
}

func TestReviewRepositoryIdentity(t *testing.T) {
	for _, body := range []string{strings.Replace(repoA, `"id":1`, `"id":2`, 1), strings.Replace(repoA, `"name":"a"`, `"name":"b"`, 1), strings.Replace(repoA, `"login":"org"`, `"login":"other"`, 1)} {
		for _, refresh := range []bool{false, true} {
			c := scopedFixture(t, recordedREST(t, []restStep{{key: "GET /repos/org/a", body: body}}), nil)
			var err error
			if refresh {
				_, err = c.RefreshJob(context.Background(), JobRef{Repo: testRepo, RunID: 7, JobID: 9, Attempt: 2})
			} else {
				obs, e := c.ObserveRepo(context.Background(), testRepo)
				err = e
				if obs.Complete || len(obs.Jobs) != 0 {
					t.Fatal(obs)
				}
			}
			if !errors.Is(err, ErrIncomplete) {
				t.Fatalf("identity mismatch accepted: %v", err)
			}
		}
	}
}

func TestReviewRunnerIdentityCoverage(t *testing.T) {
	for _, path := range []string{"/repos/org/a/actions/runners", "/orgs/org/actions/runners", "/orgs/org/actions/runner-groups/4/runners", "/orgs/org/actions/runner-groups"} {
		for _, duplicate := range []bool{true, false} {
			field := "runners"
			if path == "/orgs/org/actions/runner-groups" {
				field = "runner_groups"
			}
			items := []string{}
			for id := 1; id <= 100; id++ {
				items = append(items, fmt.Sprintf(`{"id":%d}`, id))
			}
			last := 101
			if duplicate {
				last = 1
			}
			steps := []restStep{{key: "GET " + path + "?page=1&per_page=100", body: fmt.Sprintf(`{"%s":[%s],"total_count":101}`, field, strings.Join(items, ",")), link: `<https://example.test` + path + `?page=2&per_page=100>; rel="next"`}, {key: "GET " + path + "?page=2&per_page=100", body: fmt.Sprintf(`{"%s":[{"id":%d,"busy":true}],"total_count":101}`, field, last)}}
			c := scopedFixture(t, recordedREST(t, steps), nil)
			var err error
			var runners []Runner
			var groups []RunnerGroup
			switch path {
			case "/repos/org/a/actions/runners":
				runners, err = c.ListRepoRunners(context.Background(), "org", "a")
			case "/orgs/org/actions/runners":
				runners, err = c.ListOrgRunners(context.Background(), "org")
			case "/orgs/org/actions/runner-groups/4/runners":
				runners, err = c.ListOrgRunnerGroupRunners(context.Background(), "org", 4)
			default:
				groups, err = c.ListOrgRunnerGroups(context.Background(), "org")
			}
			if duplicate && !errors.Is(err, ErrIncomplete) || !duplicate && err != nil {
				t.Fatalf("identity coverage duplicate=%v: %v", duplicate, err)
			}
			if !duplicate {
				if field == "runners" && (len(runners) != 101 || runners[100].ID != 101 || !runners[100].Busy) {
					t.Fatalf("missing busy runner: %+v", runners)
				}
				if field == "runner_groups" && (len(groups) != 101 || groups[100].ID != 101) {
					t.Fatalf("missing group: %+v", groups)
				}
			}
		}
	}
}

func TestReviewSafeContextErrors(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		c := scopedFixture(t, doerFunc(func(r *http.Request) (*http.Response, error) { return nil, fmt.Errorf("secret-url-token: %w", cause) }), nil)
		_, err := c.ListOrgRunners(context.Background(), "org")
		if !errors.Is(err, cause) || strings.Contains(fmt.Sprint(err), "secret") {
			t.Fatalf("lost safe classification: %v", err)
		}
	}
}

func TestReviewRunPayloadRepositoryMismatch(t *testing.T) {
	for _, index := range []int{1, 2, 5} {
		steps := basicSteps()
		wrong := strings.TrimSuffix(run2, "}") + `,"repository":{"id":2,"name":"a","owner":{"login":"org"}}}`
		steps[index].body = wrong
		if index == 1 {
			steps[index].body = `{"workflow_runs":[` + wrong + `],"total_count":1}`
		}
		c := scopedFixture(t, recordedREST(t, repoSteps(steps[:index+1])), nil)
		obs, err := c.ObserveRepo(context.Background(), testRepo)
		if !errors.Is(err, ErrIncomplete) || obs.Complete {
			t.Fatalf("run from replaced repo accepted: %+v %v", obs, err)
		}
	}
}

func TestReviewExplicitTotalRequiresIdentity(t *testing.T) {
	c := scopedFixture(t, recordedREST(t, []restStep{{key: "GET /orgs/org/actions/runners?page=1&per_page=100", body: `{"runners":[{"busy":false}],"total_count":0}`}}), nil)
	if _, err := c.ListOrgRunners(context.Background(), "org"); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("explicit total skipped identity validation: %v", err)
	}
}

func TestReviewSelectedCatalogChecksCurrentAccess(t *testing.T) {
	group := `{"id":4,"name":"group","visibility":"selected","allows_public_repositories":false,"restricted_to_workflows":false}`
	steps := []restStep{{key: "GET /orgs/org/actions/runner-groups?page=1&per_page=100", body: `{"runner_groups":[` + group + `],"total_count":1}`}, {key: "GET /orgs/org/actions/runner-groups/4", body: group}, {key: "GET /orgs/org/actions/runner-groups/4/repositories?page=1&per_page=100", body: `{"repositories":[` + repoA + `],"total_count":1}`}, {key: "GET /repos/org/a", body: `{}`, status: 403}}
	c := scopedFixture(t, recordedREST(t, steps), nil)
	proof, err := c.EffectiveEligible(context.Background(), orgProfile())
	if err == nil || proof.Complete || proof.Digest != "" {
		t.Fatalf("selected inaccessible repository certified: %+v %v", proof, err)
	}
}

func TestReviewHTTPBudgetKeepsSuccessfulPage(t *testing.T) {
	now := time.Unix(0, 0)
	steps := basicSteps()
	steps[0].link = `<https://example.test/repos/org/a/actions/runs?page=2&per_page=100&status=queued>; rel="next"`
	steps = append(steps[:1], append([]restStep{{key: runsBase + "?page=2&per_page=100&status=queued", body: `{"workflow_runs":[],"total_count":0}`}}, steps[1:]...)...)
	steps = append(repoSteps(steps[:1]), repoSteps(steps[1:])...)
	d := recordedREST(t, steps)
	failed := false
	c := scopedFixture(t, doerFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" && r.URL.Host == "example.test" && r.URL.Path == "/repos/org/a/actions/runs" && r.URL.Query().Encode() == "page=2&per_page=100&status=queued" && !failed {
			failed = true
			now = now.Add(61 * time.Second)
			return nil, fmt.Errorf("secret: %w", context.DeadlineExceeded)
		}
		return d.Do(r)
	}), func() time.Time { return now })
	obs, err := c.ObserveRepo(context.Background(), testRepo)
	if !errors.Is(err, context.DeadlineExceeded) || obs.Complete || obs.ErrorCode != "BUDGET_EXHAUSTED" || len(c.observations.cursors) != 1 {
		t.Fatalf("HTTP budget lost position: %+v %v", obs, err)
	}
	obs, err = c.ObserveRepo(context.Background(), testRepo)
	if err != nil || !obs.Complete || len(obs.Jobs) != 1 {
		t.Fatalf("failed page did not resume: %+v %v", obs, err)
	}
}
