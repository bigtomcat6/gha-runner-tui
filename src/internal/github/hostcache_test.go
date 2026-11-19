package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// etagServer serves one runners payload with a fixed ETag and answers a
// matching If-None-Match with 304, recording what each request sent.
type etagServer struct {
	mu          sync.Mutex
	ifNoneMatch []string
	auth        []string
	status      int
}

func (e *etagServer) handler(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	e.ifNoneMatch = append(e.ifNoneMatch, r.Header.Get("If-None-Match"))
	e.auth = append(e.auth, r.Header.Get("Authorization"))
	status := e.status
	e.mu.Unlock()
	if status != 0 {
		http.Error(w, `{"message":"fail"}`, status)
		return
	}
	if r.Header.Get("If-None-Match") == `W/"v1"` {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", `W/"v1"`)
	fmt.Fprint(w, `{"runners":[{"id":7,"name":"r7","status":"online","busy":true}]}`)
}

func (e *etagServer) sent() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.ifNoneMatch...)
}

func newCachedClient(t *testing.T, url, tokenEnv, dir string, doer HTTPDoer) Client {
	t.Helper()
	return NewClient(url, tokenEnv, "", nil, doer).WithHostCache(dir)
}

func TestConditionalGetReusesCachedBodyOn304(t *testing.T) {
	t.Setenv("TEST_ETAG_TOKEN", "fake-etag-token")
	es := &etagServer{}
	srv := httptest.NewServer(http.HandlerFunc(es.handler))
	defer srv.Close()
	c := newCachedClient(t, srv.URL, "TEST_ETAG_TOKEN", t.TempDir(), srv.Client())

	for i := 0; i < 2; i++ {
		runners, err := c.ListRepoRunners(context.Background(), "me", "app")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if len(runners) != 1 || runners[0].ID != 7 || !runners[0].Busy {
			t.Fatalf("call %d: runners=%+v", i, runners)
		}
	}
	if got := es.sent(); len(got) != 2 || got[0] != "" || got[1] != `W/"v1"` {
		t.Fatalf("If-None-Match sent = %q", got)
	}
}

func TestConditionalCacheIsSharedByProcessesButNotIdentities(t *testing.T) {
	t.Setenv("TEST_ETAG_A", "fake-token-a")
	t.Setenv("TEST_ETAG_B", "fake-token-b")
	es := &etagServer{}
	srv := httptest.NewServer(http.HandlerFunc(es.handler))
	defer srv.Close()
	dir := t.TempDir()

	// Separate Client values stand in for separate loop processes.
	if _, err := newCachedClient(t, srv.URL, "TEST_ETAG_A", dir, srv.Client()).ListRepoRunners(context.Background(), "me", "app"); err != nil {
		t.Fatal(err)
	}
	if _, err := newCachedClient(t, srv.URL, "TEST_ETAG_A", dir, srv.Client()).ListRepoRunners(context.Background(), "me", "app"); err != nil {
		t.Fatal(err)
	}
	if _, err := newCachedClient(t, srv.URL, "TEST_ETAG_B", dir, srv.Client()).ListRepoRunners(context.Background(), "me", "app"); err != nil {
		t.Fatal(err)
	}
	got := es.sent()
	if len(got) != 3 || got[0] != "" || got[1] != `W/"v1"` || got[2] != "" {
		t.Fatalf("If-None-Match sent = %q (second process must revalidate, other identity must not)", got)
	}

	// Cache files never contain the token.
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(data), "fake-token-a") || strings.Contains(string(data), "fake-token-b") {
			t.Errorf("%s contains a token", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestConditionalCacheIgnoresFailures(t *testing.T) {
	t.Setenv("TEST_ETAG_FAIL", "fake-etag-token")
	es := &etagServer{status: http.StatusInternalServerError}
	srv := httptest.NewServer(http.HandlerFunc(es.handler))
	defer srv.Close()
	c := newCachedClient(t, srv.URL, "TEST_ETAG_FAIL", t.TempDir(), srv.Client())

	_, err := c.ListRepoRunners(context.Background(), "me", "app")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("err = %v", err)
	}
	es.mu.Lock()
	es.status = 0
	es.mu.Unlock()
	if _, err := c.ListRepoRunners(context.Background(), "me", "app"); err != nil {
		t.Fatal(err)
	}
	if got := es.sent(); len(got) != 2 || got[1] != "" {
		t.Fatalf("a failed response must not be cached; If-None-Match sent = %q", got)
	}
}

func TestUnexpectedNotModifiedIsAnError(t *testing.T) {
	t.Setenv("TEST_ETAG_304", "fake-etag-token")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()
	for _, dir := range []string{"", t.TempDir()} {
		c := NewClient(srv.URL, "TEST_ETAG_304", "", nil, srv.Client()).WithHostCache(dir)
		_, err := c.ListRepoRunners(context.Background(), "me", "app")
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotModified {
			t.Fatalf("dir=%q err = %v", dir, err)
		}
	}
}

// queueServer serves one pending run with jobs for two label sets and counts
// requests per endpoint.
type queueServer struct {
	mu   sync.Mutex
	runs int
	jobs int
	fail bool
}

func (q *queueServer) handler(w http.ResponseWriter, r *http.Request) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.fail {
		http.Error(w, `{"message":"fail"}`, http.StatusForbidden)
		return
	}
	switch r.URL.Path {
	case "/repos/me/app/actions/runs":
		q.runs++
		if r.URL.Query().Get("status") == "pending" {
			fmt.Fprint(w, `{"workflow_runs":[{"id":9}]}`)
		} else {
			fmt.Fprint(w, `{"workflow_runs":[]}`)
		}
	case "/repos/me/app/actions/runs/9/jobs":
		q.jobs++
		fmt.Fprint(w, `{"jobs":[
			{"id":1,"status":"queued","labels":["self-hosted","linux","release"]},
			{"id":2,"status":"queued","labels":["self-hosted","linux","ci"]},
			{"id":3,"status":"in_progress","labels":["self-hosted","linux","ci"]}]}`)
	default:
		http.NotFound(w, r)
	}
}

