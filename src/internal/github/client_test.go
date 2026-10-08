package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"gha-runner-tui/internal/config"
	"gha-runner-tui/internal/state"
)

type fakeHTTPDoer struct {
	response *http.Response
	err      error
	check    func(*http.Request)
}

func (f fakeHTTPDoer) Do(req *http.Request) (*http.Response, error) {
	if f.check != nil {
		f.check(req)
	}
	return f.response, f.err
}

func TestListRepoRunnersParsesGitHubResponse(t *testing.T) {
	t.Setenv("TEST_GITHUB_TOKEN", "test-token")
	client := NewClient("https://example.test", "TEST_GITHUB_TOKEN", "", nil, fakeHTTPDoer{
		check: func(r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/repos/bigtomcat6/remind-me/actions/runners" || r.URL.RawQuery != "" {
				t.Fatalf("request: %s %s", r.Method, r.URL)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
				t.Fatalf("expected auth header, got %q", got)
			}
		},
		response: &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"runners":[{"id":1,"name":"remind-me-swift-123","status":"online","busy":true}]}`)),
			Header:     make(http.Header),
		},
	})

	runners, err := client.ListRepoRunners(context.Background(), "bigtomcat6", "remind-me")
	if err != nil {
		t.Fatalf("ListRepoRunners returned error: %v", err)
	}
	if len(runners) != 1 {
		t.Fatalf("expected 1 runner, got %d", len(runners))
	}
	if runners[0].Status != state.GitHubOnline || !runners[0].Busy {
		t.Fatalf("unexpected runner payload: %+v", runners[0])
	}
}

func TestListRepoRunnersRequiresToken(t *testing.T) {
	t.Setenv("TEST_GITHUB_TOKEN_EMPTY", "")
	client := NewClient("https://api.github.com", "TEST_GITHUB_TOKEN_EMPTY", "", nil, nil)

	_, err := client.ListRepoRunners(context.Background(), "bigtomcat6", "remind-me")
	if err != ErrMissingToken {
		t.Fatalf("expected ErrMissingToken, got %v", err)
	}
}

func TestListRepoRunnersReadsEnvStyleTokenFile(t *testing.T) {
	t.Setenv("TEST_GITHUB_TOKEN_FROM_FILE", "")
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "github.env")
	if err := os.WriteFile(tokenFile, []byte("GITHUB_TOKEN=test-token-from-file\n"), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	client := NewClient("https://example.test", "GITHUB_TOKEN", tokenFile, nil, fakeHTTPDoer{
		check: func(r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/repos/bigtomcat6/remind-me/actions/runners" || r.URL.RawQuery != "" {
				t.Fatalf("request: %s %s", r.Method, r.URL)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer test-token-from-file" {
				t.Fatalf("expected auth header from env file, got %q", got)
			}
		},
		response: &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"runners":[]}`)),
			Header:     make(http.Header),
		},
	})

	if _, err := client.ListRepoRunners(context.Background(), "bigtomcat6", "remind-me"); err != nil {
		t.Fatalf("ListRepoRunners returned error: %v", err)
	}
}

func TestNewClientDoesNotInjectImplicitLegacyTokenFile(t *testing.T) {
	t.Parallel()

	client := NewClient("https://example.test", "TEST_GITHUB_TOKEN", "", nil, nil)
	if client.tokenFile != "" {
		t.Fatalf("expected empty tokenFile without explicit configuration, got %q", client.tokenFile)
	}
}

func TestMatchRunnerPrefersExactName(t *testing.T) {
	t.Parallel()

	runners := []Runner{
		{Name: "remind-me-swift-older", Status: state.GitHubOffline},
		{Name: "remind-me-swift-20260601", Status: state.GitHubOnline},
	}

	match := MatchRunner(runners, "remind-me-swift-20260601", "remind-me-swift")
	if match == nil {
		t.Fatal("expected match, got nil")
	}
	if match.Name != "remind-me-swift-20260601" {
		t.Fatalf("expected exact match, got %+v", match)
	}
}

func TestListOrgRunnersUsesOrganizationEndpoint(t *testing.T) {
	t.Setenv("TEST_GITHUB_TOKEN", "test-token")
	client := NewClient("https://example.test", "TEST_GITHUB_TOKEN", "", nil, fakeHTTPDoer{
		check: func(r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/orgs/example-org/actions/runners" || r.URL.RawQuery != "" {
				t.Fatalf("request: %s %s", r.Method, r.URL)
			}
		},
		response: &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(
				`{"runners":[{"id":1,"name":"example-org-swift-1","status":"online","busy":false,"runner_group_id":42}]}`,
			)),
			Header: make(http.Header),
		},
	})

	runners, err := client.ListOrgRunners(context.Background(), "example-org")
	if err != nil {
		t.Fatalf("ListOrgRunners returned error: %v", err)
	}
	if len(runners) != 1 || runners[0].RunnerGroupID != 42 {
		t.Fatalf("unexpected runners: %+v", runners)
	}
}

