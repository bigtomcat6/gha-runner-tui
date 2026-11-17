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
