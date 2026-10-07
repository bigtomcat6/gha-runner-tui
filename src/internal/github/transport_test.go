package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gha-runner-tui/internal/config"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func reply(status int, body string, headers http.Header) *http.Response {
	if headers == nil {
		headers = make(http.Header)
	}
	return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body))}
}
func testRef() config.CredentialRef {
	return config.CredentialRef{ID: "personal", ResourceOwner: "alice", TokenEnv: "TOKEN", Repositories: []config.RepoRef{{Owner: "alice", Name: "repo", ID: 1}}}
}
func testIO(token string) CredentialIO {
	return CredentialIO{LookupEnv: func(string) (string, bool) { return token, token != "" }, ReadFile: func(string) ([]byte, error) { return nil, errors.New("denied") }}
}
func scoped(t *testing.T, ref config.CredentialRef, sources CredentialIO, doer HTTPDoer, book *RateBook, now func() time.Time) Client {
	t.Helper()
	c, err := NewScopedClient("https://example.test", ref, sources, doer, book, now)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRateIdentityIsolation(t *testing.T) {
	book := &RateBook{}
	for _, token := range []string{"private-A", "private-B"} {
		calls := 0
		c := scoped(t, testRef(), testIO(token), doerFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("X-GitHub-Api-Version") != "2026-03-10" {
				t.Fatal("wrong identity/version headers")
			}
			if r.URL.String() != "https://example.test/repos/alice/repo/actions/runners?page=1" {
				t.Fatal("wrong URL/query")
			}
			if calls == 1 {
				if r.Header.Get("If-None-Match") != "" {
					t.Fatal("another identity's ETag reused")
				}
				return reply(200, `{"name":"`+token+`"}`, http.Header{"Etag": []string{token}}), nil
			}
			if r.Header.Get("If-None-Match") != token {
				t.Fatal("own ETag not reused")
			}
			return reply(304, "", nil), nil
		}), book, nil)
		for range 2 {
			var out struct{ Name string }
			if err := c.requestJSON(context.Background(), "GET", "/repos/alice/repo/actions/runners?page=1", nil, &out); err != nil || out.Name != token {
				t.Fatalf("cross identity cache: %+v %v", out, err)
			}
		}
	}
	c := scoped(t, testRef(), testIO("private-C"), doerFunc(func(*http.Request) (*http.Response, error) { return reply(304, "", nil), nil }), book, nil)
	var out any
	if err := c.requestJSON(context.Background(), "GET", "/repos/alice/repo/actions/runners?page=1", nil, &out); err == nil {
		t.Fatal("304 reused another identity's body")
	}
}

func TestAuthRotationInvalidates(t *testing.T) {
	for _, change := range []string{"token", "source", "owner", "scope", "ref", "read-error"} {
		t.Run(change, func(t *testing.T) {
			r := testRef()
			token := "snapshot"
			calls := 0
			if change == "source" || change == "read-error" {
				r.TokenFile = "/token"
			}
			sources := CredentialIO{LookupEnv: func(string) (string, bool) { return token, token != "" }, ReadFile: func(string) ([]byte, error) {
				if change == "read-error" {
					return nil, errors.New("snapshot")
				}
				return []byte("snapshot"), nil
			}}
			c := scoped(t, r, sources, doerFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return reply(200, `{"runners":[]}`, http.Header{"Etag": []string{"frozen"}}), nil
			}), nil, nil)
			copyClient := c
			if err := c.CheckCurrent(r, sources); err != nil {
				t.Fatal("unchanged snapshot invalidated")
			}
			if _, err := c.ListRepoRunners(context.Background(), "alice", "repo"); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "token":
				token = "new"
			case "source":
				token = ""
			case "owner":
				r.ResourceOwner = "bob"
			case "scope":
				r.Repositories = append([]config.RepoRef(nil), r.Repositories...)
				r.Repositories[0].Name = "other"
			case "ref":
				r.ID = "other"
			case "read-error":
				token = ""
			}
			if err := c.CheckCurrent(r, sources); !errors.Is(err, ErrAuthChanged) {
				t.Fatalf("rotation not detected: %v", err)
			}
			if _, err := copyClient.ListRepoRunners(context.Background(), "alice", "repo"); !errors.Is(err, ErrAuthChanged) {
				t.Fatalf("copy allowed stale request: %v", err)
			}
			if calls != 1 {
				t.Fatal("invalid client called HTTP")
			}
		})
	}
	a := scoped(t, testRef(), testIO("same"), doerFunc(func(*http.Request) (*http.Response, error) { return reply(200, `{}`, nil), nil }), nil, nil)
	b := scoped(t, testRef(), testIO("same"), doerFunc(func(*http.Request) (*http.Response, error) { return reply(304, "", nil), nil }), nil, nil)
	if a.Auth().Generation == "" || a.Auth() == b.Auth() {
		t.Fatal("restart/client construction reused generation")
	}
	var out any
	if err := a.requestJSON(context.Background(), "GET", "/page?page=1", nil, &out); err != nil {
		t.Fatal(err)
	}
	if err := b.requestJSON(context.Background(), "GET", "/page?page=1", nil, &out); err == nil {
		t.Fatal("new generation reused old cache")
	}
	r := testRef()
	c := scoped(t, r, testIO("same"), nil, nil, nil)
	r.Repositories[0].Name = "changed"
	if err := c.CheckCurrent(testRef(), testIO("same")); err != nil {
		t.Fatal("snapshot aliases caller repositories")
	}
}

