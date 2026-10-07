package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"gha-runner-tui/internal/config"
)

var ErrIncomplete = errors.New("github observation incomplete")
var errBudget = errors.New("github repository scan budget exhausted")

type observedJobKey struct {
	Auth                 AuthIdentity
	RepoID, RunID, JobID int64
	Attempt              int
}
type observationState struct {
	mu      sync.Mutex
	cursors map[ObservationKey]*repoCursor
	first   map[observedJobKey]time.Time
}
type repoCursor struct {
	search   []runSearch
	runs     []rawRun
	pages    map[string]*pageScan
	runIndex int
	seenRuns map[int64]bool
	jobs     []Job
}
type runSearch struct {
	status string
	lo, hi *time.Time
	path   string
	items  []rawRun
	count  int
	total  int
}
type pageScan struct {
	next    string
	items   []json.RawMessage
	count   int
	total   int
	visited map[string]bool
	done    bool
}
type rawRun struct {
	ID         int64          `json:"id"`
	Attempt    int            `json:"run_attempt"`
	Status     string         `json:"status"`
	CreatedAt  time.Time      `json:"created_at"`
	Path       string         `json:"path"`
	HeadSHA    string         `json:"head_sha"`
	Repository *rawRepository `json:"repository"`
}

func (r rawRun) matchesRepo(repo config.RepoRef) bool {
	return r.Repository == nil || (r.Repository.ID == repo.ID && RepoKey(r.Repository.ref()) == RepoKey(repo))
}

