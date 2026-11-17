package instaboxcontroller

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testMachineID = "m_aaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestAPIGetMachineParsesDocumentedFields(t *testing.T) {
	var method, path, key string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, key = r.Method, r.URL.Path, r.Header.Get("X-API-Key")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"id": "`+testMachineID+`",
			"name": "build-a",
			"desiredState": "running",
			"state": "running",
			"revision": 12,
			"operationId": null,
			"runtime": {"ready": true},
			"runStartedAt": "2026-11-17T10:00:00Z",
			"maxRunMinutes": 480,
			"cooldownUntil": null,
			"autoSuspendedAt": "2026-11-17T11:00:00Z"
		}`)
	}))
	defer server.Close()

	client := newAPIClient(server.URL, "ibk_example", server.Client())
	machine, err := client.GetMachine(context.Background(), testMachineID)
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodGet || path != "/v1/machines/"+testMachineID {
		t.Fatalf("request was %s %s", method, path)
	}
	if key != "ibk_example" {
		t.Fatal("X-API-Key was not sent")
	}
	if machine.ID != testMachineID || machine.State != "running" || machine.DesiredState != "running" {
		t.Fatalf("machine = %+v", machine)
	}
	if !machine.Runtime.Ready || machine.Runtime.HostUnreachable {
		t.Fatalf("runtime = %+v", machine.Runtime)
	}
	if machine.RunStartedAt == nil || *machine.RunStartedAt != "2026-11-17T10:00:00Z" {
		t.Fatalf("runStartedAt = %v", machine.RunStartedAt)
	}
	if machine.MaxRunMinutes == nil || *machine.MaxRunMinutes != 480 {
		t.Fatalf("maxRunMinutes = %v", machine.MaxRunMinutes)
	}
	if machine.AutoSuspendedAt == nil || *machine.AutoSuspendedAt != "2026-11-17T11:00:00Z" {
		t.Fatalf("autoSuspendedAt = %v", machine.AutoSuspendedAt)
	}
	if machine.CooldownUntil != nil {
		t.Fatalf("cooldownUntil = %v", machine.CooldownUntil)
	}
}

func TestAPIGetMachineErrorsAreBoundedAndDoNotLeakTheKey(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		code   string
	}{
		{name: "unauthorized", status: 401, body: `{"error":{"code":"invalid_key","message":"bad ibk_secret_value","retryable":false}}`, code: "invalid_key"},
		{name: "forbidden", status: 403, body: `{"error":{"code":"missing_scope","message":"ibk_secret_value"}}`, code: "missing_scope"},
		{name: "not found", status: 404, body: `{"error":{"code":"not_found"}}`, code: "not_found"},
		{name: "unavailable", status: 503, body: `{"error":{"code":"host_unavailable","retryable":true}}`, code: "host_unavailable"},
		{name: "html body", status: 500, body: `<html>ibk_secret_value</html>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer server.Close()
			client := newAPIClient(server.URL, "ibk_secret_value", server.Client())
			_, err := client.GetMachine(context.Background(), testMachineID)
			if err == nil {
				t.Fatal("expected an error")
			}
			if strings.Contains(err.Error(), "ibk_secret_value") || strings.Contains(err.Error(), "<html>") {
				t.Fatalf("error leaked the response body or key: %v", err)
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Status != tc.status {
				t.Fatalf("error = %v", err)
			}
			if apiErr.Code != tc.code {
				t.Fatalf("code = %q, want %q", apiErr.Code, tc.code)
			}
		})
	}
}