func (q *queueServer) counts() (int, int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.runs, q.jobs
}

func TestQueuedJobsSnapshotIsSharedWithinTTL(t *testing.T) {
	t.Setenv("TEST_SHARED_QUEUE", "fake-queue-token")
	qs := &queueServer{}
	srv := httptest.NewServer(http.HandlerFunc(qs.handler))
	defer srv.Close()
	dir := t.TempDir()
	now := time.Date(2026, 11, 19, 14, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	repos := []Repository{{Owner: "me", Name: "app"}}

	release := newCachedClient(t, srv.URL, "TEST_SHARED_QUEUE", dir, srv.Client())
	release.cache.now = clock
	ci := newCachedClient(t, srv.URL, "TEST_SHARED_QUEUE", dir, srv.Client())
	ci.cache.now = clock

	got, err := release.QueuedJobs(context.Background(), repos, []string{"linux", "release"})
	if err != nil || len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("release jobs=%v err=%v", got, err)
	}
	if runs, jobs := qs.counts(); runs != 3 || jobs != 1 {
		t.Fatalf("first scan runs=%d jobs=%d", runs, jobs)
	}

	// Another loop (other labels) within the TTL reuses the snapshot.
	now = now.Add(queueShareTTL - time.Second)
	got, err = ci.QueuedJobs(context.Background(), repos, []string{"linux", "ci"})
	if err != nil || len(got) != 1 || got[0].ID != 2 {
		t.Fatalf("ci jobs=%v err=%v", got, err)
	}
	if runs, jobs := qs.counts(); runs != 3 || jobs != 1 {
		t.Fatalf("shared scan hit GitHub again: runs=%d jobs=%d", runs, jobs)
	}

	// Past the TTL the next loop rescans.
	now = now.Add(2 * time.Second)
	if _, err := ci.QueuedJobs(context.Background(), repos, []string{"linux", "ci"}); err != nil {
		t.Fatal(err)
	}
	if runs, jobs := qs.counts(); runs != 6 || jobs != 2 {
		t.Fatalf("expired snapshot not refreshed: runs=%d jobs=%d", runs, jobs)
	}
}

func TestQueuedJobsFailureIsNotShared(t *testing.T) {
	t.Setenv("TEST_SHARED_QUEUE_FAIL", "fake-queue-token")
	qs := &queueServer{fail: true}
	srv := httptest.NewServer(http.HandlerFunc(qs.handler))
	defer srv.Close()
	dir := t.TempDir()
	repos := []Repository{{Owner: "me", Name: "app"}}
	c := newCachedClient(t, srv.URL, "TEST_SHARED_QUEUE_FAIL", dir, srv.Client())

	got, err := c.QueuedJobs(context.Background(), repos, []string{"linux"})
	var apiErr *APIError
	if got != nil || !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden {
		t.Fatalf("jobs=%v err=%v", got, err)
	}
	qs.mu.Lock()
	qs.fail = false
	qs.mu.Unlock()
	got, err = c.QueuedJobs(context.Background(), repos, []string{"linux", "ci"})
	if err != nil || len(got) != 1 || got[0].ID != 2 {
		t.Fatalf("after failure jobs=%v err=%v", got, err)
	}
	if runs, _ := qs.counts(); runs != 3 {
		t.Fatalf("expected a fresh scan after the failure, runs=%d", runs)
	}
}

