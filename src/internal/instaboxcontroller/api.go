package instaboxcontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// productionBaseURL is deliberately not configurable. The API key is only ever
// sent to this host; tests use local servers through newAPIClient.
const productionBaseURL = "https://api.instabox.sh"

const (
	getTimeout  = 15 * time.Second
	wakeTimeout = 60 * time.Second
	// execTimeout is the local bound for one synchronous exec request. The
	// request's own timeoutSeconds is lower, so a server-side timeout arrives
	// as a definite result instead of a lost answer.
	execTimeout        = 25 * time.Second
	execCommandSeconds = 20
	maxBodySize        = 1 << 20
	// maxExecBodySize bounds one exec answer. The documented 1 MiB stdout and
	// 1 MiB stderr may be JSON-escaped, so this leaves headroom; a larger or
	// cut answer fails to decode and is reported as unknown, never success.
	maxExecBodySize = 8 << 20
)

// errNoAPIKey keeps an unauthenticated controller from sending any request.
var errNoAPIKey = errors.New("an Instabox API key is required")

// APIError is a non-2xx answer. It carries only the status and the documented
// error code, never the raw response body or request headers.
type APIError struct {
	Status    int
	Code      string
	Retryable bool
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("instabox API returned status %d", e.Status)
	}
	return fmt.Sprintf("instabox API returned status %d (%s)", e.Status, e.Code)
}

// Runtime is the subset of the machine's runtime block this controller trusts.
type Runtime struct {
	Ready           bool `json:"ready"`
	HostUnreachable bool `json:"hostUnreachable"`
}

// Machine mirrors the documented fields of GET /v1/machines/{id}. The response
// is a bare Machine object, not a wrapper.
type Machine struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	State           string  `json:"state"`
	DesiredState    string  `json:"desiredState"`
	OperationID     *string `json:"operationId"`
	Runtime         Runtime `json:"runtime"`
	RunStartedAt    *string `json:"runStartedAt"`
	MaxRunMinutes   *int    `json:"maxRunMinutes"`
	CooldownUntil   *string `json:"cooldownUntil"`
	CooldownReason  *string `json:"cooldownReason"`
	AutoSuspendedAt *string `json:"autoSuspendedAt"`
	Revision        int64   `json:"revision"`
}

// ExecResult is the documented synchronous answer of POST
// /v1/machines/{id}/exec. A non-zero exit code is still a completed run, not
// an API error.
type ExecResult struct {
	Stdout      string
	Stderr      string
	ExitCode    int
	TimedOut    bool
	Truncated   bool
	InvalidUTF8 bool
}

// MachineAPI is the GET, the single documented wake POST and the documented
// synchronous exec route. There is no suspend, stop or delete path.
type MachineAPI interface {
	GetMachine(ctx context.Context, id string) (Machine, error)
	WakeMachine(ctx context.Context, id string) (Machine, error)
	ExecCommand(ctx context.Context, id, command string) (ExecResult, error)
}

// APIClient talks to the fixed production API host.
type APIClient struct {
	baseURL string
	key     string
	client  *http.Client
}

// NewAPIClient builds a client for https://api.instabox.sh. Configuration
// cannot change the host, so a controller config cannot exfiltrate the key.
func NewAPIClient(key string, client *http.Client) *APIClient {
	if client == nil {
		client = &http.Client{}
	}
	return &APIClient{baseURL: productionBaseURL, key: strings.TrimSpace(key), client: client}
}

// newAPIClient is for tests only; the production constructor fixes the host.
func newAPIClient(baseURL, key string, client *http.Client) *APIClient {
	if client == nil {
		client = &http.Client{}
	}
	return &APIClient{baseURL: strings.TrimRight(baseURL, "/"), key: key, client: client}
}

func (c *APIClient) GetMachine(ctx context.Context, id string) (Machine, error) {
	if c.key == "" {
		return Machine{}, errNoAPIKey
	}
	data, err := c.do(ctx, http.MethodGet, "/v1/machines/"+url.PathEscape(id), getTimeout)
	if err != nil {
		return Machine{}, err
	}
	machine, err := decodeMachine(data)
	if err != nil {
		return Machine{}, err
	}
	if machine.ID != id {
		return Machine{}, errors.New("instabox API returned a different machine identity")
	}
	return machine, nil
}

// WakeMachine posts the documented wake request. Wake takes no body and, per
// the API reference, needs no Idempotency-Key; the controller's own persisted
// attempt record is what prevents a second wake for the same suspend cycle.
func (c *APIClient) WakeMachine(ctx context.Context, id string) (Machine, error) {
	if c.key == "" {
		return Machine{}, errNoAPIKey
	}
	data, status, err := c.doRequestBody(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(id)+"/wake", nil, wakeTimeout, maxBodySize)
	if err != nil {
		return Machine{}, err
	}
	if status != http.StatusOK && status != http.StatusAccepted {
		return Machine{}, statusError(status, data)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return Machine{}, nil
	}
	machine, err := decodeMachine(data)
	if err != nil {
		return Machine{}, err
	}
	return machine, nil
}

