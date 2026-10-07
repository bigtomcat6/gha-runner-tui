package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gha-runner-tui/internal/config"
)

func TestCredentialNoImplicitGlobal(t *testing.T) {
	io := CredentialIO{
		LookupEnv: func(k string) (string, bool) { return "org-secret", k == "GITHUB_TOKEN" },
		ReadFile:  func(p string) ([]byte, error) { return nil, errors.New("denied org-secret") },
	}
	ref := config.CredentialRef{ID: "personal", ResourceOwner: "person", TokenFile: "missing"}
	secret, source, err := ResolveCredential(ref, io)
	if err == nil || len(secret.Bytes()) != 0 {
		t.Fatal("explicit personal source must fail closed")
	}
	if secret.String() != "[redacted]" || strings.Contains(err.Error()+source, "org-secret") {
		t.Fatal("unsafe source diagnostic")
	}
}

func TestCredentialSourcesAndChoice(t *testing.T) {
	g := config.GitHubConfig{TokenEnv: "GLOBAL", EnvFile: "/global.env"}
	p := config.Profile{Target: config.TargetConfig{Scope: config.TargetScopeOrganization, Org: "example"}, GitHub: config.GitHubProfile{CredentialID: "org", ResourceOwner: "example"}}
	ref, err := ChooseCredential(g, p)
	if err != nil || ref.TokenEnv != "GLOBAL" || ref.EnvFile != "/global.env" {
		t.Fatalf("org global choice: %+v %v", ref, err)
	}
	p.Target = config.TargetConfig{Scope: config.TargetScopeRepository, Owner: "example", Repo: "repo"}
	if _, err := ChooseCredential(g, p); err == nil {
		t.Fatal("personal profile needs explicit sources")
	}
	p.GitHub.TokenFile = "/personal"
	ref, err = ChooseCredential(g, p)
	if err != nil || ref.TokenEnv != "" || ref.EnvFile != "" || ref.TokenFile != "/personal" {
		t.Fatalf("file-only inherited global source: %+v %v", ref, err)
	}
	p.GitHub.ResourceOwner = "other"
	if _, err := ChooseCredential(g, p); err == nil {
		t.Fatal("wrong resource owner accepted")
	}
	for _, tc := range []struct {
		name, env, file, envFile, want, source string
		denied                                 bool
	}{
		{"env-first", "env-secret", "file-secret", "GITHUB_TOKEN=envfile-secret", "env-secret", "env", false},
		{"file-next", "", "file-secret", "GITHUB_TOKEN=envfile-secret", "file-secret", "token_file", false},
		{"env-file", "", "", "export TOKEN='envfile-secret'\nOTHER=ignored", "envfile-secret", "env_file", false},
		{"env-file-default-key", "", "", "GITHUB_TOKEN=envfile-secret", "envfile-secret", "env_file", false},
		{"file-error-no-fallback", "", "file-secret", "GITHUB_TOKEN=envfile-secret", "", "token_file", true},
		{"empty-file-no-fallback", "", " ", "GITHUB_TOKEN=envfile-secret", "", "token_file", false},
		{"never-source-shell", "", "", "GITHUB_TOKEN=$(touch forbidden)", "", "env_file", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := config.CredentialRef{ID: "personal", ResourceOwner: "example", EnvFile: "/env"}
			if tc.name != "env-file-default-key" && tc.name != "never-source-shell" {
				r.TokenEnv = "TOKEN"
			}
			if tc.file != "" {
				r.TokenFile = "/token"
			}
			reads := []string{}
			s, source, err := ResolveCredential(r, CredentialIO{LookupEnv: func(k string) (string, bool) {
				if k != "TOKEN" {
					t.Fatalf("implicit environment lookup %s", k)
				}
				return tc.env, tc.env != ""
			}, ReadFile: func(path string) ([]byte, error) {
				reads = append(reads, path)
				if path == "/token" {
					if tc.denied {
						return nil, errors.New("file-secret")
					}
					return []byte(tc.file), nil
				}
				return []byte(tc.envFile), nil
			}})
			if source != tc.source || string(s.Bytes()) != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("source=%s bytes=%q err=%v", source, s.Bytes(), err)
			}
			if tc.source == "env" && len(reads) != 0 {
				t.Fatal("read lower-priority source")
			}
			if tc.source == "token_file" && !reflect.DeepEqual(reads, []string{"/token"}) {
				t.Fatal("file failure fell back")
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatal("secret in error")
			}
		})
	}
}

