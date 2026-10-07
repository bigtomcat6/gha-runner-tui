package github

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"gha-runner-tui/internal/config"
)

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

type restStep struct {
	key, body, link string
	status          int
	after           func()
}

func recordedREST(t *testing.T, steps []restStep) HTTPDoer {
	t.Helper()
	i := 0
	t.Cleanup(func() {
		if i != len(steps) {
			t.Errorf("missing requests: got %d want %d (%+v)", i, len(steps), steps[i:])
		}
	})
	return doerFunc(func(r *http.Request) (*http.Response, error) {
		key := r.Method + " " + r.URL.Path
		if q := r.URL.Query().Encode(); q != "" {
			key += "?" + q
		}
		if r.URL.Host != "example.test" || i >= len(steps) || key != steps[i].key {
			t.Fatalf("unexpected request %s at %d", key, i)
		}
		s := steps[i]
		i++
		if s.after != nil {
			s.after()
		}
		status := s.status
		if status == 0 {
			status = 200
		}
		h := make(http.Header)
		h.Set("Link", s.link)
		return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(s.body))}, nil
	})
}

var testRepo = config.RepoRef{Owner: "org", Name: "a", ID: 1}

// Explicit repository identity read at each ObserveRepo / RefreshJob boundary.
func repoSteps(steps []restStep) []restStep {
	return append([]restStep{{key: "GET /repos/org/a", body: repoA}}, steps...)
}

