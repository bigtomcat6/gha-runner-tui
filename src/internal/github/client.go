package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strings"

	"gha-runner-tui/internal/command"
	"gha-runner-tui/internal/config"
	"gha-runner-tui/internal/state"
)

var ErrMissingToken = errors.New("github token is not configured")

type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// APIError is a typed, body-free GitHub HTTP error. It never carries the
// response body so credentials accidentally echoed by the API cannot leak.
type APIError struct {
	Method     string
	Path       string
	StatusCode int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github api %s %s returned %d", e.Method, e.Path, e.StatusCode)
}

type Repository struct {
	Owner string
	Name  string
}

type Job struct {
	ID     int64
	Status string
	Labels []string
}

type Client struct {
	baseURL   string
	tokenEnv  string
	tokenFile string
	runner    command.Runner
	http      HTTPDoer

	// allowMissingFileEnv is true only for the global client: it may fall back
	// to the process environment when the configured file does not exist.
	allowMissingFileEnv bool
	// readFile is the credential file reader; defaults to os.ReadFile and is
	// overridable in tests to simulate permission errors without chmod.
	readFile func(string) ([]byte, error)
}

type Runner struct {
	ID            int64
	Name          string
	Status        state.GitHubStatus
	Busy          bool
	RunnerGroupID int64
}

type RunnerGroup struct {
	ID                       int64
	Name                     string
	Visibility               string
	AllowsPublicRepositories bool
}

type rawRunner struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Status        string `json:"status"`
	Busy          bool   `json:"busy"`
	RunnerGroupID int64  `json:"runner_group_id"`
}

func NewClient(baseURL, tokenEnv, tokenFile string, runner command.Runner, httpClient HTTPDoer) Client {
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	if tokenEnv == "" {
		tokenEnv = "GITHUB_TOKEN"
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		tokenEnv:  tokenEnv,
		tokenFile: tokenFile,
		runner:    runner,
		http:      httpClient,
		readFile:  os.ReadFile,
	}
}

// NewGlobalClient builds a client for the un-migrated/global configuration.
// Unlike NewClient it is allowed to fall back to the process environment when
// the configured token file does not exist.
func NewGlobalClient(baseURL, tokenEnv, tokenFile string, runner command.Runner, httpClient HTTPDoer) Client {
	c := NewClient(baseURL, tokenEnv, tokenFile, runner, httpClient)
	c.allowMissingFileEnv = true
	return c
}

// ForProfile derives a per-profile client. c.runner and c.http are preserved.
// Profile credential files are strict: a missing/unreadable/empty file never
// falls back to another identity. Only when the profile configures neither a
// token file nor an env file does it defer to the global client.
func (c Client) ForProfile(defaults config.GitHubConfig, p config.GitHubProfile) Client {
	tokenEnv := p.TokenEnv
	if tokenEnv == "" {
		tokenEnv = defaults.TokenEnv
	}
	if p.TokenFile == "" && p.EnvFile == "" {
		return NewGlobalClient(defaults.APIBaseURL, tokenEnv, defaults.EnvFile, c.runner, c.http)
	}
	tokenFile := p.TokenFile
	if tokenFile == "" {
		tokenFile = p.EnvFile
	}
	profile := NewClient(defaults.APIBaseURL, tokenEnv, tokenFile, c.runner, c.http)
	if c.readFile != nil {
		profile.readFile = c.readFile
	}
	return profile
}

func (c Client) ListRepoRunners(ctx context.Context, owner, repo string) ([]Runner, error) {
	var payload struct {
		Runners []rawRunner `json:"runners"`
	}
	if err := c.requestJSON(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/actions/runners", owner, repo), nil, &payload); err != nil {
		return nil, err
	}
	return mapRunners(payload.Runners), nil
}

func (c Client) ListOrgRunners(ctx context.Context, org string) ([]Runner, error) {
	var payload struct {
		Runners []rawRunner `json:"runners"`
	}
	if err := c.requestJSON(ctx, http.MethodGet, fmt.Sprintf("/orgs/%s/actions/runners", org), nil, &payload); err != nil {
		return nil, err
	}
	return mapRunners(payload.Runners), nil
}

