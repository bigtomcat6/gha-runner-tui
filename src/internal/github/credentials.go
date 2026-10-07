package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"gha-runner-tui/internal/config"
)

type CredentialIO struct {
	LookupEnv func(string) (string, bool)
	ReadFile  func(string) ([]byte, error)
}

type Secret struct{ value []byte }

func (s Secret) String() string               { return "[redacted]" }
func (s Secret) Bytes() []byte                { return append([]byte(nil), s.value...) }
func (s Secret) Format(f fmt.State, _ rune)   { _, _ = io.WriteString(f, "[redacted]") }
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

type RegistrationToken struct {
	value     Secret
	ExpiresAt time.Time
}

func NewRegistrationToken(value []byte, expiresAt time.Time) (RegistrationToken, error) {
	if len(value) == 0 || expiresAt.IsZero() {
		return RegistrationToken{}, errors.New("invalid registration token or expiry")
	}
	return RegistrationToken{value: Secret{value: slices.Clone(value)}, ExpiresAt: expiresAt}, nil
}
func (t RegistrationToken) Bytes() []byte              { return t.value.Bytes() }
func (t RegistrationToken) String() string             { return "[redacted]" }
func (t RegistrationToken) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, "[redacted]") }
func (t RegistrationToken) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ExpiresAt time.Time `json:"expires_at"`
		Redacted  string    `json:"redacted"`
	}{t.ExpiresAt, "[redacted]"})
}
func (t *RegistrationToken) Clear() {
	if t == nil {
		return
	}
	clear(t.value.value)
	*t = RegistrationToken{}
}

// ChooseCredential inherits global sources only for an organization with no
// explicit source. A partial/invalid explicit source never inherits global PATs.
func ChooseCredential(g config.GitHubConfig, p config.Profile) (config.CredentialRef, error) {
	target, err := p.ResolveTarget()
	if err != nil {
		return config.CredentialRef{}, errors.New("invalid credential target")
	}
	r := config.CredentialRef{ID: p.GitHub.CredentialID, ResourceOwner: p.GitHub.ResourceOwner, TokenEnv: p.GitHub.TokenEnv, TokenFile: p.GitHub.TokenFile, EnvFile: p.GitHub.EnvFile, Repositories: slices.Clone(p.Scheduler.Repositories)}
	owner := target.Owner
	if target.Scope == config.TargetScopeOrganization {
		owner = target.OrgSlug
	}
	if r.ID == "" || r.ResourceOwner == "" || !strings.EqualFold(r.ResourceOwner, owner) {
		return config.CredentialRef{}, errors.New("invalid credential identity or owner")
	}
	if r.TokenEnv == "" && r.TokenFile == "" && r.EnvFile == "" {
		if target.Scope != config.TargetScopeOrganization {
			return config.CredentialRef{}, ErrMissingToken
		}
		r.TokenEnv, r.EnvFile = g.TokenEnv, g.EnvFile
	}
	if r.TokenEnv == "" && r.TokenFile == "" && r.EnvFile == "" {
		return config.CredentialRef{}, ErrMissingToken
	}
	if !validEnvName(r.TokenEnv) {
		return config.CredentialRef{}, errors.New("invalid credential environment reference")
	}
	for _, path := range []string{r.TokenFile, r.EnvFile} {
		if path != "" && (!filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsFunc(path, unicode.IsControl)) {
			return config.CredentialRef{}, errors.New("invalid credential file reference")
		}
	}
	return r, nil
}

func validEnvName(name string) bool {
	for i, r := range name {
		if r != '_' && !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func ResolveCredential(ref config.CredentialRef, sources CredentialIO) (Secret, string, error) {
	if !validEnvName(ref.TokenEnv) {
		return Secret{}, "env", errors.New("invalid credential environment reference")
	}
	if sources.LookupEnv == nil {
		sources.LookupEnv = os.LookupEnv
	}
	if sources.ReadFile == nil {
		sources.ReadFile = os.ReadFile
	}
	if ref.TokenEnv != "" {
		if value, ok := sources.LookupEnv(ref.TokenEnv); ok && strings.TrimSpace(value) != "" {
			return credentialSecret(value, "env")
		}
	}
	path, source := ref.TokenFile, "token_file"
	if path == "" {
		path, source = ref.EnvFile, "env_file"
	}
	if path == "" {
		return Secret{}, "env", ErrMissingToken
	}
	data, err := sources.ReadFile(path)
	if err != nil {
		return Secret{}, source, fmt.Errorf("github credential %s unavailable", source)
	}
	defer clear(data)
	key := ref.TokenEnv
	if key == "" {
		key = "GITHUB_TOKEN"
	} // Parsing a key is not an environment lookup.
	value := parseCredentialFile(string(data), key, source == "token_file")
	return credentialSecret(value, source)
}

func credentialSecret(value, source string) (Secret, string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return Secret{}, source, ErrMissingToken
	}
	if strings.ContainsAny(value, "`$") || strings.ContainsFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return Secret{}, source, errors.New("github credential source invalid")
	}
	return Secret{value: []byte(value)}, source, nil
}

// Parse data only; never evaluate shell commands, expansions, or source files.
// Legacy token files may contain either a raw token or a simple KEY=value.
func parseCredentialFile(content, key string, allowRaw bool) string {
	content = strings.TrimSpace(content)
	if allowRaw && !strings.ContainsAny(content, "\n=") {
		return content
	}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		name, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(name) != key {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '\'' || value[0] == '"') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		return value
	}
	return ""
}