// ExecCommand posts one documented synchronous exec request. Only the command
// text is sent; the controller never passes the API key to the machine.
func (c *APIClient) ExecCommand(ctx context.Context, id, command string) (ExecResult, error) {
	if c.key == "" {
		return ExecResult{}, errNoAPIKey
	}
	if !machineIDPattern.MatchString(id) {
		return ExecResult{}, errors.New("instabox exec was given an unsafe machine ID")
	}
	if command == "" {
		return ExecResult{}, errors.New("instabox exec was given an empty command")
	}
	body, err := json.Marshal(struct {
		Cmd            string `json:"cmd"`
		TimeoutSeconds int    `json:"timeoutSeconds"`
		NoHistory      bool   `json:"noHistory"`
	}{Cmd: command, TimeoutSeconds: execCommandSeconds, NoHistory: true})
	if err != nil {
		return ExecResult{}, errors.New("instabox exec request could not be encoded")
	}
	data, status, err := c.doRequestBody(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(id)+"/exec", body, execTimeout, maxExecBodySize)
	if err != nil {
		return ExecResult{}, err
	}
	if status != http.StatusOK {
		return ExecResult{}, statusError(status, data)
	}
	return decodeExecResult(data)
}

// decodeExecResult accepts only the full documented ExecResult shape. An
// incomplete or keepalive-prefixed answer is an error, never a zero success.
// The documented after-200 error envelope {"error": ..., "status": N} is
// reported as the API error it names.
func decodeExecResult(data []byte) (ExecResult, error) {
	trimmed := bytes.TrimSpace(data)
	var envelope struct {
		Error  json.RawMessage `json:"error"`
		Status int             `json:"status"`
	}
	if err := json.Unmarshal(trimmed, &envelope); err == nil &&
		len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		if envelope.Status >= 400 {
			return ExecResult{}, statusError(envelope.Status, data)
		}
		return ExecResult{}, errors.New("instabox exec returned an error document instead of a result")
	}
	var doc struct {
		Stdout      *string `json:"stdout"`
		Stderr      *string `json:"stderr"`
		ExitCode    *int    `json:"exitCode"`
		TimedOut    *bool   `json:"timedOut"`
		Truncated   *bool   `json:"truncated"`
		InvalidUTF8 bool    `json:"invalidUtf8"`
	}
	if err := json.Unmarshal(trimmed, &doc); err != nil {
		return ExecResult{}, errors.New("instabox exec returned an invalid result document")
	}
	if doc.Stdout == nil || doc.Stderr == nil || doc.ExitCode == nil || doc.TimedOut == nil || doc.Truncated == nil {
		return ExecResult{}, errors.New("instabox exec returned an incomplete result document")
	}
	return ExecResult{
		Stdout:      *doc.Stdout,
		Stderr:      *doc.Stderr,
		ExitCode:    *doc.ExitCode,
		TimedOut:    *doc.TimedOut,
		Truncated:   *doc.Truncated,
		InvalidUTF8: doc.InvalidUTF8,
	}, nil
}

// ExecOK reports a completed command that produced trustworthy text output.
// A timeout, a signal or a truncated/undecodable stream is not a verified
// readback.
func (r ExecResult) ExecOK() bool {
	return r.ExitCode == 0 && !r.TimedOut && !r.Truncated && !r.InvalidUTF8
}

func (c *APIClient) do(ctx context.Context, method, path string, timeout time.Duration) ([]byte, error) {
	data, status, err := c.doRequestBody(ctx, method, path, nil, timeout, maxBodySize)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, statusError(status, data)
	}
	return data, nil
}

// doRequestBody sends one bounded request. A non-nil body is JSON, and the API
// key stays an HTTP header: it is never part of the body.
func (c *APIClient) doRequestBody(ctx context.Context, method, path string, body []byte, timeout time.Duration, limit int64) ([]byte, int, error) {
	if limit <= 0 {
		limit = maxBodySize
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, 0, errors.New("instabox request could not be built")
	}
	req.Header.Set("X-API-Key", c.key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, errors.New("instabox API request failed")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, 0, errors.New("instabox API response could not be read")
	}
	return data, resp.StatusCode, nil
}

func statusError(status int, data []byte) error {
	apiErr := &APIError{Status: status}
	var body struct {
		Error struct {
			Code      string `json:"code"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &body); err == nil {
		apiErr.Code = strings.TrimSpace(body.Error.Code)
		apiErr.Retryable = body.Error.Retryable
	}
	return apiErr
}

func decodeMachine(data []byte) (Machine, error) {
	var machine Machine
	if err := json.Unmarshal(data, &machine); err != nil {
		return Machine{}, errors.New("instabox API returned an invalid machine document")
	}
	if machine.ID == "" || machine.State == "" {
		return Machine{}, errors.New("instabox API returned an incomplete machine document")
	}
	return machine, nil
}