func TestWriteProofRequiresCurrentAuth(t *testing.T) {
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	calls := 0
	c := scoped(t, testRef(), testIO("same"), doerFunc(func(*http.Request) (*http.Response, error) { calls++; return reply(200, `{"runners":[]}`, nil), nil }), nil, func() time.Time { return now })
	if _, err := c.ListRepoRunners(context.Background(), "alice", "repo"); err != nil {
		t.Fatal(err)
	}
	target := config.ResolvedTarget{Scope: config.TargetScopeRepository, Owner: "alice", Repo: "repo"}
	for _, target := range []config.ResolvedTarget{target, {Scope: config.TargetScopeRepository, Owner: "alice", Repo: "wrong"}} {
		for _, scope := range []string{"scope", "wrong", ""} {
			proof, err := c.ManagementWriteProof(context.Background(), target, scope)
			if err == nil || !reflect.DeepEqual(proof, WriteProof{}) {
				t.Fatal("GET or absent external evidence authorized write")
			}
		}
	}
	if calls != 1 {
		t.Fatal("proof performed unauthorized permission experiment")
	}
	if !c.WriteRateReady(now) {
		t.Fatal("unlimited rate window not ready")
	}
	if err := c.CheckCurrent(testRef(), testIO("rotated")); !errors.Is(err, ErrAuthChanged) {
		t.Fatal(err)
	}
	if _, err := c.ManagementWriteProof(context.Background(), target, "scope"); !errors.Is(err, ErrAuthChanged) {
		t.Fatal("old generation not rejected")
	}
	if c.WriteRateReady(now) {
		t.Fatal("invalid transport write ready")
	}
}