func scopedFixture(t *testing.T, d HTTPDoer, now func() time.Time) Client {
	t.Helper()
	c, err := NewScopedClient("https://example.test", config.CredentialRef{ID: "test", ResourceOwner: "org", TokenEnv: "TEST_TOKEN", Repositories: []config.RepoRef{testRepo}}, CredentialIO{LookupEnv: func(string) (string, bool) { return "test-token", true }}, d, &RateBook{}, now)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const runsBase = "GET /repos/org/a/actions/runs"
const run2 = `{"id":7,"run_attempt":2,"status":"in_progress"}`
const job2 = `{"jobs":[{"id":9,"run_id":7,"run_attempt":2,"status":"queued","labels":["self-hosted","linux"]}],"total_count":1}`

func basicSteps() []restStep {
	return []restStep{
		{key: runsBase + "?page=1&per_page=100&status=queued", body: `{"workflow_runs":[],"total_count":0}`},
		{key: runsBase + "?page=1&per_page=100&status=in_progress", body: `{"workflow_runs":[` + run2 + `],"total_count":1}`},
		{key: runsBase + "/7", body: run2},
		{key: runsBase + "/7/jobs?filter=latest&page=1&per_page=100", body: job2},
		{key: runsBase + "/7/attempts/2/jobs?page=1&per_page=100", body: job2},
		{key: runsBase + "/7", body: run2},
	}
}
func TestObserveInProgressQueuedJob(t *testing.T) {
	c := scopedFixture(t, recordedREST(t, repoSteps(basicSteps())), func() time.Time { return time.Unix(0, 0) })
	obs, err := c.ObserveRepo(context.Background(), testRepo)
	if err != nil || obs.Auth != c.Auth() || !obs.Complete || len(obs.Jobs) != 1 || obs.Jobs[0].Ref.JobID != 9 || obs.Jobs[0].Ref.Attempt != 2 {
		t.Fatalf("queued job missing: %+v %v", obs, err)
	}
	if !reflect.DeepEqual(obs.Jobs[0].Labels, []string{"self-hosted", "linux"}) {
		t.Fatal(obs.Jobs)
	}
}

func TestObservePagesAttemptAndMatrix(t *testing.T) {
	for _, change := range []bool{false, true} {
		t.Run(map[bool]string{false: "second page matrix needs", true: "rerun"}[change], func(t *testing.T) {
			steps := basicSteps()
			steps[3].body = `{"jobs":[{"id":8,"run_id":7,"run_attempt":1,"status":"queued"},{"id":10,"run_id":7,"run_attempt":2,"status":"waiting"}],"total_count":3}`
			steps[3].link = `<https://example.test/repos/org/a/actions/runs/7/jobs?filter=latest&page=2&per_page=100>; rel="next"`
			steps = append(steps[:4], append([]restStep{{key: runsBase + "/7/jobs?filter=latest&page=2&per_page=100", body: job2}}, steps[4:]...)...)
			if change {
				steps[len(steps)-1].body = `{"id":7,"run_attempt":3,"status":"queued"}`
				steps = append(steps, restStep{key: runsBase + "/7/attempts/3/jobs?page=1&per_page=100", body: `{"jobs":[{"id":11,"run_id":7,"run_attempt":3,"status":"queued"},{"id":12,"run_id":7,"run_attempt":3,"status":"pending"}],"total_count":2}`}, restStep{key: runsBase + "/7", body: `{"id":7,"run_attempt":3,"status":"queued"}`})
			}
			c := scopedFixture(t, recordedREST(t, repoSteps(steps)), nil)
			obs, err := c.ObserveRepo(context.Background(), testRepo)
			wantID := int64(9)
			wantAttempt := 2
			if change {
				wantID = 11
				wantAttempt = 3
			}
			if err != nil || !obs.Complete || len(obs.Jobs) != 1 || obs.Jobs[0].Ref.JobID != wantID || obs.Jobs[0].Ref.Attempt != wantAttempt {
				t.Fatalf("%+v %v", obs, err)
			}
		})
	}
}

func TestRunsSearchCap(t *testing.T) {
	const stamp = "2026-10-01T00:00:00Z"
	capBody := `{"workflow_runs":[{"id":7,"run_attempt":2,"status":"queued","created_at":"` + stamp + `"}],"total_count":1000}`
	for _, stuck := range []bool{false, true} {
		t.Run(map[bool]string{false: "split complete", true: "same second incomplete"}[stuck], func(t *testing.T) {
			steps := []restStep{{key: runsBase + "?page=1&per_page=100&status=queued", body: capBody}, {key: runsBase + "?created=" + url.QueryEscape(stamp) + "&page=1&per_page=100&status=queued", body: map[bool]string{true: capBody, false: `{"workflow_runs":[],"total_count":0}`}[stuck]}}
			if !stuck {
				steps = append(steps, restStep{key: runsBase + "?created=" + url.QueryEscape("<"+stamp) + "&page=1&per_page=100&status=queued", body: `{"workflow_runs":[],"total_count":0}`}, restStep{key: runsBase + "?created=" + url.QueryEscape(">"+stamp) + "&page=1&per_page=100&status=queued", body: `{"workflow_runs":[],"total_count":0}`}, basicSteps()[1])
				steps[len(steps)-1].body = `{"workflow_runs":[],"total_count":0}`
			}
			c := scopedFixture(t, recordedREST(t, repoSteps(steps)), nil)
			obs, err := c.ObserveRepo(context.Background(), testRepo)
			if obs.Complete == stuck || (stuck && err == nil) || (!stuck && err != nil) {
				t.Fatalf("cap admitted or lost completeness: %+v %v", obs, err)
			}
		})
	}
}

func TestObservationBudgetResumes(t *testing.T) {
	now := time.Unix(0, 0)
	steps := basicSteps()
	steps[0].body = `{"workflow_runs":[],"total_count":0}`
	steps[0].after = func() { now = now.Add(61 * time.Second) }
	steps = append(repoSteps(steps[:1]), repoSteps(steps[1:])...)
	c := scopedFixture(t, recordedREST(t, steps), func() time.Time { return now })
	obs, err := c.ObserveRepo(context.Background(), testRepo)
	if err == nil || obs.Complete || obs.Auth != c.Auth() {
		t.Fatalf("budget must fail closed: %+v %v", obs, err)
	}
	obs, err = c.ObserveRepo(context.Background(), testRepo)
	if err != nil || !obs.Complete || len(obs.Jobs) != 1 {
		t.Fatalf("cursor not resumed: %+v %v", obs, err)
	}
}

func TestObservationAuthAcrossPages(t *testing.T) {
	steps := basicSteps()
	steps[0].body = `{"workflow_runs":[],"total_count":0}`
	steps[0].link = `<https://example.test/repos/org/a/actions/runs?page=2&per_page=100&status=queued>; rel="next"`
	steps = append(steps[:1], append([]restStep{{key: runsBase + "?page=2&per_page=100&status=queued", body: `{"workflow_runs":[],"total_count":0}`}}, steps[1:]...)...)
	steps = append(repoSteps(steps), restStep{key: "GET /repos/org/a", body: repoA}, restStep{key: runsBase + "/7", body: run2}, restStep{key: "GET /repos/org/a/actions/jobs/9", body: `{"id":9,"run_id":7,"run_attempt":2,"status":"queued"}`}, restStep{key: runsBase + "/7", body: run2}, restStep{key: "GET /repos/org/a", body: repoA})
	x := scopedFixture(t, recordedREST(t, steps), nil)
	y := scopedFixture(t, recordedREST(t, []restStep{{key: "GET /repos/org/a", status: 403, body: `{}`}, {key: "GET /repos/org/a", status: 403, body: `{}`}, {key: "GET /repos/org/a", status: 403, body: `{}`}}), nil)
	for _, c := range []Client{x, y} {
		obs, err := c.ObserveRepo(context.Background(), testRepo)
		if obs.Auth != c.Auth() || obs.Complete != (c.Auth() == x.Auth()) || (err == nil) != (c.Auth() == x.Auth()) {
			t.Fatalf("mixed auth: %+v %v", obs, err)
		}
		job, err := c.RefreshJob(context.Background(), JobRef{Repo: testRepo, RunID: 7, JobID: 9, Attempt: 2})
		if job.Auth != c.Auth() || (err == nil) != (c.Auth() == x.Auth()) {
			t.Fatalf("refresh auth %+v %v", job, err)
		}
		p := orgProfile()
		p.Target = config.TargetConfig{Scope: config.TargetScopeRepository, Owner: "org", Repo: "a"}
		proof, err := c.EffectiveEligible(context.Background(), p)
		if proof.Auth != c.Auth() || proof.Complete != (c.Auth() == x.Auth()) || (err == nil) != (c.Auth() == x.Auth()) {
			t.Fatalf("scope auth %+v %v", proof, err)
		}
	}
	if x.Auth() == y.Auth() {
		t.Fatal("generation reused")
	}
}

func TestRunnerListsPagingAndLabels(t *testing.T) {
	for _, path := range []string{"/repos/org/a/actions/runners", "/orgs/org/actions/runners", "/orgs/org/actions/runner-groups/4/runners"} {
		t.Run(path, func(t *testing.T) {
			c := scopedFixture(t, recordedREST(t, []restStep{{key: "GET " + path + "?page=1&per_page=100", body: `{"runners":[{"id":1,"labels":[{"name":"self-hosted"},{"name":"Linux"}]}],"total_count":2}`, link: `<https://example.test` + path + `?page=2&per_page=100>; rel="next"`}, {key: "GET " + path + "?page=2&per_page=100", body: `{"runners":[{"id":2,"labels":[{"name":"ARM64"}]}],"total_count":2}`}}), nil)
			var runners []Runner
			var err error
			switch path {
			case "/repos/org/a/actions/runners":
				runners, err = c.ListRepoRunners(context.Background(), "org", "a")
			case "/orgs/org/actions/runners":
				runners, err = c.ListOrgRunners(context.Background(), "org")
			default:
				runners, err = c.ListOrgRunnerGroupRunners(context.Background(), "org", 4)
			}
			if err != nil || len(runners) != 2 || !reflect.DeepEqual(runners[0].Labels, []string{"self-hosted", "Linux"}) {
				t.Fatalf("%+v %v", runners, err)
			}
		})
	}
}

func TestObserveIncompleteResponses(t *testing.T) {
	for _, tc := range []struct {
		name       string
		index      int
		body, link string
	}{
		{"missing total", 0, `{"workflow_runs":[]}`, ""},
		{"truncated", 0, `{"workflow_runs":[],"total_count":2}`, ""},
		{"wrong repo link", 0, `{"workflow_runs":[],"total_count":0}`, `<https://example.test/evil/repos/org/a/actions/runs?page=2&per_page=100&status=queued>; rel="next"`},
		{"wrong query link", 0, `{"workflow_runs":[],"total_count":0}`, `<https://example.test/repos/org/a/actions/runs?page=2&per_page=100&status=in_progress>; rel="next"`},
		{"external link", 0, `{"workflow_runs":[],"total_count":0}`, `<https://evil.test/repos/org/a/actions/runs?page=2&per_page=100&status=queued>; rel="next"`},
		{"wrong run identity", 2, `{"id":8,"run_attempt":2,"status":"in_progress"}`, ""},
		{"missing run attempt", 2, `{"id":7,"status":"in_progress"}`, ""},
		{"missing run status", 2, `{"id":7,"run_attempt":2}`, ""},
		{"missing jobs", 3, `{"total_count":0}`, ""},
		{"wrong job attempt", 4, `{"jobs":[{"id":9,"run_id":7,"run_attempt":1,"status":"queued"}],"total_count":1}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := basicSteps()
			steps[tc.index].body = tc.body
			steps[tc.index].link = tc.link
			end := tc.index + 1
			if tc.index == 4 {
				end = 6
			}
			c := scopedFixture(t, recordedREST(t, repoSteps(steps[:end])), nil)
			obs, err := c.ObserveRepo(context.Background(), testRepo)
			if err == nil || obs.Complete || obs.Auth != c.Auth() || obs.ErrorCode == "" {
				t.Fatalf("incomplete admitted %+v %v", obs, err)
			}
		})
	}
}

func TestFirstObservedAndRefreshRerun(t *testing.T) {
	now := time.Unix(1, 0)
	steps := append(repoSteps(basicSteps()), repoSteps(basicSteps())...)
	steps = append(steps, restStep{key: "GET /repos/org/a", body: repoA}, restStep{key: runsBase + "/7", body: `{"id":7,"run_attempt":3,"status":"queued"}`})
	c := scopedFixture(t, recordedREST(t, steps), func() time.Time { return now })
	a, err := c.ObserveRepo(context.Background(), testRepo)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	b, err := c.ObserveRepo(context.Background(), testRepo)
	if err != nil || b.Jobs[0].FirstObserved != a.Jobs[0].FirstObserved {
		t.Fatalf("lost local first observed %+v %v", b, err)
	}
	job, err := c.RefreshJob(context.Background(), a.Jobs[0].Ref)
	if err == nil || job.Auth != c.Auth() {
		t.Fatal("historical job refreshed as candidate")
	}
}

func TestBudgetPreservesJobPageCursor(t *testing.T) {
	now := time.Unix(0, 0)
	steps := basicSteps()
	steps[3].link = `<https://example.test/repos/org/a/actions/runs/7/jobs?filter=latest&page=2&per_page=100>; rel="next"`
	steps[3].after = func() { now = now.Add(61 * time.Second) }
	steps = append(steps[:4], append([]restStep{{key: runsBase + "/7", body: run2}, {key: runsBase + "/7/jobs?filter=latest&page=2&per_page=100", body: `{"jobs":[],"total_count":1}`}}, steps[4:]...)...)
	steps = append(repoSteps(steps[:4]), repoSteps(steps[4:])...)
	c := scopedFixture(t, recordedREST(t, steps), func() time.Time { return now })
	obs, err := c.ObserveRepo(context.Background(), testRepo)
	if err == nil || obs.Complete {
		t.Fatal("budget admitted")
	}
	obs, err = c.ObserveRepo(context.Background(), testRepo)
	if err != nil || !obs.Complete || len(obs.Jobs) != 1 {
		t.Fatalf("job page cursor lost %+v %v", obs, err)
	}
}

func TestRefreshMissingAttemptAfterRead(t *testing.T) {
	steps := []restStep{{key: runsBase + "/7", body: run2}, {key: "GET /repos/org/a/actions/jobs/9", body: `{"id":9,"run_id":7,"run_attempt":2,"status":"queued"}`}, {key: runsBase + "/7", body: `{"id":7,"status":"in_progress"}`}}
	c := scopedFixture(t, recordedREST(t, repoSteps(steps)), nil)
	obs, err := c.RefreshJob(context.Background(), JobRef{Repo: testRepo, RunID: 7, JobID: 9, Attempt: 2})
	if err == nil || obs.Auth != c.Auth() {
		t.Fatalf("missing final attempt accepted %+v %v", obs, err)
	}
}

func TestPagination304RetainsNext(t *testing.T) {
	calls := 0
	d := doerFunc(func(r *http.Request) (*http.Response, error) {
		key := r.Method + " " + r.URL.Path + "?" + r.URL.Query().Encode()
		want := []string{"GET /orgs/org/actions/runners?page=1&per_page=100", "GET /orgs/org/actions/runners?page=2&per_page=100", "GET /orgs/org/actions/runners?page=1&per_page=100", "GET /orgs/org/actions/runners?page=2&per_page=100"}
		if calls >= len(want) || key != want[calls] {
			t.Fatalf("unexpected %s at %d", key, calls)
		}
		status := 200
		body := `{"runners":[{"id":1}],"total_count":2}`
		h := make(http.Header)
		h.Set("ETag", "test-etag")
		if calls%2 == 0 {
			h.Set("Link", `<https://example.test/orgs/org/actions/runners?page=2&per_page=100>; rel="next"`)
		} else {
			body = `{"runners":[{"id":2}],"total_count":2}`
		}
		if calls >= 2 {
			status = 304
			body = ""
			h.Del("Link")
			if r.Header.Get("If-None-Match") != "test-etag" {
				t.Fatal("cache identity lost")
			}
		}
		calls++
		return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	c := scopedFixture(t, d, nil)
	for i := 0; i < 2; i++ {
		r, err := c.ListOrgRunners(context.Background(), "org")
		if err != nil || len(r) != 2 {
			t.Fatalf("cached pagination lost %+v %v", r, err)
		}
	}
	if calls != 4 {
		t.Fatal("required calls missing")
	}
}

func TestBudgetPreservesCompletedRunCursor(t *testing.T) {
	now := time.Unix(0, 0)
	steps := basicSteps()
	steps[0].body = `{"workflow_runs":[` + run2 + `,{"id":8,"run_attempt":2,"status":"queued"}],"total_count":2}`
	steps[1].body = `{"workflow_runs":[],"total_count":0}`
	steps[5].after = func() { now = now.Add(61 * time.Second) }
	steps = append(steps, restStep{key: runsBase + "/8", body: `{"id":8,"run_attempt":2,"status":"queued"}`}, restStep{key: runsBase + "/8/jobs?filter=latest&page=1&per_page=100", body: `{"jobs":[],"total_count":0}`}, restStep{key: runsBase + "/8/attempts/2/jobs?page=1&per_page=100", body: `{"jobs":[],"total_count":0}`}, restStep{key: runsBase + "/8", body: `{"id":8,"run_attempt":2,"status":"queued"}`})
	steps = append(repoSteps(steps[:6]), repoSteps(steps[6:])...)
	c := scopedFixture(t, recordedREST(t, steps), func() time.Time { return now })
	obs, err := c.ObserveRepo(context.Background(), testRepo)
	if err == nil || obs.Complete {
		t.Fatal("budget admitted")
	}
	obs, err = c.ObserveRepo(context.Background(), testRepo)
	if err != nil || !obs.Complete || len(obs.Jobs) != 1 {
		t.Fatalf("completed run cursor lost %+v %v", obs, err)
	}
}

func TestObserveCompletedRunCannotSupplyQueuedCandidate(t *testing.T) {
	steps := basicSteps()
	steps[5].body = `{"id":7,"run_attempt":2,"status":"completed"}`
	c := scopedFixture(t, recordedREST(t, repoSteps(steps)), nil)
	obs, err := c.ObserveRepo(context.Background(), testRepo)
	if err != nil || !obs.Complete || len(obs.Jobs) != 0 {
		t.Fatalf("completed run supplied queued job %+v %v", obs, err)
	}
}

func TestRefreshCompletedRunRejectsQueuedCandidate(t *testing.T) {
	steps := []restStep{{key: runsBase + "/7", body: run2}, {key: "GET /repos/org/a/actions/jobs/9", body: `{"id":9,"run_id":7,"run_attempt":2,"status":"queued"}`}, {key: runsBase + "/7", body: `{"id":7,"run_attempt":2,"status":"completed"}`}}
	c := scopedFixture(t, recordedREST(t, repoSteps(steps)), nil)
	obs, err := c.RefreshJob(context.Background(), JobRef{Repo: testRepo, RunID: 7, JobID: 9, Attempt: 2})
	if err == nil || obs.Auth != c.Auth() {
		t.Fatal("completed run refreshed queued candidate")
	}
}

func TestPreciseAttemptJobsAllPages(t *testing.T) {
	steps := basicSteps()
	steps[4].body = `{"jobs":[],"total_count":1}`
	steps[4].link = `<https://example.test/repos/org/a/actions/runs/7/attempts/2/jobs?page=2&per_page=100>; rel="next"`
	steps = append(steps[:5], append([]restStep{{key: runsBase + "/7/attempts/2/jobs?page=2&per_page=100", body: job2}}, steps[5:]...)...)
	c := scopedFixture(t, recordedREST(t, repoSteps(steps)), nil)
	obs, err := c.ObserveRepo(context.Background(), testRepo)
	if err != nil || !obs.Complete || len(obs.Jobs) != 1 || obs.Jobs[0].Ref.Attempt != 2 {
		t.Fatalf("precise attempt pagination missing %+v %v", obs, err)
	}
}

func TestNewGenerationCannotResumeOldCursor(t *testing.T) {
	now := time.Unix(0, 0)
	x := scopedFixture(t, recordedREST(t, repoSteps([]restStep{{key: runsBase + "?page=1&per_page=100&status=queued", body: `{"workflow_runs":[],"total_count":0}`, link: `<https://example.test/repos/org/a/actions/runs?page=2&per_page=100&status=queued>; rel="next"`, after: func() { now = now.Add(61 * time.Second) }}})), func() time.Time { return now })
	a, err := x.ObserveRepo(context.Background(), testRepo)
	if err == nil || a.Complete || a.Auth != x.Auth() {
		t.Fatal("incomplete old generation admitted")
	}
	y := scopedFixture(t, recordedREST(t, []restStep{{key: "GET /repos/org/a", status: 403, body: `{}`}}), func() time.Time { return now })
	b, err := y.ObserveRepo(context.Background(), testRepo)
	if err == nil || b.Complete || b.Auth != y.Auth() || b.Auth == a.Auth {
		t.Fatal("old cursor crossed generation")
	}
}

func TestPaginationCountCannotShrinkToHideMissingJobs(t *testing.T) {
	steps := basicSteps()
	steps[3].body = `{"jobs":[],"total_count":3}`
	steps[3].link = `<https://example.test/repos/org/a/actions/runs/7/jobs?filter=latest&page=2&per_page=100>; rel="next"`
	steps = append(steps[:4], restStep{key: runsBase + "/7/jobs?filter=latest&page=2&per_page=100", body: job2})
	c := scopedFixture(t, recordedREST(t, repoSteps(steps)), nil)
	obs, err := c.ObserveRepo(context.Background(), testRepo)
	if err == nil || obs.Complete || obs.Auth != c.Auth() {
		t.Fatalf("shrinking total hid missing jobs %+v %v", obs, err)
	}
}

func TestDuplicateJobsCannotSatisfyCompleteTotal(t *testing.T) {
	steps := basicSteps()
	steps[4].body = `{"jobs":[{"id":9,"run_id":7,"run_attempt":2,"status":"queued"},{"id":9,"run_id":7,"run_attempt":2,"status":"queued"}],"total_count":2}`
	c := scopedFixture(t, recordedREST(t, repoSteps(steps[:5])), nil)
	obs, err := c.ObserveRepo(context.Background(), testRepo)
	if err == nil || obs.Complete {
		t.Fatal("duplicate identity hid missing job")
	}
}