func (c Client) DeleteRunner(ctx context.Context, owner, repo string, id int64) error {
	return c.requestJSON(ctx, http.MethodDelete, fmt.Sprintf("/repos/%s/%s/actions/runners/%d", owner, repo, id), nil, nil)
}

func (c Client) CreateRegistrationToken(ctx context.Context, owner, repo string) (string, error) {
	var payload struct {
		Token string `json:"token"`
	}
	err := c.requestJSON(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/actions/runners/registration-token", owner, repo), map[string]any{}, &payload)
	return payload.Token, err
}

func (c Client) CreateRemoveToken(ctx context.Context, owner, repo string) (string, error) {
	var payload struct {
		Token string `json:"token"`
	}
	err := c.requestJSON(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/actions/runners/remove-token", owner, repo), map[string]any{}, &payload)
	return payload.Token, err
}

func (c Client) CreateOrgRegistrationToken(ctx context.Context, org string) (string, error) {
	var payload struct {
		Token string `json:"token"`
	}
	err := c.requestJSON(ctx, http.MethodPost, fmt.Sprintf("/orgs/%s/actions/runners/registration-token", org), map[string]any{}, &payload)
	return payload.Token, err
}

func (c Client) CreateOrgRemoveToken(ctx context.Context, org string) (string, error) {
	var payload struct {
		Token string `json:"token"`
	}
	err := c.requestJSON(ctx, http.MethodPost, fmt.Sprintf("/orgs/%s/actions/runners/remove-token", org), map[string]any{}, &payload)
	return payload.Token, err
}

func (c Client) DeleteOrgRunner(ctx context.Context, org string, id int64) error {
	return c.requestJSON(ctx, http.MethodDelete, fmt.Sprintf("/orgs/%s/actions/runners/%d", org, id), nil, nil)
}

// QueuedJobs returns queued jobs across repos whose labels are a case
// insensitive subset of the requested labels. It scans both queued and
// in_progress runs so a job that started between the two queries is still
// seen, dedupes runs/jobs, and never follows pagination.
func (c Client) QueuedJobs(ctx context.Context, repos []Repository, labels []string) ([]Job, error) {
	allowed := map[string]bool{"self-hosted": true}
	for _, label := range labels {
		allowed[strings.ToLower(label)] = true
	}

	var out []Job
	for _, repo := range repos {
		seenRuns := map[int64]bool{}
		seenJobs := map[int64]bool{}
		for _, status := range []string{"queued", "in_progress"} {
			runsQuery := url.Values{}
			runsQuery.Set("status", status)
			runsQuery.Set("per_page", "100")
			runsPath := fmt.Sprintf("/repos/%s/%s/actions/runs?%s", repo.Owner, repo.Name, runsQuery.Encode())

			var runsPayload struct {
				WorkflowRuns []struct {
					ID int64 `json:"id"`
				} `json:"workflow_runs"`
			}
			if err := c.requestJSON(ctx, http.MethodGet, runsPath, nil, &runsPayload); err != nil {
				return nil, err
			}

			for _, run := range runsPayload.WorkflowRuns {
				if seenRuns[run.ID] {
					continue
				}
				seenRuns[run.ID] = true

				jobsQuery := url.Values{}
				jobsQuery.Set("filter", "latest")
				jobsQuery.Set("per_page", "100")
				jobsPath := fmt.Sprintf("/repos/%s/%s/actions/runs/%d/jobs?%s", repo.Owner, repo.Name, run.ID, jobsQuery.Encode())

				var jobsPayload struct {
					Jobs []struct {
						ID     int64    `json:"id"`
						Status string   `json:"status"`
						Labels []string `json:"labels"`
					} `json:"jobs"`
				}
				if err := c.requestJSON(ctx, http.MethodGet, jobsPath, nil, &jobsPayload); err != nil {
					return nil, err
				}

				for _, job := range jobsPayload.Jobs {
					if job.Status != "queued" || len(job.Labels) == 0 {
						continue
					}
					matched := true
					for _, label := range job.Labels {
						if !allowed[strings.ToLower(label)] {
							matched = false
							break
						}
					}
					if !matched || seenJobs[job.ID] {
						continue
					}
					seenJobs[job.ID] = true
					out = append(out, Job{ID: job.ID, Status: job.Status, Labels: job.Labels})
				}
			}
		}
	}
	return out, nil
}

// FindRunner looks up a runner by exact name for a resolved target. It uses
// the name query parameter and never falls back to prefix matching.
func (c Client) FindRunner(ctx context.Context, target config.ResolvedTarget, name string) (*Runner, error) {
	path := fmt.Sprintf("/repos/%s/%s/actions/runners", target.Owner, target.Repo)
	if target.Scope == config.TargetScopeOrganization {
		path = fmt.Sprintf("/orgs/%s/actions/runners", target.OrgSlug)
	}
	query := url.Values{}
	query.Set("name", name)

	var payload struct {
		Runners []rawRunner `json:"runners"`
	}
	if err := c.requestJSON(ctx, http.MethodGet, path+"?"+query.Encode(), nil, &payload); err != nil {
		return nil, err
	}
	for _, runner := range mapRunners(payload.Runners) {
		if runner.Name == name {
			copy := runner
			return &copy, nil
		}
	}
	return nil, nil
}

func (c Client) ListOrgRunnerGroups(ctx context.Context, org string) ([]RunnerGroup, error) {
	var payload struct {
		RunnerGroups []struct {
			ID                       int64  `json:"id"`
			Name                     string `json:"name"`
			Visibility               string `json:"visibility"`
			AllowsPublicRepositories bool   `json:"allows_public_repositories"`
		} `json:"runner_groups"`
	}
	if err := c.requestJSON(ctx, http.MethodGet, fmt.Sprintf("/orgs/%s/actions/runner-groups", org), nil, &payload); err != nil {
		return nil, err
	}

	groups := make([]RunnerGroup, 0, len(payload.RunnerGroups))
	for _, group := range payload.RunnerGroups {
		groups = append(groups, RunnerGroup{
			ID:                       group.ID,
			Name:                     group.Name,
			Visibility:               group.Visibility,
			AllowsPublicRepositories: group.AllowsPublicRepositories,
		})
	}
	return groups, nil
}

func (c Client) ListOrgRunnerGroupRunners(ctx context.Context, org string, id int64) ([]Runner, error) {
	var payload struct {
		Runners []rawRunner `json:"runners"`
	}
	if err := c.requestJSON(ctx, http.MethodGet, fmt.Sprintf("/orgs/%s/actions/runner-groups/%d/runners", org, id), nil, &payload); err != nil {
		return nil, err
	}
	return mapRunners(payload.Runners), nil
}

func (c Client) CreateOrgRunnerGroup(ctx context.Context, org, name, visibility string) (RunnerGroup, error) {
	var payload struct {
		ID                       int64  `json:"id"`
		Name                     string `json:"name"`
		Visibility               string `json:"visibility"`
		AllowsPublicRepositories bool   `json:"allows_public_repositories"`
	}
	err := c.requestJSON(ctx, http.MethodPost, fmt.Sprintf("/orgs/%s/actions/runner-groups", org), map[string]any{
		"name":                       name,
		"visibility":                 visibility,
		"allows_public_repositories": false,
	}, &payload)
	return RunnerGroup{
		ID:                       payload.ID,
		Name:                     payload.Name,
		Visibility:               payload.Visibility,
		AllowsPublicRepositories: payload.AllowsPublicRepositories,
	}, err
}

func (c Client) UpdateOrgRunnerGroup(ctx context.Context, org string, id int64, name, visibility string) (RunnerGroup, error) {
	var payload struct {
		ID                       int64  `json:"id"`
		Name                     string `json:"name"`
		Visibility               string `json:"visibility"`
		AllowsPublicRepositories bool   `json:"allows_public_repositories"`
	}
	err := c.requestJSON(ctx, http.MethodPatch, fmt.Sprintf("/orgs/%s/actions/runner-groups/%d", org, id), map[string]any{
		"name":                       name,
		"visibility":                 visibility,
		"allows_public_repositories": false,
	}, &payload)
	return RunnerGroup{
		ID:                       payload.ID,
		Name:                     payload.Name,
		Visibility:               payload.Visibility,
		AllowsPublicRepositories: payload.AllowsPublicRepositories,
	}, err
}

func (c Client) DeleteOrgRunnerGroup(ctx context.Context, org string, id int64) error {
	return c.requestJSON(ctx, http.MethodDelete, fmt.Sprintf("/orgs/%s/actions/runner-groups/%d", org, id), nil, nil)
}

func MatchRunner(runners []Runner, exactName, prefix string) *Runner {
	for _, runner := range runners {
		if exactName != "" && runner.Name == exactName {
			copy := runner
			return &copy
		}
	}

	for _, runner := range runners {
		if prefix != "" && strings.HasPrefix(runner.Name, prefix) {
			copy := runner
			return &copy
		}
	}

	return nil
}

func BusyState(runner *Runner) state.BusyStatus {
	if runner == nil {
		return state.BusyNA
	}
	if runner.Busy {
		return state.BusyYes
	}
	return state.BusyNo
}

func RunnerState(runner *Runner) state.GitHubStatus {
	if runner == nil {
		return state.GitHubGone
	}
	return runner.Status
}

func mapRunners(raw []rawRunner) []Runner {
	runners := make([]Runner, 0, len(raw))
	for _, runner := range raw {
		runners = append(runners, Runner{
			ID:            runner.ID,
			Name:          runner.Name,
			Status:        state.NormalizeGitHubStatus(strings.ToLower(runner.Status)),
			Busy:          runner.Busy,
			RunnerGroupID: runner.RunnerGroupID,
		})
	}
	return runners
}

func (c Client) requestJSON(ctx context.Context, method, path string, body any, out any) error {
	token, err := c.resolveToken(ctx)
	if err != nil {
		return err
	}

	var reader io.Reader
	if body != nil {
		buf := bytes.NewBuffer(nil)
		if err := json.NewEncoder(buf).Encode(body); err != nil {
			return err
		}
		reader = buf
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest || (method == http.MethodDelete && resp.StatusCode != http.StatusNoContent) {
		return &APIError{Method: method, Path: path, StatusCode: resp.StatusCode}
	}

	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}

	return json.NewDecoder(resp.Body).Decode(out)
}

func (c Client) resolveToken(ctx context.Context) (string, error) {
	if c.tokenFile == "" {
		if token := strings.TrimSpace(os.Getenv(c.tokenEnv)); token != "" {
			return token, nil
		}
		return "", ErrMissingToken
	}

	data, readErr := c.readTokenFile()
	if readErr == nil {
		token := c.parseTokenFile(string(data))
		if token == "" {
			return "", fmt.Errorf("github token file %q did not contain %s", c.tokenFile, c.tokenEnv)
		}
		return token, nil
	}

	// Only the global client may fall back to the process environment, and
	// only when the file genuinely does not exist. Any other failure
	// (permission, IO) must not silently switch identity.
	if c.allowMissingFileEnv && errors.Is(readErr, fs.ErrNotExist) {
		if token := strings.TrimSpace(os.Getenv(c.tokenEnv)); token != "" {
			return token, nil
		}
		return "", ErrMissingToken
	}

	if c.runner != nil {
		out, err := c.runner.Run(ctx, "sudo", "-n", "cat", c.tokenFile)
		if err == nil {
			token := c.parseTokenFile(string(out))
			if token == "" {
				return "", fmt.Errorf("github token file %q did not contain %s", c.tokenFile, c.tokenEnv)
			}
			return token, nil
		}
	}
	// Safe error: only the path and failure category, never file contents or
	// command output.
	return "", fmt.Errorf("github token file %q is not readable: %w", c.tokenFile, readErr)
}

func (c Client) readTokenFile() ([]byte, error) {
	if c.readFile != nil {
		return c.readFile(c.tokenFile)
	}
	return os.ReadFile(c.tokenFile)
}

func (c Client) parseTokenFile(content string) string {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return ""
	}
	if !strings.ContainsAny(trimmed, "= \t\r\n") && !strings.HasPrefix(trimmed, "#") {
		return trimmed
	}

	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if strings.TrimSpace(key) != c.tokenEnv {
			continue
		}
		return strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return ""
}