func TestCreateOrgRegistrationTokenUsesOrganizationEndpoint(t *testing.T) {
	t.Setenv("TEST_GITHUB_TOKEN", "test-token")
	client := NewClient("https://example.test", "TEST_GITHUB_TOKEN", "", nil, fakeHTTPDoer{
		check: func(r *http.Request) {
			if r.Method != http.MethodPost {
				t.Fatalf("unexpected method: %s", r.Method)
			}
			if r.URL.Path != "/orgs/example-org/actions/runners/registration-token" {
				t.Fatalf("unexpected path: %s", r.URL.Path)
			}
			if r.URL.RawQuery != "" {
				t.Fatalf("unexpected query: %s", r.URL.RawQuery)
			}
		},
		response: &http.Response{
			StatusCode: http.StatusCreated,
			Body:       io.NopCloser(strings.NewReader(`{"token":"abc"}`)),
			Header:     make(http.Header),
		},
	})

	token, err := client.CreateOrgRegistrationToken(context.Background(), "example-org")
	if err != nil {
		t.Fatalf("CreateOrgRegistrationToken returned error: %v", err)
	}
	if token != "abc" {
		t.Fatalf("expected abc token, got %q", token)
	}
}

func TestListOrgRunnerGroupsParsesGroups(t *testing.T) {
	t.Setenv("TEST_GITHUB_TOKEN", "test-token")
	client := NewClient("https://example.test", "TEST_GITHUB_TOKEN", "", nil, fakeHTTPDoer{
		check: func(r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/orgs/example-org/actions/runner-groups" || r.URL.RawQuery != "" {
				t.Fatalf("request: %s %s", r.Method, r.URL)
			}
		},
		response: &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(
				`{"runner_groups":[{"id":42,"name":"example-org-swift","visibility":"private","allows_public_repositories":false}]}`,
			)),
			Header: make(http.Header),
		},
	})

	groups, err := client.ListOrgRunnerGroups(context.Background(), "example-org")
	if err != nil {
		t.Fatalf("ListOrgRunnerGroups returned error: %v", err)
	}
	if len(groups) != 1 || groups[0].ID != 42 || groups[0].Visibility != "private" || groups[0].AllowsPublicRepositories {
		t.Fatalf("unexpected groups: %+v", groups)
	}
}

func TestListOrgRunnerGroupRunnersUsesGroupEndpoint(t *testing.T) {
	t.Setenv("TEST_GITHUB_TOKEN", "test-token")
	client := NewClient("https://example.test", "TEST_GITHUB_TOKEN", "", nil, fakeHTTPDoer{
		check: func(r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/orgs/example-org/actions/runner-groups/42/runners" || r.URL.RawQuery != "" {
				t.Fatalf("request: %s %s", r.Method, r.URL)
			}
		},
		response: &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(
				`{"runners":[{"id":1,"name":"example-org-swift-1","status":"online","busy":true}]}`,
			)),
			Header: make(http.Header),
		},
	})

	runners, err := client.ListOrgRunnerGroupRunners(context.Background(), "example-org", 42)
	if err != nil {
		t.Fatalf("ListOrgRunnerGroupRunners returned error: %v", err)
	}
	if len(runners) != 1 || !runners[0].Busy {
		t.Fatalf("unexpected runners: %+v", runners)
	}
}