func TestAPIGetMachineRejectsInvalidDocuments(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "not json", body: `{`},
		{name: "empty object", body: `{}`},
		{name: "missing state", body: `{"id":"` + testMachineID + `"}`},
		{name: "different machine", body: `{"id":"m_bbbbbbbbbbbbbbbbbbbbbbbbbb","state":"running"}`},
		{name: "wrapper instead of machine", body: `{"machine":{"id":"` + testMachineID + `","state":"running"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, tc.body)
			}))
			defer server.Close()
			client := newAPIClient(server.URL, "ibk_example", server.Client())
			if _, err := client.GetMachine(context.Background(), testMachineID); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestAPIWakeMachine(t *testing.T) {
	var method, path, key string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, key = r.Method, r.URL.Path, r.Header.Get("X-API-Key")
		io.WriteString(w, `{"id":"`+testMachineID+`","state":"running","desiredState":"running","runtime":{"ready":true}}`)
	}))
	defer server.Close()
	client := newAPIClient(server.URL, "ibk_example", server.Client())
	machine, err := client.WakeMachine(context.Background(), testMachineID)
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || path != "/v1/machines/"+testMachineID+"/wake" {
		t.Fatalf("request was %s %s", method, path)
	}
	if key != "ibk_example" || machine.State != "running" {
		t.Fatalf("key=%q machine=%+v", key, machine)
	}
}

func TestAPIWakeMachineAcceptsAcceptedAndRejectsConflicts(t *testing.T) {
	accepted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer accepted.Close()
	client := newAPIClient(accepted.URL, "ibk_example", accepted.Client())
	if _, err := client.WakeMachine(context.Background(), testMachineID); err != nil {
		t.Fatalf("202 wake: %v", err)
	}

	conflict := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		io.WriteString(w, `{"error":{"code":"machine_cooldown","retryable":true,"retryAfterSeconds":300}}`)
	}))
	defer conflict.Close()
	client = newAPIClient(conflict.URL, "ibk_example", conflict.Client())
	_, err := client.WakeMachine(context.Background(), testMachineID)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict || apiErr.Code != "machine_cooldown" {
		t.Fatalf("conflict error = %v", err)
	}
}

func TestAPIClientUsesFixedProductionHost(t *testing.T) {
	client := NewAPIClient("ibk_example", nil)
	if client.baseURL != "https://api.instabox.sh" {
		t.Fatalf("baseURL = %q", client.baseURL)
	}
}

func TestAPIWithoutKeyMakesNoRequest(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	client := newAPIClient(server.URL, "", server.Client())
	if _, err := client.GetMachine(context.Background(), testMachineID); !errors.Is(err, errNoAPIKey) {
		t.Fatalf("GetMachine error = %v", err)
	}
	if _, err := client.WakeMachine(context.Background(), testMachineID); !errors.Is(err, errNoAPIKey) {
		t.Fatalf("WakeMachine error = %v", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("requests = %d, want 0", got)
	}
}

func TestAPITimeoutIsBounded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer server.Close()
	client := newAPIClient(server.URL, "ibk_example", server.Client())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if _, err := client.GetMachine(ctx, testMachineID); err == nil {
		t.Fatal("expected the cancelled request to fail")
	}
}

func TestMachineJSONFieldsRoundTrip(t *testing.T) {
	// A minimal guard against accidental renames of the documented fields.
	var machine Machine
	if err := json.Unmarshal([]byte(`{"id":"x","state":"y","runtime":{"ready":true},"maxRunMinutes":120,"cooldownUntil":"2026-11-17T10:00:00Z"}`), &machine); err != nil {
		t.Fatal(err)
	}
	if machine.CooldownUntil == nil || *machine.CooldownUntil != "2026-11-17T10:00:00Z" {
		t.Fatalf("cooldownUntil = %v", machine.CooldownUntil)
	}
}

// execJSON is the documented 200 result shape.
func execJSON(stdout, stderr string, exitCode int, timedOut, truncated bool) string {
	data, _ := json.Marshal(map[string]any{
		"stdout": stdout, "stderr": stderr, "exitCode": exitCode,
		"timedOut": timedOut, "truncated": truncated,
	})
	return string(data)
}

func TestAPIExecSendsTheDocumentedRequestAndParsesTheResult(t *testing.T) {
	var method, path, key, contentType, accept string
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		key, contentType, accept = r.Header.Get("X-API-Key"), r.Header.Get("Content-Type"), r.Header.Get("Accept")
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		if strings.Contains(string(raw), "ibk_secret_value") {
			t.Errorf("the API key was sent in the body")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, execJSON("payload\n", "", 0, false, false))
	}))
	defer server.Close()

	client := newAPIClient(server.URL, "ibk_secret_value", server.Client())
	result, err := client.ExecCommand(context.Background(), testMachineID, "cat -- '/data/x'")
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || path != "/v1/machines/"+testMachineID+"/exec" {
		t.Fatalf("request was %s %s", method, path)
	}
	if key != "ibk_secret_value" || contentType != "application/json" || accept != "application/json" {
		t.Fatalf("key=%q content-type=%q accept=%q", key, contentType, accept)
	}
	if got := body["cmd"]; got != "cat -- '/data/x'" {
		t.Fatalf("cmd = %v", got)
	}
	if got := body["timeoutSeconds"]; got != float64(execCommandSeconds) {
		t.Fatalf("timeoutSeconds = %v", got)
	}
	if got := body["noHistory"]; got != true {
		t.Fatalf("noHistory = %v", got)
	}
	// Only documented fields; no stdin, envs or secret material.
	for field := range body {
		switch field {
		case "cmd", "timeoutSeconds", "noHistory":
		default:
			t.Fatalf("unexpected request field %q", field)
		}
	}
	if result.Stdout != "payload\n" || result.ExitCode != 0 || !result.ExecOK() {
		t.Fatalf("result = %+v", result)
	}
}

func TestAPIExecNonzeroAndTimeoutAreResultsNotSuccess(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		wantExit int
		timedOut bool
	}{
		{name: "nonzero exit", body: execJSON("", "denied", 1, false, false), wantExit: 1},
		{name: "timed out", body: execJSON("", "", 124, true, false), wantExit: 124, timedOut: true},
		{name: "truncated output", body: execJSON("x", "", 0, false, true)},
		{name: "invalid utf8 output", body: `{"stdout":"x","stderr":"","exitCode":0,"timedOut":false,"truncated":false,"invalidUtf8":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, tc.body)
			}))
			defer server.Close()
			result, err := newAPIClient(server.URL, "ibk_example", server.Client()).ExecCommand(context.Background(), testMachineID, "true")
			if err != nil {
				t.Fatalf("a non-zero exit or timeout is a 200 result, not an error: %v", err)
			}
			if result.ExitCode != tc.wantExit || result.TimedOut != tc.timedOut || result.ExecOK() {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestAPIExecRejectsIncompleteOrErrorDocuments(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		wantCode   string
		wantStatus int
	}{
		{name: "missing exit code", body: `{"stdout":"x","stderr":"","timedOut":false,"truncated":false}`},
		{name: "missing stdout", body: `{"stderr":"","exitCode":0,"timedOut":false,"truncated":false}`},
		{name: "empty body", body: ``},
		{name: "html body", body: `<html>ibk_secret_value</html>`},
		{name: "after 200 error envelope", body: `{"error":{"code":"host_unavailable","retryable":false},"status":503}`, wantCode: "host_unavailable", wantStatus: 503},
		{name: "forbidden scope", status: 403, body: `{"error":{"code":"missing_scope","message":"ibk_secret_value","requiredScopes":["machines:write"]}}`, wantCode: "missing_scope", wantStatus: 403},
		{name: "not running", status: 409, body: `{"error":{"code":"machine_not_running"}}`, wantCode: "machine_not_running", wantStatus: 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := tc.status
			if status == 0 {
				status = http.StatusOK
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				io.WriteString(w, tc.body)
			}))
			defer server.Close()
			result, err := newAPIClient(server.URL, "ibk_secret_value", server.Client()).ExecCommand(context.Background(), testMachineID, "true")
			if err == nil {
				t.Fatalf("incomplete answer was accepted as success: %+v", result)
			}
			if strings.Contains(err.Error(), "ibk_secret_value") || strings.Contains(err.Error(), "<html>") {
				t.Fatalf("error leaked the response body or key: %v", err)
			}
			if tc.wantCode != "" {
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.Status != tc.wantStatus || apiErr.Code != tc.wantCode {
					t.Fatalf("error = %v, want %d %s", err, tc.wantStatus, tc.wantCode)
				}
			}
		})
	}
}

