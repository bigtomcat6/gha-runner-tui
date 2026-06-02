package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"gha-runner-tui/internal/command"
	"gha-runner-tui/internal/state"
)

var ErrMissingToken = errors.New("github token is not configured")

const defaultLegacyTokenFile = "/etc/gha-runner/github_pat"

type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

type Client struct {
	baseURL   string
	tokenEnv  string
	tokenFile string
	runner    command.Runner
	http      HTTPDoer
}

type Runner struct {
	ID            int64
	Name          string
	Status        state.GitHubStatus
	Busy          bool
	RunnerGroupID int64
}

type RunnerGroup struct {
	ID         int64
	Name       string
	Visibility string
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
	if tokenFile == "" {
		tokenFile = defaultLegacyTokenFile
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
	}
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

func (c Client) ListOrgRunnerGroups(ctx context.Context, org string) ([]RunnerGroup, error) {
	var payload struct {
		RunnerGroups []struct {
			ID         int64  `json:"id"`
			Name       string `json:"name"`
			Visibility string `json:"visibility"`
		} `json:"runner_groups"`
	}
	if err := c.requestJSON(ctx, http.MethodGet, fmt.Sprintf("/orgs/%s/actions/runner-groups", org), nil, &payload); err != nil {
		return nil, err
	}

	groups := make([]RunnerGroup, 0, len(payload.RunnerGroups))
	for _, group := range payload.RunnerGroups {
		groups = append(groups, RunnerGroup{
			ID:         group.ID,
			Name:       group.Name,
			Visibility: group.Visibility,
		})
	}
	return groups, nil
}

func (c Client) CreateOrgRunnerGroup(ctx context.Context, org, name, visibility string) (RunnerGroup, error) {
	var payload struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		Visibility string `json:"visibility"`
	}
	err := c.requestJSON(ctx, http.MethodPost, fmt.Sprintf("/orgs/%s/actions/runner-groups", org), map[string]any{
		"name":       name,
		"visibility": visibility,
	}, &payload)
	return RunnerGroup{
		ID:         payload.ID,
		Name:       payload.Name,
		Visibility: payload.Visibility,
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

	if resp.StatusCode >= http.StatusBadRequest {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("github api %s %s returned %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}

	return json.NewDecoder(resp.Body).Decode(out)
}

func (c Client) resolveToken(ctx context.Context) (string, error) {
	if token := strings.TrimSpace(os.Getenv(c.tokenEnv)); token != "" {
		return token, nil
	}

	if c.tokenFile == "" {
		return "", ErrMissingToken
	}
	if data, err := os.ReadFile(c.tokenFile); err == nil {
		if token := strings.TrimSpace(string(data)); token != "" {
			return token, nil
		}
	}
	if c.runner != nil {
		out, err := c.runner.Run(ctx, "cat", c.tokenFile)
		if err == nil {
			if token := strings.TrimSpace(string(out)); token != "" {
				return token, nil
			}
		}
	}
	return "", ErrMissingToken
}