func TestCreateOrgRunnerGroupDefaultsToPrivateRepositories(t *testing.T) {
	t.Setenv("TEST_GITHUB_TOKEN", "test-token")
	client := NewClient("https://example.test", "TEST_GITHUB_TOKEN", "", nil, fakeHTTPDoer{
		check: func(r *http.Request) {
			if r.Method != http.MethodPost {
				t.Fatalf("unexpected method: %s", r.Method)
			}
			if r.URL.Path != "/orgs/example-org/actions/runner-groups" {
				t.Fatalf("unexpected path: %s", r.URL.Path)
			}
			if r.URL.RawQuery != "" {
				t.Fatalf("unexpected query: %s", r.URL.RawQuery)
			}
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"name":"example-org-swift"`) {
				t.Fatalf("missing group name in body: %s", string(body))
			}
			if !strings.Contains(string(body), `"visibility":"private"`) {
				t.Fatalf("missing visibility in body: %s", string(body))
			}
			if !strings.Contains(string(body), `"allows_public_repositories":false`) {
				t.Fatalf("missing public repo policy in body: %s", string(body))
			}
		},
		response: &http.Response{
			StatusCode: http.StatusCreated,
			Body: io.NopCloser(strings.NewReader(
				`{"id":42,"name":"example-org-swift","visibility":"private","allows_public_repositories":false}`,
			)),
			Header: make(http.Header),
		},
	})

	group, err := client.CreateOrgRunnerGroup(context.Background(), "example-org", "example-org-swift", "private")
	if err != nil {
		t.Fatalf("CreateOrgRunnerGroup returned error: %v", err)
	}
	if group.ID != 42 || group.AllowsPublicRepositories {
		t.Fatalf("unexpected group: %+v", group)
	}
}

func TestUpdateOrgRunnerGroupDisablesPublicRepositories(t *testing.T) {
	t.Setenv("TEST_GITHUB_TOKEN", "test-token")
	client := NewClient("https://example.test", "TEST_GITHUB_TOKEN", "", nil, fakeHTTPDoer{
		check: func(r *http.Request) {
			if r.Method != http.MethodPatch {
				t.Fatalf("unexpected method: %s", r.Method)
			}
			if r.URL.Path != "/orgs/example-org/actions/runner-groups/42" {
				t.Fatalf("unexpected path: %s", r.URL.Path)
			}
			if r.URL.RawQuery != "" {
				t.Fatalf("unexpected query: %s", r.URL.RawQuery)
			}
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"visibility":"private"`) {
				t.Fatalf("missing visibility in body: %s", string(body))
			}
			if !strings.Contains(string(body), `"allows_public_repositories":false`) {
				t.Fatalf("missing public repo policy in body: %s", string(body))
			}
		},
		response: &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(
				`{"id":42,"name":"example-org-swift","visibility":"private","allows_public_repositories":false}`,
			)),
			Header: make(http.Header),
		},
	})

	group, err := client.UpdateOrgRunnerGroup(context.Background(), "example-org", 42, "example-org-swift", "private")
	if err != nil {
		t.Fatalf("UpdateOrgRunnerGroup returned error: %v", err)
	}
	if group.Visibility != "private" || group.AllowsPublicRepositories {
		t.Fatalf("unexpected group: %+v", group)
	}
}

func TestDeleteOrgRunnerGroupUsesGroupIDEndpoint(t *testing.T) {
	t.Setenv("TEST_GITHUB_TOKEN", "test-token")
	client := NewClient("https://example.test", "TEST_GITHUB_TOKEN", "", nil, fakeHTTPDoer{
		check: func(r *http.Request) {
			if r.Method != http.MethodDelete {
				t.Fatalf("unexpected method: %s", r.Method)
			}
			if r.URL.Path != "/orgs/example-org/actions/runner-groups/42" {
				t.Fatalf("unexpected path: %s", r.URL.Path)
			}
			if r.URL.RawQuery != "" {
				t.Fatalf("unexpected query: %s", r.URL.RawQuery)
			}
		},
		response: &http.Response{
			StatusCode: http.StatusNoContent,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     make(http.Header),
		},
	})

	if err := client.DeleteOrgRunnerGroup(context.Background(), "example-org", 42); err != nil {
		t.Fatalf("DeleteOrgRunnerGroup returned error: %v", err)
	}
}

