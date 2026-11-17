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
	maxBodySize = 1 << 20
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

// MachineAPI is the GET plus the single documented wake POST. There is no
// suspend, stop or delete path.
type MachineAPI interface {
	GetMachine(ctx context.Context, id string) (Machine, error)
	WakeMachine(ctx context.Context, id string) (Machine, error)
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
	data, status, err := c.doStatus(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(id)+"/wake", wakeTimeout)
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

func (c *APIClient) do(ctx context.Context, method, path string, timeout time.Duration) ([]byte, error) {
	data, status, err := c.doStatus(ctx, method, path, timeout)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, statusError(status, data)
	}
	return data, nil
}

func (c *APIClient) doStatus(ctx context.Context, method, path string, timeout time.Duration) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, nil)
	if err != nil {
		return nil, 0, errors.New("instabox request could not be built")
	}
	req.Header.Set("X-API-Key", c.key)
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, errors.New("instabox API request failed")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize))
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