func TestQueuedJobsConcurrentLoopsScanOnce(t *testing.T) {
	t.Setenv("TEST_SHARED_QUEUE_CONC", "fake-queue-token")
	qs := &queueServer{}
	srv := httptest.NewServer(http.HandlerFunc(qs.handler))
	defer srv.Close()
	dir := t.TempDir()
	repos := []Repository{{Owner: "me", Name: "app"}}

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := newCachedClient(t, srv.URL, "TEST_SHARED_QUEUE_CONC", dir, srv.Client())
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			got, err := c.QueuedJobs(ctx, repos, []string{"linux", "ci"})
			if err == nil && (len(got) != 1 || got[0].ID != 2) {
				err = fmt.Errorf("jobs=%v", got)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if runs, jobs := qs.counts(); runs != 3 || jobs != 1 {
		t.Fatalf("8 concurrent loops should share one scan: runs=%d jobs=%d", runs, jobs)
	}
}

func TestHostCacheFallsBackWhenDirectoryIsUnsafe(t *testing.T) {
	t.Setenv("TEST_SHARED_QUEUE_LINK", "fake-queue-token")
	qs := &queueServer{}
	srv := httptest.NewServer(http.HandlerFunc(qs.handler))
	defer srv.Close()
	root := t.TempDir()
	target := filepath.Join(root, "elsewhere")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "cache")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	repos := []Repository{{Owner: "me", Name: "app"}}
	c := newCachedClient(t, srv.URL, "TEST_SHARED_QUEUE_LINK", link, srv.Client())
	for i := 0; i < 2; i++ {
		got, err := c.QueuedJobs(context.Background(), repos, []string{"linux", "release"})
		if err != nil || len(got) != 1 || got[0].ID != 1 {
			t.Fatalf("call %d jobs=%v err=%v", i, got, err)
		}
	}
	if runs, _ := qs.counts(); runs != 6 {
		t.Fatalf("unsafe cache dir must fall back to direct scans, runs=%d", runs)
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("symlinked cache target was written: %v", entries)
	}
}

func TestConditionalCachePrunesStaleEntries(t *testing.T) {
	dir := t.TempDir()
	h := newHostCache(dir)
	now := time.Date(2026, 11, 19, 14, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return now }
	etagDir, err := h.subdir("etag")
	if err != nil {
		t.Fatal(err)
	}
	old := now.Add(-2 * conditionalMaxAge)
	for i := 0; i < conditionalMaxEntries; i++ {
		path := filepath.Join(etagDir, fmt.Sprintf("old-%d.json", i))
		if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	h.storeConditional("fresh", `W/"x"`, []byte(`{"ok":true}`))
	entries, err := os.ReadDir(etagDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "fresh.json" {
		t.Fatalf("stale entries not pruned: %d left", len(entries))
	}
	if entry, ok := h.loadConditional("fresh"); !ok || entry.ETag != `W/"x"` {
		t.Fatalf("fresh entry = %+v ok=%t", entry, ok)
	}
}

func TestRegistrationTokenInvalidatesSharedSnapshot(t *testing.T) {
	t.Setenv("TEST_SHARED_QUEUE_CLAIM", "fake-queue-token")
	qs := &queueServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/me/app/actions/runners/registration-token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"token":"fake-registration"}`)
	})
	mux.HandleFunc("/orgs/acme/actions/runners/registration-token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"token":"fake-org-registration"}`)
	})
	mux.HandleFunc("/", qs.handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	dir := t.TempDir()
	repos := []Repository{{Owner: "me", Name: "app"}}
	labels := []string{"linux", "release"}
	c := newCachedClient(t, srv.URL, "TEST_SHARED_QUEUE_CLAIM", dir, srv.Client())
	other := newCachedClient(t, srv.URL, "TEST_SHARED_QUEUE_CLAIM", dir, srv.Client())

	if _, err := c.QueuedJobs(context.Background(), repos, labels); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateRegistrationToken(context.Background(), "me", "app"); err != nil {
		t.Fatal(err)
	}
	if _, err := other.QueuedJobs(context.Background(), repos, labels); err != nil {
		t.Fatal(err)
	}
	if runs, _ := qs.counts(); runs != 6 {
		t.Fatalf("a claimed snapshot must be rescanned, runs=%d", runs)
	}
	if _, err := other.QueuedJobs(context.Background(), repos, labels); err != nil {
		t.Fatal(err)
	}
	if runs, _ := qs.counts(); runs != 6 {
		t.Fatalf("the rescanned snapshot should be shared again, runs=%d", runs)
	}
	if _, err := c.CreateOrgRegistrationToken(context.Background(), "acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := other.QueuedJobs(context.Background(), repos, labels); err != nil {
		t.Fatal(err)
	}
	if runs, _ := qs.counts(); runs != 9 {
		t.Fatalf("an organization claim must drop every snapshot, runs=%d", runs)
	}
}