func TestAPIExecAcceptsDocumentedKeepaliveWhitespace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "\n\n"+execJSON("payload\n", "", 0, false, false))
	}))
	defer server.Close()
	result, err := newAPIClient(server.URL, "ibk_example", server.Client()).ExecCommand(context.Background(), testMachineID, "true")
	if err != nil {
		t.Fatal(err)
	}
	if !result.ExecOK() || result.Stdout != "payload\n" {
		t.Fatalf("result = %+v", result)
	}
}

func TestAPIExecRejectsAnUnsafeMachineIDBeforeAnyRequest(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	client := newAPIClient(server.URL, "ibk_example", server.Client())
	for _, id := range []string{"", "m_bad", "m_aaaaaaaaaaaaaaaaaaaaaaaaaa/../../x", "build-a"} {
		if _, err := client.ExecCommand(context.Background(), id, "true"); err == nil {
			t.Fatalf("unsafe machine ID %q was accepted", id)
		}
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("requests = %d, want 0", got)
	}
}

func TestAPIExecTimeoutIsBounded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer server.Close()
	client := newAPIClient(server.URL, "ibk_example", server.Client())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if _, err := client.ExecCommand(ctx, testMachineID, "true"); err == nil {
		t.Fatal("expected the bounded request to fail")
	}
}

func TestAPIExecRejectsAnOversizedAnswer(t *testing.T) {
	// A body over the bounded reader is cut, so it cannot decode as a
	// complete documented result and must not be treated as success.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"stdout":"`)
		io.WriteString(w, strings.Repeat("a", maxExecBodySize+1024))
		io.WriteString(w, `","stderr":"","exitCode":0,"timedOut":false,"truncated":false}`)
	}))
	defer server.Close()
	if _, err := newAPIClient(server.URL, "ibk_example", server.Client()).ExecCommand(context.Background(), testMachineID, "true"); err == nil {
		t.Fatal("an oversized exec answer was accepted")
	}
}