func TestRateBackoffAndNoWriteRetry(t *testing.T) {
	epoch := time.Unix(1000000, 0).UTC()
	for _, tc := range []struct {
		name    string
		headers http.Header
		body    string
		delay   time.Duration
	}{
		{"retry-after", http.Header{"Retry-After": []string{"120"}}, `{}`, 120 * time.Second},
		{"retry-after-date", http.Header{"Retry-After": []string{epoch.Add(180 * time.Second).Format(http.TimeFormat)}}, `{}`, 180 * time.Second},
		{"primary-reset", http.Header{"X-Ratelimit-Remaining": []string{"0"}, "X-Ratelimit-Reset": []string{strconv.FormatInt(epoch.Add(300*time.Second).Unix(), 10)}}, `{}`, 300 * time.Second},
		{"secondary", nil, `{"message":"secondary rate limit"}`, time.Minute},
		{"negative-retry", http.Header{"Retry-After": []string{"-1"}}, `{"message":"secondary rate limit"}`, time.Minute},
		{"short-secondary-retry", http.Header{"Retry-After": []string{"1"}}, `{"message":"secondary rate limit"}`, time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			book := &RateBook{}
			now := epoch
			calls := 0
			c := scoped(t, testRef(), testIO("same"), doerFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "POST" {
					t.Fatal("wrong write method")
				}
				return reply(403, tc.body, tc.headers), nil
			}), book, func() time.Time { return now })
			if _, err := c.CreateScopedRegistrationToken(context.Background(), config.ResolvedTarget{Scope: config.TargetScopeRepository, Owner: "alice", Repo: "repo"}); err == nil {
				t.Fatal("rate error ignored")
			}
			if calls != 1 || c.WriteRateReady(epoch.Add(tc.delay-time.Second)) {
				t.Fatal("automatic write retry or too-short backoff")
			}
			ref := testRef()
			ref.ID = "another-ref"
			other := scoped(t, ref, testIO("same"), doerFunc(func(*http.Request) (*http.Response, error) { t.Fatal("same quota bypassed"); return nil, nil }), book, func() time.Time { return epoch })
			if other.WriteRateReady(epoch) {
				t.Fatal("ref rotation bypassed quota")
			}
			if err := other.DeleteRunner(context.Background(), "alice", "repo", 1); err == nil {
				t.Fatal("DELETE ignored rate window")
			}
			for _, e := range book.Entries {
				if e.Next.Before(epoch.Add(tc.delay)) || e.Failures != 1 {
					t.Fatalf("server lower bound ignored: %+v", e)
				}
			}
		})
	}
	book := &RateBook{}
	now := epoch
	calls := 0
	c := scoped(t, testRef(), testIO("same"), doerFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return reply(403, `{"message":"secondary rate limit"}`, nil), nil
	}), book, func() time.Time { return now })
	for i, delay := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute} {
		start := now
		if _, err := c.ListRepoRunners(context.Background(), "alice", "repo"); err == nil {
			t.Fatal("rate response ignored")
		}
		for _, entry := range book.Entries {
			if entry.Next.Before(start.Add(delay)) || entry.Failures != i+1 || entry.Isolated != (i == 4) {
				t.Fatalf("bad secondary backoff: %+v", entry)
			}
			now = entry.Next
		}
	}
	if c.WriteRateReady(now.Add(24 * time.Hour)) {
		t.Fatal("isolated credential became ready")
	}
	if _, err := c.ListRepoRunners(context.Background(), "alice", "repo"); err == nil || calls != 5 {
		t.Fatal("isolation not enforced")
	}
}

func TestTransportTimeoutOriginAndSafeErrors(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		status     int
		body       string
		headers    http.Header
	}{
		{"external-url", "https://evil.test/page", 200, `{}`, nil},
		{"external-link", "/page", 200, `{}`, http.Header{"Link": []string{`<https://evil.test/page>; rel="next"`}}},
		{"redirect", "/page", 302, `{}`, http.Header{"Location": []string{"https://evil.test/page"}}},
		{"raw-body", "/page", 400, `Bearer fake-PAT private-body`, nil},
		{"decode", "/page", 200, `fake-PAT private-body`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := scoped(t, testRef(), testIO("fake-PAT"), doerFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				deadline, ok := r.Context().Deadline()
				remaining := time.Until(deadline)
				if !ok || remaining <= 0 || remaining > 10*time.Second {
					t.Fatal("request not bounded by ten seconds")
				}
				return reply(tc.status, tc.body, tc.headers), nil
			}), nil, nil)
			var out any
			err := c.requestJSON(context.Background(), "GET", tc.path, nil, &out)
			if err == nil || strings.Contains(err.Error(), "fake-PAT") || strings.Contains(err.Error(), "private-body") {
				t.Fatalf("unsafe error: %v", err)
			}
			if tc.name == "external-url" && calls != 0 {
				t.Fatal("sent credential to untrusted URL")
			}
		})
	}
	c := scoped(t, testRef(), testIO("fake-PAT"), doerFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("Bearer fake-PAT private-body") }), nil, nil)
	if _, err := c.ListRepoRunners(context.Background(), "alice", "repo"); err == nil || strings.Contains(err.Error(), "fake-PAT") {
		t.Fatalf("unsafe doer error %v", err)
	}
	for _, external := range []bool{false, true} {
		calls := 0
		rawClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				location := "/second"
				if external {
					location = "https://evil.test/second"
				}
				return reply(302, "", http.Header{"Location": []string{location}}), nil
			}
			if r.URL.Host != "example.test" {
				t.Fatal("redirect leaked credential")
			}
			return reply(200, `{"runners":[]}`, nil), nil
		})}
		c := scoped(t, testRef(), testIO("fake-PAT"), rawClient, nil, nil)
		_, err := c.ListRepoRunners(context.Background(), "alice", "repo")
		if external && (err == nil || calls != 1) {
			t.Fatal("external redirect followed")
		}
		if !external && (err != nil || calls != 2) {
			t.Fatalf("trusted redirect failed: %v", err)
		}
		if rawClient.CheckRedirect != nil {
			t.Fatal("mutated injected HTTP client")
		}
	}
}

