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
