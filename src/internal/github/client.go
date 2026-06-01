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
	ID     int64
	Name   string
	Status state.GitHubStatus
	Busy   bool
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
		Runners []struct {
			ID     int64  `json:"id"`
			Name   string `json:"name"`
			Status string `json:"status"`
			Busy   bool   `json:"busy"`
		} `json:"runners"`
	}
	if err := c.requestJSON(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/actions/runners", owner, repo), nil, &payload); err != nil {
		return nil, err
	}

	runners := make([]Runner, 0, len(payload.Runners))
	for _, runner := range payload.Runners {
		runners = append(runners, Runner{
			ID:     runner.ID,
			Name:   runner.Name,
			Status: state.NormalizeGitHubStatus(strings.ToLower(runner.Status)),
			Busy:   runner.Busy,
		})
	}
	return runners, nil
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