func TestScopedConstructorFailClosed(t *testing.T) {
	if _, err := NewScopedClient("https://example.test", testRef(), testIO(""), nil, nil, nil); err == nil {
		t.Fatal("resolver failure ignored")
	}
	if _, err := newScopedClient("https://example.test", testRef(), testIO("test"), nil, nil, nil, strings.NewReader("")); err == nil {
		t.Fatal("random failure ignored")
	}
	for _, base := range []string{"https://user:password@example.test", "file:///tmp/api", "https://example.test/?secret", "https://example.test/#fragment"} {
		if _, err := NewScopedClient(base, testRef(), testIO("test"), nil, nil, nil); err == nil {
			t.Fatalf("unsafe API base accepted %s", base)
		}
	}
}

func TestTransportPreservesAPIBasePathAndQueryCache(t *testing.T) {
	calls := 0
	c, err := NewScopedClient("https://example.test/api/v3/", testRef(), testIO("fake"), doerFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		want := "https://example.test/api/v3/runners?page=" + strconv.Itoa(calls)
		if r.URL.String() != want {
			t.Errorf("want %s got %s", want, r.URL.String())
		}
		if r.Header.Get("If-None-Match") != "" {
			t.Error("cache crossed query/page boundary")
		}
		return reply(200, `{}`, http.Header{"Etag": []string{"page"}, "Link": []string{`<https://example.test/api/v3/runners?page=2>; rel="next"`}}), nil
	}), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	auth := c.Auth()
	for _, path := range []string{"/runners?page=1", "/runners?page=2"} {
		var out any
		if err := c.requestJSON(context.Background(), "GET", path, nil, &out); err != nil {
			t.Fatal(err)
		}
		if c.Auth() != auth {
			t.Fatal("pagination changed identity")
		}
	}
}