func TestQueuedJobsIncludesInProgressAndMatchesSubset(t *testing.T) {
	t.Setenv("TEST_QUEUE_TOKEN", "fake-queue-token")
	seen := map[string]int{}
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method: %s", r.Method)
		}
		if r.URL.Query().Get("per_page") != "100" {
			t.Error("per_page")
		}
		switch r.URL.Path {
		case "/repos/me/app/actions/runs":
			status := r.URL.Query().Get("status")
			if status != "queued" && status != "in_progress" {
				t.Errorf("status: %s", status)
			}
			mu.Lock()
			seen[status]++
			mu.Unlock()
			fmt.Fprint(w, `{"workflow_runs":[{"id":10}]}`)
		case "/repos/me/app/actions/runs/10/jobs":
			if r.URL.Query().Get("filter") != "latest" {
				t.Error("filter")
			}
			fmt.Fprint(w, `{"jobs":[
                {"id":1,"status":"queued","labels":["SELF-HOSTED","Linux"]},
                {"id":2,"status":"queued","labels":["ubuntu-latest"]},
                {"id":3,"status":"queued","labels":[]},
                {"id":4,"status":"in_progress","labels":["linux"]}]}`)
		default:
			t.Errorf("path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "TEST_QUEUE_TOKEN", "", nil, srv.Client())
	jobs, err := c.QueuedJobs(context.Background(), []Repository{{Owner: "me", Name: "app"}}, []string{"linux", "x64"})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(jobs) != 1 || jobs[0].ID != 1 || seen["queued"] != 1 || seen["in_progress"] != 1 {
		t.Fatalf("jobs=%v, requests=%v", jobs, seen)
	}
}

func TestProfileMissingTokenFileDoesNotUseEnvironment(t *testing.T) {
	t.Setenv("TEST_STRICT_TOKEN", "fake-wrong-identity")
	c := NewClient("https://example.test", "TEST_STRICT_TOKEN", filepath.Join(t.TempDir(), "missing.env"), nil, nil)
	if _, err := c.resolveToken(context.Background()); err == nil {
		t.Fatal("expected strict file error")
	}
}

type tokenRunnerFunc func(context.Context, string, ...string) ([]byte, error)

func (f tokenRunnerFunc) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f(ctx, name, args...)
}

func TestProfileTokenFileFormatsAndAuthorization(t *testing.T) {
	t.Setenv("TEST_FILE_TOKEN", "fake-wrong-environment-identity")
	for _, tc := range []struct {
		name, content, want string
		missing, sudo       bool
	}{
		{name: "raw", content: "fake-file-token", want: "fake-file-token"},
		{name: "raw-tail-newline", content: "fake-file-token\n", want: "fake-file-token"},
		{name: "sudo-raw", content: "fake-file-token\n", want: "fake-file-token", sudo: true},
		{name: "env-export-quotes-comments", content: "# comment\nexport TEST_FILE_TOKEN='fake-file-token'\n", want: "fake-file-token"},
		{name: "env-missing-key", content: "OTHER_TOKEN=fake-other-identity\n"},
		{name: "env-empty-value", content: "TEST_FILE_TOKEN=\n"},
		{name: "empty", content: " \n"},
		{name: "comment-not-raw", content: "#fake-not-a-token\n"},
		{name: "malformed-multiline", content: "fake-one\nfake-two\n"},
		{name: "strict-missing", missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path, envPath := filepath.Join(root, "token"), filepath.Join(root, "other.env")
			if err := os.WriteFile(envPath, []byte("TEST_FILE_TOKEN=fake-other-file-identity\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if !tc.missing {
				if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != "GET" || r.URL.Path != "/repos/me/app/actions/runners" || r.URL.RawQuery != "" {
					t.Errorf("request: %s %s", r.Method, r.URL)
				}
				if r.Header.Get("Authorization") != "Bearer "+tc.want {
					t.Error("wrong credential identity")
				}
				fmt.Fprint(w, `{"runners":[]}`)
			}))
			defer srv.Close()
			var cats int
			runner := tokenRunnerFunc(func(_ context.Context, name string, args ...string) ([]byte, error) {
				cats++
				if (!tc.sudo && !tc.missing) || name != "sudo" || len(args) != 3 || args[0] != "-n" || args[1] != "cat" || args[2] != path {
					t.Fatalf("unexpected credential read: %s %v", name, args)
				}
				if tc.missing {
					return nil, fs.ErrNotExist
				}
				return []byte(tc.content), nil
			})
			c := NewClient(srv.URL, "TEST_FILE_TOKEN", "", runner, srv.Client()).ForProfile(
				config.GitHubConfig{APIBaseURL: srv.URL, TokenEnv: "TEST_FILE_TOKEN"},
				config.GitHubProfile{TokenFile: path, EnvFile: envPath},
			)
			if tc.sudo {
				c.readFile = func(string) ([]byte, error) { return nil, fs.ErrPermission }
			}
			_, err := c.ListRepoRunners(context.Background(), "me", "app")
			if tc.want == "" {
				if err == nil || requests.Load() != 0 {
					t.Fatal("invalid strict file used a fallback identity")
				}
			} else if err != nil || requests.Load() != 1 {
				t.Fatalf("request count=%d err=%v", requests.Load(), err)
			}
			wantCats := 0
			if tc.sudo || tc.missing {
				wantCats = 1
			}
			if cats != wantCats {
				t.Fatalf("sudo reads=%d want=%d", cats, wantCats)
			}
		})
	}
}