type rawJob struct {
	ID         int64     `json:"id"`
	RunID      int64     `json:"run_id"`
	Attempt    int       `json:"run_attempt"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	Labels     []string  `json:"labels"`
	RunnerID   int64     `json:"runner_id"`
	RunnerName string    `json:"runner_name"`
	GroupID    int64     `json:"runner_group_id"`
	CreatedAt  time.Time `json:"created_at"`
}
type JobRef struct {
	Repo         config.RepoRef
	RunID, JobID int64
	Attempt      int
}
type Job struct {
	Ref                             JobRef
	Status, Conclusion, WorkflowRef string
	Labels                          []string
	GroupID, RunnerID               int64
	RunnerName                      string
	CreatedAt, FirstObserved        time.Time
}
type ObservationKey struct {
	Auth    AuthIdentity
	RepoKey string
}
type JobObservation struct {
	Auth AuthIdentity
	Job  Job
}
type RepoObservation struct {
	Auth       AuthIdentity
	Repo       config.RepoRef
	Jobs       []Job
	Complete   bool
	ErrorCode  string
	ObservedAt time.Time
}

func RepoKey(repo config.RepoRef) string { return repo.RepoKey() }
func (c Client) now() time.Time {
	if c.transport != nil {
		return c.transport.now()
	}
	return time.Now()
}
func (c Client) repoAllowed(repo config.RepoRef) bool {
	if c.transport == nil || repo.ID <= 0 || !strings.EqualFold(repo.Owner, c.transport.ref.ResourceOwner) {
		return false
	}
	for _, r := range c.transport.ref.Repositories {
		if r.ID == repo.ID && RepoKey(r) == RepoKey(repo) {
			return true
		}
	}
	return false
}
func (c Client) verifyRepo(ctx context.Context, repo config.RepoRef) error {
	var current rawRepository
	if err := c.requestJSON(ctx, http.MethodGet, repoPath(repo), nil, &current); err != nil {
		return err
	}
	if current.ID != repo.ID || RepoKey(current.ref()) != RepoKey(repo) || !current.known() {
		return ErrIncomplete
	}
	return nil
}
func repoPath(repo config.RepoRef) string { return "/repos/" + repo.Owner + "/" + repo.Name }

// page=1 is explicit; next links must preserve the exact endpoint and filters.
func firstPage(path string) string {
	u, _ := url.Parse(path)
	q := u.Query()
	q.Set("page", "1")
	q.Set("per_page", "100")
	u.RawQuery = q.Encode()
	return u.String()
}
func (c Client) nextPage(current, link string) (string, error) {
	var next string
	for _, part := range strings.Split(link, ",") {
		pieces := strings.Split(strings.TrimSpace(part), ";")
		if len(pieces) < 2 {
			continue
		}
		isNext := false
		for _, p := range pieces[1:] {
			if strings.TrimSpace(p) == `rel="next"` {
				isNext = true
			}
		}
		if !isNext {
			continue
		}
		if next != "" {
			return "", ErrIncomplete
		}
		next = strings.Trim(pieces[0], "<>")
	}
	if next == "" {
		return "", nil
	}
	a, err := url.Parse(current)
	if err != nil {
		return "", ErrIncomplete
	}
	b, err := url.Parse(next)
	if err != nil {
		return "", ErrIncomplete
	}
	b = a.ResolveReference(b)
	expectedPath := a.Path
	if !a.IsAbs() {
		expectedPath = strings.TrimRight(c.transport.base.Path, "/") + "/" + strings.TrimLeft(a.Path, "/")
	}
	if b.Path != expectedPath {
		return "", ErrIncomplete
	}
	aq, bq := a.Query(), b.Query()
	ap, _ := strconv.Atoi(aq.Get("page"))
	bp, _ := strconv.Atoi(bq.Get("page"))
	if bp != ap+1 || bq.Get("per_page") != "100" {
		return "", ErrIncomplete
	}
	aq.Del("page")
	bq.Del("page")
	if aq.Encode() != bq.Encode() {
		return "", ErrIncomplete
	}
	return b.String(), nil
}
func (c Client) page(ctx context.Context, path, field string, required bool) ([]json.RawMessage, int, string, error) {
	if field == "" {
		var items []json.RawMessage
		var link string
		if err := c.requestJSONLinked(ctx, http.MethodGet, path, nil, &items, &link); err != nil {
			return nil, 0, "", err
		}
		if items == nil || len(items) > 100 {
			return nil, 0, "", ErrIncomplete
		}
		next, err := c.nextPage(path, link)
		return items, -1, next, err
	}
	var payload map[string]json.RawMessage
	var link string
	if err := c.requestJSONLinked(ctx, http.MethodGet, path, nil, &payload, &link); err != nil {
		return nil, 0, "", err
	}
	data, ok := payload[field]
	if !ok || string(data) == "null" {
		return nil, 0, "", ErrIncomplete
	}
	var items []json.RawMessage
	if err := json.Unmarshal(data, &items); err != nil || len(items) > 100 {
		return nil, 0, "", ErrIncomplete
	}
	total := -1
	if data, ok = payload["total_count"]; ok {
		if json.Unmarshal(data, &total) != nil || total < 0 {
			return nil, 0, "", ErrIncomplete
		}
	} else if required {
		return nil, 0, "", ErrIncomplete
	}
	next, err := c.nextPage(path, link)
	return items, total, next, err
}
func listPages[T any](ctx context.Context, c Client, path, field string, required bool) ([]T, error) {
	return listPagesResume[T](ctx, c, path, field, required, nil, nil)
}
func listPagesResume[T any](ctx context.Context, c Client, path, field string, required bool, scans map[string]*pageScan, check func() error) ([]T, error) {
	if err := c.checkValid(); err != nil {
		return nil, err
	}
	key := firstPage(path)
	s := scans[key]
	if s == nil {
		s = &pageScan{next: key, total: -1, visited: make(map[string]bool)}
		if scans != nil {
			scans[key] = s
		}
	}
	for !s.done {
		if check != nil {
			if err := check(); err != nil {
				return nil, err
			}
		}
		if s.count >= 10000 || s.visited[s.next] {
			return nil, ErrIncomplete
		}
		items, total, next, err := c.page(ctx, s.next, field, required)
		if err != nil {
			return nil, err
		}
		s.visited[s.next] = true
		s.items = append(s.items, items...)
		s.count++
		s.total = max(s.total, total)
		if next == "" {
			if len(s.items) < s.total {
				return nil, ErrIncomplete
			}
			s.done = true
		} else {
			s.next = next
		}
	}
	result := make([]T, 0, len(s.items))
	identities := make(map[int64]bool)
	for _, data := range s.items {
		var item T
		if json.Unmarshal(data, &item) != nil {
			return nil, ErrIncomplete
		}
		result = append(result, item)
		if required || s.total >= 0 {
			var identity struct {
				ID int64 `json:"id"`
			}
			if json.Unmarshal(data, &identity) != nil || identity.ID <= 0 {
				return nil, ErrIncomplete
			}
			identities[identity.ID] = true
		}
	}
	if len(identities) < s.total {
		return nil, ErrIncomplete
	}
	if err := c.checkValid(); err != nil {
		return nil, err
	}
	return result, nil
}
func (s runSearch) query() string {
	f := func(t *time.Time) string { return t.UTC().Format(time.RFC3339) }
	if s.lo == nil && s.hi == nil {
		return ""
	}
	if s.lo != nil && s.hi != nil {
		if s.lo.Equal(*s.hi) {
			return f(s.lo)
		}
		return f(s.lo) + ".." + f(s.hi)
	}
	if s.lo != nil {
		t := s.lo.Add(-time.Second)
		return ">" + f(&t)
	}
	t := s.hi.Add(time.Second)
	return "<" + f(&t)
}
func (c Client) ObserveRepo(ctx context.Context, repo config.RepoRef) (obs RepoObservation, err error) {
	obs = RepoObservation{Auth: c.Auth(), Repo: repo, ObservedAt: c.now()}
	if err = c.checkValid(); err != nil {
		obs.ErrorCode = "AUTH_CHANGED"
		return obs, err
	}
	if !c.repoAllowed(repo) || c.observations == nil {
		obs.ErrorCode = "SCOPE_UNAVAILABLE"
		return obs, ErrIncomplete
	}
	start := c.now()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	s := c.observations
	s.mu.Lock()
	defer s.mu.Unlock()
	key := ObservationKey{Auth: c.Auth(), RepoKey: RepoKey(repo)}
	if err = c.verifyRepo(ctx, repo); err != nil {
		obs.ErrorCode = "REPOSITORY_IDENTITY_UNAVAILABLE"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			obs.ErrorCode = "BUDGET_EXHAUSTED"
		} else {
			delete(s.cursors, key)
		}
		return obs, err
	}
	cur := s.cursors[key]
	if cur == nil {
		cur = &repoCursor{search: []runSearch{{status: "queued"}, {status: "in_progress"}}, pages: make(map[string]*pageScan), seenRuns: make(map[int64]bool)}
		s.cursors[key] = cur
	}
	defer func() {
		if invalid := c.checkValid(); invalid != nil {
			err = invalid
			obs.Jobs = nil
			obs.Complete = false
			clear(s.cursors)
			clear(s.first)
		}
		if err != nil {
			obs.ErrorCode = "INCOMPLETE"
			if errors.Is(err, errBudget) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				obs.ErrorCode = "BUDGET_EXHAUSTED"
			} else {
				delete(s.cursors, key)
			}
		} else {
			delete(s.cursors, key)
		}
	}()
	check := func() error {
		if e := ctx.Err(); e != nil {
			return e
		}
		if c.now().Sub(start) >= 60*time.Second {
			return errBudget
		}
		return c.checkValid()
	}
	base := repoPath(repo) + "/actions/runs"
	for len(cur.search) > 0 {
		if err = check(); err != nil {
			return obs, err
		}
		search := &cur.search[0]
		if search.path == "" {
			q := url.Values{"status": {search.status}}
			if w := search.query(); w != "" {
				q.Set("created", w)
			}
			search.path = firstPage(base + "?" + q.Encode())
		}
		var items []json.RawMessage
		var total int
		var next string
		items, total, next, err = c.page(ctx, search.path, "workflow_runs", true)
		if err != nil {
			return obs, err
		}
		runs := make([]rawRun, 0, len(items))
		for _, item := range items {
			var r rawRun
			if json.Unmarshal(item, &r) != nil || r.ID <= 0 || r.Attempt <= 0 || !r.matchesRepo(repo) {
				return obs, ErrIncomplete
			}
			runs = append(runs, r)
		}
		if total >= 1000 {
			if search.lo != nil && search.hi != nil && search.lo.Equal(*search.hi) || len(runs) == 0 || runs[0].CreatedAt.IsZero() {
				return obs, ErrIncomplete
			}
			pivot := runs[0].CreatedAt.UTC().Truncate(time.Second)
			if search.lo != nil && pivot.Before(*search.lo) || search.hi != nil && pivot.After(*search.hi) {
				return obs, ErrIncomplete
			}
			before, after := pivot.Add(-time.Second), pivot.Add(time.Second)
			parts := []runSearch{{status: search.status, lo: &pivot, hi: &pivot}}
			if search.lo == nil || !before.Before(*search.lo) {
				parts = append(parts, runSearch{status: search.status, lo: search.lo, hi: &before})
			}
			if search.hi == nil || !after.After(*search.hi) {
				parts = append(parts, runSearch{status: search.status, lo: &after, hi: search.hi})
			}
			cur.search = append(parts, cur.search[1:]...)
			continue
		}
		search.items = append(search.items, runs...)
		search.count++
		search.total = max(search.total, total)
		if search.count > 10 {
			return obs, ErrIncomplete
		}
		if next != "" {
			search.path = next
			continue
		}
		identities := make(map[int64]bool)
		for _, run := range search.items {
			identities[run.ID] = true
		}
		if len(identities) < search.total {
			return obs, ErrIncomplete
		}
		cur.runs = append(cur.runs, search.items...)
		cur.search = cur.search[1:]
	}
	obs.Jobs = append(obs.Jobs, cur.jobs...)
	seenJobs := make(map[observedJobKey]bool)
	for cur.runIndex < len(cur.runs) {
		run := cur.runs[cur.runIndex]
		if cur.seenRuns[run.ID] {
			cur.runIndex++
			continue
		}
		if err = check(); err != nil {
			return obs, err
		}
		path := fmt.Sprintf("%s/%d", base, run.ID)
		id := run.ID
		run = rawRun{}
		if err = c.requestJSON(ctx, http.MethodGet, path, nil, &run); err != nil {
			return obs, err
		}
		if run.ID != id || run.Attempt <= 0 || run.Status == "" || !run.matchesRepo(repo) {
			return obs, ErrIncomplete
		}
		if _, err = listPagesResume[rawJob](ctx, c, path+"/jobs?filter=latest", "jobs", true, cur.pages, check); err != nil {
			return obs, err
		}
		var jobs []rawJob
		for changes := 0; ; changes++ {
			if changes >= 5 {
				return obs, ErrIncomplete
			}
			attempt := run.Attempt
			jobs, err = listPagesResume[rawJob](ctx, c, fmt.Sprintf("%s/attempts/%d/jobs", path, attempt), "jobs", true, cur.pages, check)
			if err != nil {
				return obs, err
			}
			if err = check(); err != nil {
				return obs, err
			}
			run = rawRun{}
			if err = c.requestJSON(ctx, http.MethodGet, path, nil, &run); err != nil {
				return obs, err
			}
			if run.ID != id || run.Status == "" || !run.matchesRepo(repo) {
				return obs, ErrIncomplete
			}
			if run.Attempt == attempt {
				break
			}
			if run.Attempt <= 0 {
				return obs, ErrIncomplete
			}
		}
		for _, raw := range jobs {
			if raw.ID <= 0 || raw.RunID != run.ID || raw.Attempt != run.Attempt {
				return obs, ErrIncomplete
			}
			if raw.Status != "queued" && raw.Status != "in_progress" {
				continue
			}
			if raw.Status == "queued" && run.Status != "queued" && run.Status != "in_progress" {
				continue
			}
			k := observedJobKey{c.Auth(), repo.ID, run.ID, raw.ID, raw.Attempt}
			if seenJobs[k] {
				continue
			}
			seenJobs[k] = true
			first, ok := s.first[k]
			if !ok {
				first = c.now()
				s.first[k] = first
			}
			job := mapJob(repo, raw)
			job.FirstObserved = first
			if run.Path != "" && run.HeadSHA != "" {
				job.WorkflowRef = RepoKey(repo) + "/" + run.Path + "@" + run.HeadSHA
			}
			obs.Jobs = append(obs.Jobs, job)
			cur.jobs = append(cur.jobs, job)
		}
		cur.seenRuns[id] = true
		cur.runIndex++
	}
	if err = check(); err != nil {
		return obs, err
	}
	obs.Complete = true
	return obs, nil
}
func mapJob(repo config.RepoRef, r rawJob) Job {
	return Job{Ref: JobRef{Repo: repo, RunID: r.RunID, JobID: r.ID, Attempt: r.Attempt}, Status: r.Status, Conclusion: r.Conclusion, Labels: append([]string(nil), r.Labels...), GroupID: r.GroupID, RunnerID: r.RunnerID, RunnerName: r.RunnerName, CreatedAt: r.CreatedAt}
}
func (c Client) RefreshJob(ctx context.Context, ref JobRef) (obs JobObservation, err error) {
	obs.Auth = c.Auth()
	if err = c.checkValid(); err != nil {
		return obs, err
	}
	if !c.repoAllowed(ref.Repo) || ref.RunID <= 0 || ref.JobID <= 0 || ref.Attempt <= 0 {
		return obs, ErrIncomplete
	}
	if err = c.verifyRepo(ctx, ref.Repo); err != nil {
		return obs, err
	}
	path := fmt.Sprintf("%s/actions/runs/%d", repoPath(ref.Repo), ref.RunID)
	var run rawRun
	if err = c.requestJSON(ctx, http.MethodGet, path, nil, &run); err != nil {
		return obs, err
	}
	if run.ID != ref.RunID || run.Attempt != ref.Attempt || run.Status == "" || !run.matchesRepo(ref.Repo) {
		return obs, ErrIncomplete
	}
	var job rawJob
	if err = c.requestJSON(ctx, http.MethodGet, fmt.Sprintf("%s/actions/jobs/%d", repoPath(ref.Repo), ref.JobID), nil, &job); err != nil {
		return obs, err
	}
	if job.ID != ref.JobID || job.RunID != ref.RunID || job.Attempt != ref.Attempt {
		return obs, ErrIncomplete
	}
	run = rawRun{}
	if err = c.requestJSON(ctx, http.MethodGet, path, nil, &run); err != nil {
		return obs, err
	}
	if run.ID != ref.RunID || run.Attempt != ref.Attempt || run.Status == "" || !run.matchesRepo(ref.Repo) || (job.Status == "queued" && run.Status != "queued" && run.Status != "in_progress") {
		return obs, ErrIncomplete
	}
	obs.Job = mapJob(ref.Repo, job)
	if c.observations != nil {
		s := c.observations
		s.mu.Lock()
		obs.Job.FirstObserved = s.first[observedJobKey{c.Auth(), ref.Repo.ID, ref.RunID, ref.JobID, ref.Attempt}]
		s.mu.Unlock()
	}
	if run.Path != "" && run.HeadSHA != "" {
		obs.Job.WorkflowRef = RepoKey(ref.Repo) + "/" + run.Path + "@" + run.HeadSHA
	}
	if err = c.checkValid(); err != nil {
		obs.Job = Job{}
		return obs, err
	}
	return obs, nil
}