func TestCanceledTransportDoesNotRequest(t *testing.T) {
	c := scoped(t, testRef(), testIO("fake"), doerFunc(func(*http.Request) (*http.Response, error) {
		t.Error("canceled request reached HTTP")
		return reply(200, `{"runners":[]}`, nil), nil
	}), nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.ListRepoRunners(ctx, "alice", "repo"); err == nil {
		t.Fatal("canceled request succeeded")
	}
}

func TestSharedRateBookSerializesRequests(t *testing.T) {
	book := &RateBook{}
	var active atomic.Int32
	var peak atomic.Int32
	doer := doerFunc(func(*http.Request) (*http.Response, error) {
		n := active.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		for range 20 {
			runtime.Gosched()
		}
		active.Add(-1)
		return reply(200, `{"runners":[]}`, nil), nil
	})
	clients := []Client{scoped(t, testRef(), testIO("one"), doer, book, nil), scoped(t, testRef(), testIO("two"), doer, book, nil)}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := clients[i%2].ListRepoRunners(context.Background(), "alice", "repo"); err != nil {
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if peak.Load() != 1 {
		t.Fatalf("shared API requests overlapped: peak=%d", peak.Load())
	}
}

func TestScopedRegistrationTokenRetainsExpiryAndIsNotCached(t *testing.T) {
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	for _, org := range []bool{false, true} {
		for _, expired := range []bool{false, true} {
			calls := 0
			target := config.ResolvedTarget{Scope: config.TargetScopeRepository, Owner: "alice", Repo: "repo"}
			path := "/repos/alice/repo/actions/runners/registration-token"
			if org {
				target = config.ResolvedTarget{Scope: config.TargetScopeOrganization, Org: "Alice", OrgSlug: "alice"}
				path = "/orgs/alice/actions/runners/registration-token"
			}
			expiry := "2026-10-06T01:00:00Z"
			if expired {
				expiry = "2026-10-05T23:00:00Z"
			}
			c := scoped(t, testRef(), testIO("fake-PAT"), doerFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				body, _ := io.ReadAll(r.Body)
				if r.Method != "POST" || r.URL.Path != path || string(body) != "{}" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("If-None-Match") != "" {
					t.Fatal("incorrect registration request")
				}
				return reply(201, `{"token":"fake-registration","expires_at":"`+expiry+`"}`, http.Header{"Etag": []string{"never-cache"}}), nil
			}), nil, func() time.Time { return now })
			for range 2 {
				token, err := c.CreateScopedRegistrationToken(context.Background(), target)
				if (err != nil) != expired {
					t.Fatalf("expiry rejection %v", err)
				}
				if !expired && (string(token.Bytes()) != "fake-registration" || !token.ExpiresAt.Equal(now.Add(time.Hour))) {
					t.Fatal("expiry/value lost")
				}
				token.Clear()
			}
			if calls != 2 || len(c.transport.cache) != 0 {
				t.Fatal("registration token entered cache")
			}
		}
	}
}

func TestSecondaryHeaderOnlyMinimumAndSharedOwnerQuota(t *testing.T) {
	for _, status := range []int{403, 429} {
		now := time.Unix(1000000, 0)
		book := &RateBook{}
		c := scoped(t, testRef(), testIO("same"), doerFunc(func(*http.Request) (*http.Response, error) {
			return reply(status, `{}`, http.Header{"Retry-After": []string{"1"}}), nil
		}), book, func() time.Time { return now })
		_, _ = c.ListRepoRunners(context.Background(), "alice", "repo")
		if c.WriteRateReady(now.Add(59 * time.Second)) {
			t.Error("header-only secondary response backed off less than sixty seconds")
		}
		ref := testRef()
		ref.ID = "other"
		ref.ResourceOwner = "bob"
		other := scoped(t, ref, testIO("same"), nil, book, func() time.Time { return now })
		if other.WriteRateReady(now.Add(59 * time.Second)) {
			t.Error("same token changed owner to bypass quota")
		}
	}
}

func TestNotModifiedCannotIntroduceUntrustedLink(t *testing.T) {
	calls := 0
	c := scoped(t, testRef(), testIO("same"), doerFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return reply(200, `{"runners":[]}`, http.Header{"Etag": []string{"private"}}), nil
		}
		return reply(304, "", http.Header{"Link": []string{`<https://evil.test/page>; rel="next"`}}), nil
	}), nil, nil)
	if _, err := c.ListRepoRunners(context.Background(), "alice", "repo"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListRepoRunners(context.Background(), "alice", "repo"); err == nil {
		t.Fatal("304 accepted untrusted pagination metadata")
	}
}

func TestMutatingRedirectMakesOneNetworkAttempt(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		for _, method := range []string{"POST", "PATCH", "DELETE"} {
			t.Run(strconv.Itoa(status)+"/"+method, func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.Header.Get("Authorization") != "Bearer fake-PAT" {
						t.Error("missing authentication")
					}
					if r.URL.Path == "/first" {
						http.Redirect(w, r, "/second", status)
						return
					}
					w.WriteHeader(http.StatusNoContent)
				}))
				defer server.Close()
				c, err := NewScopedClient(server.URL, testRef(), testIO("fake-PAT"), server.Client(), nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				err = c.requestJSON(context.Background(), method, "/first", map[string]any{}, nil)
				if calls.Load() != 1 || err == nil {
					t.Fatalf("mutating redirect: network attempts=%d error=%v", calls.Load(), err)
				}
			})
		}
	}
}