func TestRegistrationTokenExpiryAndRedaction(t *testing.T) {
	expires := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	input := []byte("registration-secret")
	token, err := NewRegistrationToken(input, expires)
	if err != nil {
		t.Fatal(err)
	}
	input[0] = 'X'
	copyBytes := token.Bytes()
	copyBytes[0] = 'Y'
	if string(token.Bytes()) != "registration-secret" {
		t.Fatal("token aliases input/output")
	}
	secret := Secret{value: []byte("PAT-secret")}
	secretCopy := secret.Bytes()
	secretCopy[0] = 'X'
	if string(secret.Bytes()) != "PAT-secret" {
		t.Fatal("Secret.Bytes aliases private storage")
	}
	for _, value := range []any{secret, token, &secret, &token} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
			if got := fmt.Sprintf(verb, value); got != "[redacted]" {
				t.Fatalf("unsafe format %s: %s", verb, got)
			}
		}
		data, err := json.Marshal(value)
		if err != nil || strings.Contains(string(data), "secret") {
			t.Fatalf("unsafe JSON %s %v", data, err)
		}
	}
	data, _ := json.Marshal(token)
	if !strings.Contains(string(data), `"expires_at":"2099-01-01T00:00:00Z"`) {
		t.Fatal("expiry metadata missing")
	}
	for _, tc := range []struct {
		bytes   []byte
		expires time.Time
	}{{nil, expires}, {[]byte("abc"), time.Time{}}} {
		if _, err := NewRegistrationToken(tc.bytes, tc.expires); err == nil {
			t.Fatal("invalid token accepted")
		}
	}
	alias := token
	token.Clear()
	if len(token.Bytes()) != 0 || !token.ExpiresAt.IsZero() {
		t.Fatal("Clear did not reset token")
	}
	for _, b := range alias.Bytes() {
		if b != 0 {
			t.Fatal("Clear did not erase private storage")
		}
	}
}

type forbiddenCredentialRunner struct{ t *testing.T }

func (r forbiddenCredentialRunner) Run(context.Context, string, ...string) ([]byte, error) {
	r.t.Fatal("credential resolver invoked command/sudo fallback")
	return nil, nil
}

func TestLegacyResolverFileOnlyAndSafeErrors(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "fake-global-PAT")
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("fake-personal-PAT"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	c := NewClient("https://example.test", "", path, forbiddenCredentialRunner{t}, doerFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Authorization") != "Bearer fake-personal-PAT" {
			t.Fatal("file-only client used global PAT")
		}
		return reply(400, "fake-personal-PAT fake-global-PAT private-body", nil), nil
	}))
	if _, err := c.ListRepoRunners(context.Background(), "alice", "repo"); err == nil || strings.Contains(err.Error(), "fake-") || strings.Contains(err.Error(), "private-body") {
		t.Fatalf("unsafe legacy error %v", err)
	}
	if calls != 1 {
		t.Fatal("legacy request missing")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListRepoRunners(context.Background(), "alice", "repo"); !errors.Is(err, ErrAuthChanged) {
		t.Fatal("legacy snapshot not rechecked")
	}
	missing := NewClient("https://example.test", "", path, forbiddenCredentialRunner{t}, doerFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("missing file fell back to global environment")
		return nil, nil
	}))
	if _, err := missing.ListRepoRunners(context.Background(), "alice", "repo"); err == nil {
		t.Fatal("missing source did not fail closed")
	}
}

func TestCredentialChoiceRejectsInvalidExplicitSources(t *testing.T) {
	for _, source := range []config.GitHubProfile{
		{TokenEnv: "INVALID KEY"},
		{TokenFile: "relative"},
		{EnvFile: "/token/../env"},
	} {
		source.CredentialID, source.ResourceOwner = "org", "example"
		p := config.Profile{Target: config.TargetConfig{Scope: config.TargetScopeOrganization, Org: "example"}, GitHub: source}
		if _, err := ChooseCredential(config.GitHubConfig{TokenEnv: "GLOBAL"}, p); err == nil {
			t.Fatal("invalid explicit source accepted")
		}
	}
}

func TestRegistrationResponseRequiresExpiry(t *testing.T) {
	t.Setenv("EXPIRY_TEST_TOKEN", "fake-PAT")
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"missing", `{"token":"fake-registration"}`, false},
		{"invalid", `{"token":"fake-registration","expires_at":"not-a-date"}`, false},
		{"zero", `{"token":"fake-registration","expires_at":"0001-01-01T00:00:00Z"}`, false},
		{"valid", `{"token":"fake-registration","expires_at":"2099-01-01T00:00:00Z"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, org := range []bool{false, true} {
				c := NewClient("https://example.test", "EXPIRY_TEST_TOKEN", "", nil, fakeHTTPDoer{response: &http.Response{StatusCode: 201, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}})
				var value string
				var err error
				if org {
					value, err = c.CreateOrgRegistrationToken(context.Background(), "example")
				} else {
					value, err = c.CreateRegistrationToken(context.Background(), "example", "repo")
				}
				if (err == nil) != tc.valid || (!tc.valid && value != "") || (tc.valid && value != "fake-registration") {
					t.Fatalf("value=%q err=%v", value, err)
				}
				if err != nil && strings.Contains(err.Error(), "fake-") {
					t.Fatal("unsafe expiry error")
				}
			}
		})
	}
}