func TestProfileEnvFileUsesRawAndEnvFormats(t *testing.T) {
	t.Setenv("TEST_PROFILE_ENV_TOKEN", "fake-wrong-environment")
	for _, tc := range []struct{ name, content, want string }{
		{name: "raw", content: "fake-env-file-token", want: "fake-env-file-token"},
		{name: "env", content: "export TEST_PROFILE_ENV_TOKEN='fake-env-file-token'\n", want: "fake-env-file-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "profile.env")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/repos/me/app/actions/runners" || r.URL.RawQuery != "" {
					t.Errorf("request: %s %s", r.Method, r.URL)
				}
				if r.Header.Get("Authorization") != "Bearer "+tc.want {
					t.Errorf("wrong credential identity: %q", r.Header.Get("Authorization"))
				}
				fmt.Fprint(w, `{"runners":[]}`)
			}))
			defer srv.Close()
			c := NewClient(srv.URL, "TEST_PROFILE_ENV_TOKEN", "", nil, srv.Client()).ForProfile(
				config.GitHubConfig{APIBaseURL: srv.URL, TokenEnv: "TEST_PROFILE_ENV_TOKEN"},
				config.GitHubProfile{EnvFile: path},
			)
			if _, err := c.ListRepoRunners(context.Background(), "me", "app"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProfileEnvFileStrictWhenMissing(t *testing.T) {
	t.Setenv("TEST_PROFILE_ENV_TOKEN", "fake-wrong-environment")
	missing := filepath.Join(t.TempDir(), "missing.env")
	var requests atomic.Int32
	c := NewClient("https://example.test", "TEST_PROFILE_ENV_TOKEN", "", nil, fakeHTTPDoer{
		err:   errors.New("unexpected http request"),
		check: func(*http.Request) { requests.Add(1) },
	}).ForProfile(
		config.GitHubConfig{APIBaseURL: "https://example.test", TokenEnv: "TEST_PROFILE_ENV_TOKEN"},
		config.GitHubProfile{EnvFile: missing},
	)
	if _, err := c.ListRepoRunners(context.Background(), "me", "app"); err == nil {
		t.Fatal("expected strict missing env_file error")
	}
	if requests.Load() != 0 {
		t.Fatalf("expected no request, got %d", requests.Load())
	}
}

func TestGlobalClientFallsBackToEnvOnlyWhenFileMissing(t *testing.T) {
	t.Setenv("TEST_GLOBAL_TOKEN", "fake-global-env")
	missing := filepath.Join(t.TempDir(), "missing.env")
	client := NewGlobalClient("https://example.test", "TEST_GLOBAL_TOKEN", missing, nil, fakeHTTPDoer{
		check: func(r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/repos/me/app/actions/runners" || r.URL.RawQuery != "" {
				t.Errorf("request: %s %s", r.Method, r.URL)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer fake-global-env" {
				t.Fatalf("expected env fallback, got %q", got)
			}
		},
		response: &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"runners":[]}`)),
			Header:     make(http.Header),
		},
	})
	if _, err := client.ListRepoRunners(context.Background(), "me", "app"); err != nil {
		t.Fatal(err)
	}
}

func TestGlobalClientFileTakesPriorityOverEnv(t *testing.T) {
	t.Setenv("TEST_GLOBAL_TOKEN", "fake-env-identity")
	path := filepath.Join(t.TempDir(), "github.env")
	if err := os.WriteFile(path, []byte("TEST_GLOBAL_TOKEN=fake-file-identity\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := NewGlobalClient("https://example.test", "TEST_GLOBAL_TOKEN", path, nil, fakeHTTPDoer{
		check: func(r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/repos/me/app/actions/runners" || r.URL.RawQuery != "" {
				t.Errorf("request: %s %s", r.Method, r.URL)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer fake-file-identity" {
				t.Fatalf("expected file identity, got %q", got)
			}
		},
		response: &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"runners":[]}`)),
			Header:     make(http.Header),
		},
	})
	if _, err := client.ListRepoRunners(context.Background(), "me", "app"); err != nil {
		t.Fatal(err)
	}
}

func TestGlobalClientInvalidFileDoesNotFallBackToEnv(t *testing.T) {
	t.Setenv("TEST_GLOBAL_TOKEN", "fake-valid-env-identity")
	for _, tc := range []struct{ name, content string }{
		{name: "missing-key", content: "OTHER_TOKEN=fake-other-identity\n"},
		{name: "empty-value", content: "TEST_GLOBAL_TOKEN=\n"},
		{name: "comment-only", content: "# comment only\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "github.env")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			requests := 0
			client := NewGlobalClient("https://example.test", "TEST_GLOBAL_TOKEN", path, nil, fakeHTTPDoer{
				err:   errors.New("unexpected http request"),
				check: func(*http.Request) { requests++ },
			})
			if _, err := client.ListRepoRunners(context.Background(), "me", "app"); err == nil {
				t.Fatal("expected parse error")
			}
			if requests != 0 {
				t.Fatal("invalid file must not fall back to environment")
			}
		})
	}
}