type rateReadProbe struct {
	read func([]byte) (int, error)
}

func (p rateReadProbe) Read(b []byte) (int, error) { return p.read(b) }
func (p rateReadProbe) Close() error               { return nil }

func TestUnreadableRateResponseRetainsHeaderBounds(t *testing.T) {
	for _, kind := range []string{"read-error", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Unix(1000000, 0)
			book := &RateBook{}
			calls := 0
			reader := strings.NewReader(strings.Repeat("x", 16*1024*1024+1))
			checked := false
			c := scoped(t, testRef(), testIO("same"), doerFunc(func(*http.Request) (*http.Response, error) {
				calls++
				resp := reply(429, "", http.Header{"Retry-After": []string{"120"}})
				resp.Body = rateReadProbe{read: func(b []byte) (int, error) {
					book.mu.Lock()
					entry := book.Entries["https://example.test|core"]
					book.mu.Unlock()
					if !checked && entry.Next.Before(now.Add(120*time.Second)) {
						t.Error("header backoff absent before body read")
					}
					checked = true
					if kind == "read-error" {
						return 0, errors.New("private-body")
					}
					return reader.Read(b)
				}}
				return resp, nil
			}), book, func() time.Time { return now })
			if err := c.DeleteRunner(context.Background(), "alice", "repo", 1); err == nil || strings.Contains(err.Error(), "private-body") {
				t.Fatalf("unreadable response must fail safely: %v", err)
			}
			if c.WriteRateReady(now.Add(119 * time.Second)) {
				t.Error("unreadable 429 lost Retry-After")
			}
			if err := c.DeleteRunner(context.Background(), "alice", "repo", 1); !errors.Is(err, ErrRateLimited) || calls != 1 {
				t.Fatalf("rate window bypass: attempts=%d error=%v", calls, err)
			}
			entry := book.Entries["https://example.test|core"]
			if entry.Failures != 1 {
				t.Errorf("one response counted %d times", entry.Failures)
			}
		})
	}
}

func TestCombinedRateBounds(t *testing.T) {
	for _, status := range []int{403, 429} {
		for _, resetDelay := range []time.Duration{5 * time.Second, 180 * time.Second} {
			t.Run(strconv.Itoa(status)+"/"+resetDelay.String(), func(t *testing.T) {
				now := time.Unix(1000000, 0)
				book := &RateBook{}
				c := scoped(t, testRef(), testIO("same"), doerFunc(func(*http.Request) (*http.Response, error) {
					return reply(status, `{"message":"secondary rate limit"}`, http.Header{
						"Retry-After": []string{"1"}, "X-Ratelimit-Remaining": []string{"0"},
						"X-Ratelimit-Reset": []string{strconv.FormatInt(now.Add(resetDelay).Unix(), 10)},
					}), nil
				}), book, func() time.Time { return now })
				for i := range 2 {
					start := now
					_, _ = c.ListRepoRunners(context.Background(), "alice", "repo")
					want := start.Add(max(time.Minute*time.Duration(1<<i), resetDelay))
					entry := book.Entries["https://example.test|core"]
					if entry.Next.Before(want) || c.WriteRateReady(want.Add(-time.Nanosecond)) {
						t.Errorf("combined bounds shortened: got %v want >= %v", entry.Next, want)
					}
					if entry.Failures != i+1 {
						t.Errorf("one response counted more than once: %+v", entry)
					}
					now = entry.Next
				}
			})
		}
	}
}

