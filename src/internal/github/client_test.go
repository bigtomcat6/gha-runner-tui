package github

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

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
			if r.URL.Path != "/orgs/example-org/actions/runners" {
				t.Fatalf("unexpected path: %s", r.URL.Path)
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
			if r.URL.Path != "/orgs/example-org/actions/runner-groups" {
				t.Fatalf("unexpected path: %s", r.URL.Path)
			}
		},
		response: &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(
				`{"runner_groups":[{"id":42,"name":"example-org-swift","visibility":"all"}]}`,
			)),
			Header: make(http.Header),
		},
	})

	groups, err := client.ListOrgRunnerGroups(context.Background(), "example-org")
	if err != nil {
		t.Fatalf("ListOrgRunnerGroups returned error: %v", err)
	}
	if len(groups) != 1 || groups[0].ID != 42 || groups[0].Visibility != "all" {
		t.Fatalf("unexpected groups: %+v", groups)
	}
}

func TestCreateOrgRunnerGroupSendsVisibilityAll(t *testing.T) {
	t.Setenv("TEST_GITHUB_TOKEN", "test-token")
	client := NewClient("https://example.test", "TEST_GITHUB_TOKEN", "", nil, fakeHTTPDoer{
		check: func(r *http.Request) {
			if r.Method != http.MethodPost {
				t.Fatalf("unexpected method: %s", r.Method)
			}
			if r.URL.Path != "/orgs/example-org/actions/runner-groups" {
				t.Fatalf("unexpected path: %s", r.URL.Path)
			}
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"name":"example-org-swift"`) {
				t.Fatalf("missing group name in body: %s", string(body))
			}
			if !strings.Contains(string(body), `"visibility":"all"`) {
				t.Fatalf("missing visibility in body: %s", string(body))
			}
		},
		response: &http.Response{
			StatusCode: http.StatusCreated,
			Body: io.NopCloser(strings.NewReader(
				`{"id":42,"name":"example-org-swift","visibility":"all"}`,
			)),
			Header: make(http.Header),
		},
	})

	group, err := client.CreateOrgRunnerGroup(context.Background(), "example-org", "example-org-swift", "all")
	if err != nil {
		t.Fatalf("CreateOrgRunnerGroup returned error: %v", err)
	}
	if group.ID != 42 {
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
