package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"gha-runner-tui/internal/command"
	"gha-runner-tui/internal/config"
	"gha-runner-tui/internal/state"
)

var ErrMissingToken = errors.New("github token is not configured")

type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

type Client struct {
	observations *observationState
	transport    *transport
	initErr      error
	legacy       bool
	baseURL      string
	tokenEnv     string
	tokenFile    string
}

type Runner struct {
	Labels        []string
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
	RestrictedToWorkflows    bool
	SelectedWorkflows        []string
}

type rawRunner struct {
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Status        string `json:"status"`
	Busy          bool   `json:"busy"`
	RunnerGroupID int64  `json:"runner_group_id"`
}

func NewClient(baseURL, tokenEnv, tokenFile string, runner command.Runner, httpClient HTTPDoer) Client {
	if tokenEnv == "" && tokenFile == "" {
		tokenEnv = "GITHUB_TOKEN"
	}
	// Keep the old constructor signature, not its sudo/cat credential fallback.
	c, err := NewScopedClient(baseURL, config.CredentialRef{ID: "legacy", ResourceOwner: "legacy", TokenEnv: tokenEnv, TokenFile: tokenFile}, CredentialIO{}, httpClient, nil, nil)
	c.initErr, c.legacy, c.tokenEnv, c.tokenFile = err, true, tokenEnv, tokenFile
	return c
}

func (c Client) ListRepoRunners(ctx context.Context, owner, repo string) ([]Runner, error) {
	return c.listRunners(ctx, fmt.Sprintf("/repos/%s/%s/actions/runners", owner, repo))
}

func (c Client) ListOrgRunners(ctx context.Context, org string) ([]Runner, error) {
	return c.listRunners(ctx, fmt.Sprintf("/orgs/%s/actions/runners", org))
}

func (c Client) DeleteRunner(ctx context.Context, owner, repo string, id int64) error {
	return c.requestJSON(ctx, http.MethodDelete, fmt.Sprintf("/repos/%s/%s/actions/runners/%d", owner, repo, id), nil, nil)
}

func (c Client) CreateRegistrationToken(ctx context.Context, owner, repo string) (string, error) {
	return c.legacyToken(ctx, fmt.Sprintf("/repos/%s/%s/actions/runners/registration-token", owner, repo))
}

func (c Client) CreateRemoveToken(ctx context.Context, owner, repo string) (string, error) {
	return c.legacyToken(ctx, fmt.Sprintf("/repos/%s/%s/actions/runners/remove-token", owner, repo))
}

func (c Client) CreateOrgRegistrationToken(ctx context.Context, org string) (string, error) {
	return c.legacyToken(ctx, fmt.Sprintf("/orgs/%s/actions/runners/registration-token", org))
}

func (c Client) CreateOrgRemoveToken(ctx context.Context, org string) (string, error) {
	return c.legacyToken(ctx, fmt.Sprintf("/orgs/%s/actions/runners/remove-token", org))
}

// CreateScopedRegistrationToken is the expiry-bearing host API. Legacy callers
// retain their string-returning signatures but use the same validated decoder.
func (c Client) CreateScopedRegistrationToken(ctx context.Context, target config.ResolvedTarget) (RegistrationToken, error) {
	if target.Scope == config.TargetScopeOrganization {
		return c.registrationToken(ctx, fmt.Sprintf("/orgs/%s/actions/runners/registration-token", target.OrgSlug))
	}
	if target.Scope != config.TargetScopeRepository {
		return RegistrationToken{}, errors.New("invalid registration target")
	}
	return c.registrationToken(ctx, fmt.Sprintf("/repos/%s/%s/actions/runners/registration-token", target.Owner, target.Repo))
}

func (c Client) legacyToken(ctx context.Context, path string) (string, error) {
	token, err := c.registrationToken(ctx, path)
	if err != nil {
		return "", err
	}
	defer token.Clear()
	data := token.Bytes()
	defer clear(data)
	return string(data), nil
}

func (c Client) registrationToken(ctx context.Context, path string) (RegistrationToken, error) {
	var payload struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := c.requestJSON(ctx, http.MethodPost, path, map[string]any{}, &payload); err != nil {
		return RegistrationToken{}, err
	}
	if c.transport == nil || !payload.ExpiresAt.After(c.transport.now()) {
		return RegistrationToken{}, errors.New("invalid registration token expiry")
	}
	value := []byte(payload.Token)
	defer clear(value)
	return NewRegistrationToken(value, payload.ExpiresAt)
}

func (c Client) DeleteOrgRunner(ctx context.Context, org string, id int64) error {
	return c.requestJSON(ctx, http.MethodDelete, fmt.Sprintf("/orgs/%s/actions/runners/%d", org, id), nil, nil)
}

func (c Client) ListOrgRunnerGroups(ctx context.Context, org string) ([]RunnerGroup, error) {
	raw, err := listPages[rawGroup](ctx, c, fmt.Sprintf("/orgs/%s/actions/runner-groups", org), "runner_groups", false)
	if err != nil {
		return nil, err
	}
	groups := make([]RunnerGroup, 0, len(raw))
	for _, group := range raw {
		groups = append(groups, RunnerGroup{
			ID:                       group.ID,
			Name:                     group.Name,
			Visibility:               group.Visibility,
			AllowsPublicRepositories: group.AllowsPublicRepositories != nil && *group.AllowsPublicRepositories,
			RestrictedToWorkflows:    group.RestrictedToWorkflows != nil && *group.RestrictedToWorkflows,
			SelectedWorkflows:        append([]string(nil), group.SelectedWorkflows...),
		})
	}
	return groups, nil
}

func (c Client) ListOrgRunnerGroupRunners(ctx context.Context, org string, id int64) ([]Runner, error) {
	return c.listRunners(ctx, fmt.Sprintf("/orgs/%s/actions/runner-groups/%d/runners", org, id))
}

func (c Client) listRunners(ctx context.Context, path string) ([]Runner, error) {
	raw, err := listPages[rawRunner](ctx, c, path, "runners", false)
	if err != nil {
		return nil, err
	}
	return mapRunners(raw), nil
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
		labels := make([]string, 0, len(runner.Labels))
		for _, label := range runner.Labels {
			labels = append(labels, label.Name)
		}
		runners = append(runners, Runner{
			Labels:        labels,
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
	return c.requestJSONLinked(ctx, method, path, body, out, nil)
}

func (c Client) requestJSONLinked(ctx context.Context, method, path string, body any, out any, link *string) error {
	if c.initErr != nil {
		return c.initErr
	}
	if c.transport == nil {
		return ErrMissingToken
	}
	if c.legacy {
		if err := c.CheckCurrent(c.transport.ref, CredentialIO{}); err != nil {
			return err
		}
	}
	data, err := c.transport.requestLinked(ctx, method, path, body, link)
	if err != nil {
		return err
	}
	defer clear(data)
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return errors.New("invalid github JSON response")
	}
	return nil
}