func TestRateRecoveryResetsConsecutiveFailures(t *testing.T) {
	now := time.Unix(1000000, 0)
	book := &RateBook{}
	limited := true
	c := scoped(t, testRef(), testIO("same"), doerFunc(func(*http.Request) (*http.Response, error) {
		if limited {
			return reply(429, `{}`, nil), nil
		}
		return reply(200, `{"runners":[]}`, nil), nil
	}), book, func() time.Time { return now })
	for i := range 6 {
		limited = true
		if _, err := c.ListRepoRunners(context.Background(), "alice", "repo"); err == nil {
			t.Fatal("rate response accepted")
		}
		entry := book.Entries["https://example.test|core"]
		if entry.Failures != 1 || entry.Isolated {
			t.Fatalf("recovered incidents accumulated at incident %d: %+v", i+1, entry)
		}
		now = entry.Next
		limited = false
		if _, err := c.ListRepoRunners(context.Background(), "alice", "repo"); err != nil {
			t.Fatal(err)
		}
		got := book.Entries["https://example.test|core"]
		if got.Failures != 0 || got.Isolated || got.Next != entry.Next {
			t.Fatalf("confirmed recovery did not reset streak/preserve bound: %+v", got)
		}
	}
}

func TestRateRecoveryPreservesActiveBoundsAndResourceIsolation(t *testing.T) {
	now := time.Unix(1000000, 0)
	book := &RateBook{}
	core := RateEntry{Next: now.Add(time.Minute), Failures: 3}
	actions := RateEntry{Next: now, Failures: 2}
	isolated := RateEntry{Next: now, Failures: 5, Isolated: true}
	c := scoped(t, testRef(), testIO("same"), doerFunc(func(*http.Request) (*http.Response, error) {
		// Model a bound learned while this response was in flight. A success
		// must not erase that bound, another resource's streak, or isolation.
		book.mu.Lock()
		book.Entries["https://example.test|core"] = core
		book.Entries["https://example.test|actions"] = actions
		book.Entries["https://example.test|isolated"] = isolated
		book.mu.Unlock()
		return reply(200, `{"runners":[]}`, nil), nil
	}), book, func() time.Time { return now })
	if _, err := c.ListRepoRunners(context.Background(), "alice", "repo"); err != nil {
		t.Fatal(err)
	}
	if book.Entries["https://example.test|core"] != core || book.Entries["https://example.test|actions"] != actions || book.Entries["https://example.test|isolated"] != isolated {
		t.Fatal("success changed active/other-resource/isolated state")
	}
	if c.WriteRateReady(now.Add(24 * time.Hour)) {
		t.Fatal("success cleared isolation")
	}
}

func TestOnlyValidatedSuccessConfirmsRateRecovery(t *testing.T) {
	for _, kind := range []string{"read-error", "external-link", "uncached-304", "cached-304", "200"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Unix(1000000, 0)
			book := &RateBook{}
			calls := 0
			c := scoped(t, testRef(), testIO("same"), doerFunc(func(*http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return reply(200, `{"runners":[]}`, http.Header{"Etag": []string{"cached"}}), nil
				}
				resp := reply(200, `{"runners":[]}`, nil)
				switch kind {
				case "read-error":
					resp.Body = rateReadProbe{read: func([]byte) (int, error) { return 0, errors.New("unreadable") }}
				case "external-link":
					resp.Header.Set("Link", `<https://evil.test/page>; rel="next"`)
				case "uncached-304", "cached-304":
					resp.StatusCode = 304
				}
				return resp, nil
			}), book, func() time.Time { return now })
			if _, err := c.ListRepoRunners(context.Background(), "alice", "repo"); err != nil {
				t.Fatal(err)
			}
			before := RateEntry{Next: now, Failures: 3}
			book.Entries["https://example.test|core"] = before
			path := "/repos/alice/repo/actions/runners?page=1&per_page=100"
			if kind == "uncached-304" {
				path = "/repos/alice/repo/actions/runners?page=2&per_page=100"
			}
			var out any
			err := c.requestJSON(context.Background(), "GET", path, nil, &out)
			valid := kind == "200" || kind == "cached-304"
			if (err == nil) != valid {
				t.Fatalf("unexpected response validation: %v", err)
			}
			got := book.Entries["https://example.test|core"]
			want := before
			if valid {
				want.Failures = 0
			}
			if got != want {
				t.Fatalf("recovery state: got %+v want %+v", got, want)
			}
		})
	}
}