func TestGlobalClientPermissionUsesSudoThenFails(t *testing.T) {
	t.Setenv("TEST_GLOBAL_TOKEN", "fake-valid-env-identity")
	path := filepath.Join(t.TempDir(), "github.env")
	if err := os.WriteFile(path, []byte("TEST_GLOBAL_TOKEN=fake-file-identity\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var cats int
	runner := tokenRunnerFunc(func(_ context.Context, name string, args ...string) ([]byte, error) {
		cats++
		if name != "sudo" || len(args) != 3 || args[0] != "-n" || args[1] != "cat" || args[2] != path {
			t.Fatalf("unexpected read: %s %v", name, args)
		}
		return nil, fs.ErrPermission
	})
	requests := 0
	client := NewGlobalClient("https://example.test", "TEST_GLOBAL_TOKEN", path, runner, fakeHTTPDoer{
		err:   errors.New("unexpected http request"),
		check: func(*http.Request) { requests++ },
	})
	client.readFile = func(string) ([]byte, error) { return nil, fs.ErrPermission }
	if _, err := client.ListRepoRunners(context.Background(), "me", "app"); err == nil {
		t.Fatal("expected safe error when sudo cannot read")
	}
	if cats != 1 || requests != 0 {
		t.Fatalf("cats=%d requests=%d", cats, requests)
	}
}

func TestGlobalClientPermissionSudoSuccessPrefersFile(t *testing.T) {
	t.Setenv("TEST_GLOBAL_TOKEN", "fake-valid-env-identity")
	path := filepath.Join(t.TempDir(), "github.env")
	if err := os.WriteFile(path, []byte("TEST_GLOBAL_TOKEN=fake-file-identity\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var cats int
	runner := tokenRunnerFunc(func(_ context.Context, name string, args ...string) ([]byte, error) {
		cats++
		if name != "sudo" || len(args) != 3 || args[0] != "-n" || args[1] != "cat" || args[2] != path {
			t.Fatalf("unexpected read: %s %v", name, args)
		}
		return []byte("TEST_GLOBAL_TOKEN=fake-sudo-file-identity\n"), nil
	})
	client := NewGlobalClient("https://example.test", "TEST_GLOBAL_TOKEN", path, runner, fakeHTTPDoer{
		check: func(r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/repos/me/app/actions/runners" || r.URL.RawQuery != "" {
				t.Errorf("request: %s %s", r.Method, r.URL)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer fake-sudo-file-identity" {
				t.Fatalf("expected sudo file identity, got %q", got)
			}
		},
		response: &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"runners":[]}`)),
			Header:     make(http.Header),
		},
	})
	client.readFile = func(string) ([]byte, error) { return nil, fs.ErrPermission }
	if _, err := client.ListRepoRunners(context.Background(), "me", "app"); err != nil {
		t.Fatal(err)
	}
	if cats != 1 {
		t.Fatalf("sudo reads=%d want=1", cats)
	}
}

func TestForProfileWithoutFilesUsesGlobalEnvFile(t *testing.T) {
	t.Setenv("TEST_GLOBAL_TOKEN", "")
	for _, tc := range []struct{ name, content, want string }{
		{name: "raw", content: "fake-global-file", want: "fake-global-file"},
		{name: "env", content: "TEST_GLOBAL_TOKEN=fake-global-file\n", want: "fake-global-file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "github.env")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/repos/me/app/actions/runners" || r.URL.RawQuery != "" {
					t.Errorf("request: %s %s", r.Method, r.URL)
				}
				if r.Header.Get("Authorization") != "Bearer "+tc.want {
					t.Errorf("wrong credential identity: %q", r.Header.Get("Authorization"))
				}
				fmt.Fprint(w, `{"runners":[]}`)
			}))
			defer srv.Close()
			c := NewClient(srv.URL, "TEST_GLOBAL_TOKEN", "", nil, srv.Client()).ForProfile(
				config.GitHubConfig{APIBaseURL: srv.URL, TokenEnv: "TEST_GLOBAL_TOKEN", EnvFile: path},
				config.GitHubProfile{},
			)
			if _, err := c.ListRepoRunners(context.Background(), "me", "app"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestForProfileWithoutFilesFallsBackToEnvWhenGlobalFileMissing(t *testing.T) {
	t.Setenv("TEST_GLOBAL_TOKEN", "fake-global-env")
	missing := filepath.Join(t.TempDir(), "missing.env")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/me/app/actions/runners" || r.URL.RawQuery != "" {
			t.Errorf("request: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer fake-global-env" {
			t.Errorf("wrong credential identity: %q", r.Header.Get("Authorization"))
		}
		fmt.Fprint(w, `{"runners":[]}`)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "TEST_GLOBAL_TOKEN", "", nil, srv.Client()).ForProfile(
		config.GitHubConfig{APIBaseURL: srv.URL, TokenEnv: "TEST_GLOBAL_TOKEN", EnvFile: missing},
		config.GitHubProfile{},
	)
	if _, err := c.ListRepoRunners(context.Background(), "me", "app"); err != nil {
		t.Fatal(err)
	}
}

func TestNewClientUsesCustomTokenEnv(t *testing.T) {
	t.Setenv("MY_CUSTOM_TOKEN", "custom-value")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/me/app/actions/runners" || r.URL.RawQuery != "" {
			t.Errorf("request: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer custom-value" {
			t.Errorf("wrong credential identity: %q", r.Header.Get("Authorization"))
		}
		fmt.Fprint(w, `{"runners":[]}`)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "MY_CUSTOM_TOKEN", "", nil, srv.Client())
	if _, err := c.ListRepoRunners(context.Background(), "me", "app"); err != nil {
		t.Fatal(err)
	}
}

func TestFindRunnerRepoUsesExactNameAndEncodesQuery(t *testing.T) {
	t.Setenv("TEST_GITHUB_TOKEN", "test-token")
	name := "runner+one"
	client := NewClient("https://example.test", "TEST_GITHUB_TOKEN", "", nil, fakeHTTPDoer{
		check: func(r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("method: %s", r.Method)
			}
			if r.URL.Path != "/repos/me/app/actions/runners" {
				t.Errorf("path: %s", r.URL.Path)
			}
			if r.URL.RawQuery != "name=runner%2Bone" {
				t.Errorf("query: %q", r.URL.RawQuery)
			}
		},
		response: &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(
				`{"runners":[{"id":1,"name":"runner+one","status":"online"},{"id":2,"name":"runner+one-extra","status":"online"}]}`,
			)),
			Header: make(http.Header),
		},
	})
	got, err := client.FindRunner(context.Background(), config.ResolvedTarget{Scope: config.TargetScopeRepository, Owner: "me", Repo: "app"}, name)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != 1 || got.Name != name {
		t.Fatalf("unexpected runner: %+v", got)
	}
}

func TestFindRunnerOrgUsesSlugAndExactName(t *testing.T) {
	t.Setenv("TEST_GITHUB_TOKEN", "test-token")
	client := NewClient("https://example.test", "TEST_GITHUB_TOKEN", "", nil, fakeHTTPDoer{
		check: func(r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("method: %s", r.Method)
			}
			if r.URL.Path != "/orgs/myorg/actions/runners" {
				t.Errorf("path: %s", r.URL.Path)
			}
			if r.URL.Query().Get("name") != "runner-one" {
				t.Errorf("query: %q", r.URL.RawQuery)
			}
		},
		response: &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"runners":[{"id":9,"name":"runner-one","status":"online"}]}`)),
			Header:     make(http.Header),
		},
	})
	got, err := client.FindRunner(context.Background(), config.ResolvedTarget{Scope: config.TargetScopeOrganization, Org: "MyOrg", OrgSlug: "myorg"}, "runner-one")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != 9 {
		t.Fatalf("unexpected runner: %+v", got)
	}
}

func TestFindRunnerDoesNotPrefixFallback(t *testing.T) {
	t.Setenv("TEST_GITHUB_TOKEN", "test-token")
	client := NewClient("https://example.test", "TEST_GITHUB_TOKEN", "", nil, fakeHTTPDoer{
		response: &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"runners":[{"id":2,"name":"runner-one-extra","status":"online"}]}`)),
			Header:     make(http.Header),
		},
	})
	got, err := client.FindRunner(context.Background(), config.ResolvedTarget{Scope: config.TargetScopeRepository, Owner: "me", Repo: "app"}, "runner-one")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("expected nil without exact name, got %+v", got)
	}
}

func TestDeleteRunnerRequiresNoContentAndTypedError(t *testing.T) {
	for _, scope := range []struct {
		name, path string
		del        func(Client) error
	}{
		{name: "repo", path: "/repos/me/app/actions/runners/7", del: func(c Client) error {
			return c.DeleteRunner(context.Background(), "me", "app", 7)
		}},
		{name: "org", path: "/orgs/myorg/actions/runners/7", del: func(c Client) error {
			return c.DeleteOrgRunner(context.Background(), "myorg", 7)
		}},
	} {
		for _, tc := range []struct {
			name    string
			status  int
			wantErr bool
		}{
			{name: "204", status: http.StatusNoContent},
			{name: "200", status: http.StatusOK, wantErr: true},
			{name: "403", status: http.StatusForbidden, wantErr: true},
			{name: "422", status: http.StatusUnprocessableEntity, wantErr: true},
			{name: "429", status: http.StatusTooManyRequests, wantErr: true},
			{name: "500", status: http.StatusInternalServerError, wantErr: true},
		} {
			t.Run(scope.name+"-"+tc.name, func(t *testing.T) {
				t.Setenv("TEST_GITHUB_TOKEN", "test-token")
				body := ""
				if tc.wantErr {
					body = `{"message":"fake-secret-token-value"}`
				}
				client := NewClient("https://example.test", "TEST_GITHUB_TOKEN", "", nil, fakeHTTPDoer{
					check: func(r *http.Request) {
						if r.Method != http.MethodDelete {
							t.Errorf("method: %s", r.Method)
						}
						if r.URL.Path != scope.path {
							t.Errorf("path: %s", r.URL.Path)
						}
						if r.URL.RawQuery != "" {
							t.Errorf("query: %s", r.URL.RawQuery)
						}
					},
					response: &http.Response{
						StatusCode: tc.status,
						Body:       io.NopCloser(strings.NewReader(body)),
						Header:     make(http.Header),
					},
				})
				err := scope.del(client)
				if tc.wantErr {
					if err == nil {
						t.Fatal("expected error")
					}
					var apiErr *APIError
					if !errors.As(err, &apiErr) {
						t.Fatalf("expected APIError, got %T: %v", err, err)
					}
					if apiErr.Method != http.MethodDelete || apiErr.Path != scope.path || apiErr.StatusCode != tc.status {
						t.Fatalf("unexpected APIError: %+v", apiErr)
					}
					if strings.Contains(err.Error(), "fake-secret-token-value") {
						t.Fatal("error leaked response body")
					}
				} else if err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestQueuedJobsBoundaryLabels(t *testing.T) {
	t.Setenv("TEST_QUEUE_TOKEN_BOUND", "fake-queue-token")
	var runs, jobs int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method: %s", r.Method)
		}
		if r.URL.Query().Get("per_page") != "100" {
			t.Error("per_page")
		}
		switch r.URL.Path {
		case "/repos/me/app/actions/runs":
			status := r.URL.Query().Get("status")
			if status != "queued" && status != "in_progress" {
				t.Errorf("status: %s", status)
			}
			runs++
			fmt.Fprint(w, `{"workflow_runs":[{"id":5}]}`)
		case "/repos/me/app/actions/runs/5/jobs":
			if r.URL.Query().Get("filter") != "latest" {
				t.Error("filter")
			}
			jobs++
			fmt.Fprint(w, `{"jobs":[
                {"id":1,"status":"queued","labels":["self-hosted"]},
                {"id":2,"status":"queued","labels":["linux"]},
                {"id":3,"status":"queued","labels":[]}]}`)
		default:
			t.Errorf("path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "TEST_QUEUE_TOKEN_BOUND", "", nil, srv.Client())
	got, err := c.QueuedJobs(context.Background(), []Repository{{Owner: "me", Name: "app"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("jobs=%v", got)
	}
	if runs != 2 || jobs != 1 {
		t.Fatalf("runs=%d jobs=%d", runs, jobs)
	}
}

func TestQueuedJobsRequestErrorReturnsNilJobs(t *testing.T) {
	t.Setenv("TEST_QUEUE_TOKEN_ERR", "fake-queue-token")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"fake-secret-token-value"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "TEST_QUEUE_TOKEN_ERR", "", nil, srv.Client())
	got, err := c.QueuedJobs(context.Background(), []Repository{{Owner: "me", Name: "app"}}, []string{"linux"})
	if err == nil {
		t.Fatal("expected error")
	}
	if got != nil {
		t.Fatalf("expected nil jobs on error, got %v", got)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %T: %v", err, err)
	}
	if strings.Contains(err.Error(), "fake-secret-token-value") {
		t.Fatal("error leaked response body")
	}
}
