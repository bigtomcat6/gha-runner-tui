package github

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"gha-runner-tui/internal/config"
)

const apiVersion = "2026-03-10"

var ErrAuthChanged = errors.New("AUTH_CHANGED")
var ErrWriteEvidenceUnavailable = errors.New("protected management write evidence unavailable")
var ErrRateLimited = errors.New("github rate window unavailable")

type AuthIdentity struct{ CredentialID, Generation string }
type WriteProof struct {
	Auth                        AuthIdentity
	Target                      config.ResolvedTarget
	ScopeDigest, EvidenceDigest string
	VerifiedAt, ExpiresAt       time.Time
}
type RateBook struct {
	requestMu sync.Mutex
	mu        sync.Mutex
	Entries   map[string]RateEntry
}
type RateEntry struct {
	Next     time.Time
	Failures int
	Isolated bool
}

type cacheKey struct {
	Auth         AuthIdentity
	URL, Version string
}
type cacheEntry struct {
	Body       []byte
	ETag, Link string
}
type transport struct {
	mu      sync.Mutex
	base    *url.URL
	ref     config.CredentialRef
	source  string
	secret  Secret
	auth    AuthIdentity
	invalid bool
	http    HTTPDoer
	book    *RateBook
	now     func() time.Time
	cache   map[cacheKey]cacheEntry
}

func NewScopedClient(baseURL string, ref config.CredentialRef, sources CredentialIO, httpClient HTTPDoer, book *RateBook, now func() time.Time) (Client, error) {
	return newScopedClient(baseURL, ref, sources, httpClient, book, now, rand.Reader)
}

func newScopedClient(baseURL string, ref config.CredentialRef, sources CredentialIO, httpClient HTTPDoer, book *RateBook, now func() time.Time, random io.Reader) (Client, error) {
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	base, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return Client{}, errors.New("invalid github API origin")
	}
	if ref.ID == "" || ref.ResourceOwner == "" {
		return Client{}, errors.New("invalid credential identity")
	}
	secret, source, err := ResolveCredential(ref, sources)
	if err != nil {
		return Client{}, err
	}
	var generation [32]byte
	if random == nil {
		clear(secret.value)
		return Client{}, errors.New("github authentication generation unavailable")
	}
	if _, err := io.ReadFull(random, generation[:]); err != nil {
		clear(secret.value)
		return Client{}, errors.New("github authentication generation unavailable")
	}
	if now == nil {
		now = time.Now
	}
	if book == nil {
		book = &RateBook{}
	}
	book.mu.Lock()
	if book.Entries == nil {
		book.Entries = make(map[string]RateEntry)
	}
	book.mu.Unlock()
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if client, ok := httpClient.(*http.Client); ok {
		copyClient := *client
		previous := client.CheckRedirect
		copyClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) > 0 && via[0].Method != http.MethodGet && via[0].Method != http.MethodHead {
				return errors.New("github mutating redirect rejected")
			}
			if !sameOrigin(base, req.URL) {
				return errors.New("github redirect origin rejected")
			}
			if len(via) >= 10 {
				return errors.New("github redirect limit exceeded")
			}
			if previous != nil {
				return previous(req, via)
			}
			return nil
		}
		httpClient = &copyClient
	}
	ref.Repositories = slices.Clone(ref.Repositories)
	t := &transport{base: base, ref: ref, source: source, secret: secret, auth: AuthIdentity{ref.ID, hex.EncodeToString(generation[:])}, http: httpClient, book: book, now: now, cache: make(map[cacheKey]cacheEntry)}
	return Client{baseURL: base.String(), transport: t, observations: &observationState{cursors: make(map[ObservationKey]*repoCursor), first: make(map[observedJobKey]time.Time)}}, nil
}

func (c Client) Auth() AuthIdentity {
	if c.transport == nil {
		return AuthIdentity{}
	}
	return c.transport.auth
}

func (c Client) checkValid() error {
	if c.initErr != nil {
		return c.initErr
	}
	if c.transport == nil {
		return ErrAuthChanged
	}
	c.transport.mu.Lock()
	defer c.transport.mu.Unlock()
	if c.transport.invalid {
		return ErrAuthChanged
	}
	return nil
}

func (c Client) CheckCurrent(ref config.CredentialRef, sources CredentialIO) error {
	t := c.transport
	if t == nil {
		return ErrAuthChanged
	}
	t.mu.Lock()
	defer func() {
		invalid := t.invalid
		t.mu.Unlock()
		if invalid && c.observations != nil {
			c.observations.mu.Lock()
			clear(c.observations.cursors)
			clear(c.observations.first)
			c.observations.mu.Unlock()
		}
	}()
	if t.invalid {
		return ErrAuthChanged
	}
	secret, source, err := ResolveCredential(ref, sources)
	defer clear(secret.value)
	if err != nil || !reflect.DeepEqual(ref, t.ref) || source != t.source || !bytes.Equal(secret.value, t.secret.value) {
		t.invalid = true
		clear(t.secret.value)
		clear(t.cache)
		return ErrAuthChanged
	}
	return nil
}

func (c Client) ManagementWriteProof(ctx context.Context, target config.ResolvedTarget, scopeDigest string) (WriteProof, error) {
	t := c.transport
	if t == nil {
		return WriteProof{}, ErrAuthChanged
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.invalid {
		return WriteProof{}, ErrAuthChanged
	}
	if err := ctx.Err(); err != nil {
		return WriteProof{}, errors.New("github write proof check canceled")
	}
	// No authorized V4 protected evidence is available. Neither a management
	// GET nor a caller-supplied digest can establish or rebind write authority.
	// Fail closed rather than invent an evidence source or permission experiment.
	return WriteProof{}, ErrWriteEvidenceUnavailable
}

func (c Client) WriteRateReady(now time.Time) bool {
	if c.transport == nil || c.initErr != nil {
		return false
	}
	t := c.transport
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.invalid && t.rateReady(now)
}

func (t *transport) rateReady(now time.Time) bool {
	t.book.mu.Lock()
	defer t.book.mu.Unlock()
	for key, e := range t.book.Entries {
		if strings.HasPrefix(key, t.origin()+"|") && (e.Isolated || now.Before(e.Next)) {
			return false
		}
	}
	return true
}
func (t *transport) origin() string { return strings.ToLower(t.base.Scheme + "://" + t.base.Host) }
func sameOrigin(base, u *url.URL) bool {
	return u != nil && u.User == nil && u.Fragment == "" && strings.EqualFold(base.Scheme, u.Scheme) && strings.EqualFold(base.Host, u.Host)
}

func (t *transport) request(ctx context.Context, method, path string, body any) ([]byte, error) {
	return t.requestLinked(ctx, method, path, body, nil)
}

// Return pagination metadata atomically with its body, including cached 304s.
func (t *transport) requestLinked(ctx context.Context, method, path string, body any, linkOut *string) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.invalid {
		return nil, ErrAuthChanged
	}
	t.book.requestMu.Lock()
	defer t.book.requestMu.Unlock()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("github request canceled: %w", ctx.Err())
	}
	if !t.rateReady(t.now()) {
		return nil, ErrRateLimited
	}
	u, err := url.Parse(path)
	if err != nil {
		return nil, errors.New("invalid github request URL")
	}
	if !u.IsAbs() && !strings.HasPrefix(path, "//") {
		// Existing callers pass /repos/... relative to the configured API base,
		// including enterprise bases such as /api/v3, not the origin root.
		base := *t.base
		base.Path = strings.TrimRight(base.Path, "/") + "/"
		base.RawPath = ""
		u, err = url.Parse(strings.TrimLeft(path, "/"))
		if err != nil {
			return nil, errors.New("invalid github request URL")
		}
		u = base.ResolveReference(u)
	} else {
		u = t.base.ResolveReference(u)
	}
	if !sameOrigin(t.base, u) {
		return nil, errors.New("github request origin rejected")
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, errors.New("invalid github request body")
		}
		reader = bytes.NewReader(data)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, errors.New("invalid github request")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("Authorization", "Bearer "+string(t.secret.value))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	key := cacheKey{Auth: t.auth, URL: u.String(), Version: apiVersion}
	cached, hasCache := t.cache[key]
	cacheable := method == http.MethodGet && !strings.Contains(u.Path, "registration-token") && !strings.Contains(u.Path, "remove-token")
	if cacheable && hasCache && cached.ETag != "" {
		req.Header.Set("If-None-Match", cached.ETag)
	}
	resp, err := t.http.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, safeRequestError("github request failed", err, ctx)
	}
	if resp == nil {
		return nil, errors.New("invalid github response")
	}
	if resp.Body != nil {
		defer resp.Body.Close()
	}
	if resp.Request != nil && !sameOrigin(t.base, resp.Request.URL) {
		return nil, errors.New("github response origin rejected")
	}
	// Persist header bounds even if reading the body fails. Count each response
	// once; body-only secondary evidence is considered only if headers did not limit it.
	limited := t.recordRate(resp, nil)
	if resp.Body == nil {
		return nil, errors.New("invalid github response")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024+1))
	if err != nil {
		return nil, safeRequestError("github response unreadable", err, ctx)
	}
	if len(data) > 16*1024*1024 {
		return nil, errors.New("github response unreadable")
	}
	if !limited {
		limited = t.recordRate(resp, data)
	}
	link := resp.Header.Get("Link")
	if err := t.checkLinks(u, link); err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotModified {
		if !cacheable || !hasCache || cached.ETag == "" {
			return nil, errors.New("github 304 without current authentication cache")
		}
		if !limited {
			t.recordRecovery(resp)
		}
		if linkOut != nil {
			*linkOut = cached.Link
		}
		return slices.Clone(cached.Body), nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("github API returned status %d", resp.StatusCode)
	}
	if !limited {
		t.recordRecovery(resp)
	}
	if cacheable {
		if etag := resp.Header.Get("ETag"); etag != "" {
			t.cache[key] = cacheEntry{Body: slices.Clone(data), ETag: etag, Link: link}
		} else {
			delete(t.cache, key)
		}
	} else if method != http.MethodGet {
		clear(t.cache)
	}
	if linkOut != nil {
		*linkOut = link
	}
	return data, nil
}

// Preserve only standard cancellation sentinels, never a transport URL/body.
func safeRequestError(message string, err error, ctx context.Context) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%s: %w", message, ctx.Err())
	}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, cause) {
			return fmt.Errorf("%s: %w", message, cause)
		}
	}
	return errors.New(message)
}

func (t *transport) checkLinks(current *url.URL, link string) error {
	for _, part := range strings.Split(link, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		start := strings.IndexByte(part, '<')
		end := strings.IndexByte(part, '>')
		if start != 0 || end <= start {
			return errors.New("invalid github pagination link")
		}
		u, err := url.Parse(part[start+1 : end])
		if err != nil || !sameOrigin(t.base, current.ResolveReference(u)) {
			return errors.New("github pagination origin rejected")
		}
	}
	return nil
}

func (t *transport) recordRate(resp *http.Response, body []byte) bool {
	now := t.now()
	retryAfter := time.Duration(0)
	if seconds, err := strconv.ParseInt(resp.Header.Get("Retry-After"), 10, 64); err == nil && seconds > 0 && seconds <= int64((1<<63-1)/int64(time.Second)) {
		retryAfter = time.Duration(seconds) * time.Second
	} else if date, err := http.ParseTime(resp.Header.Get("Retry-After")); err == nil && date.After(now) {
		retryAfter = date.Sub(now)
	}
	remaining := -1
	if n, err := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining")); err == nil {
		remaining = n
	}
	reset := time.Time{}
	if seconds, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		reset = time.Unix(seconds, 0)
	}
	message := strings.ToLower(string(body))
	secondary := (resp.StatusCode == 403 || resp.StatusCode == 429) && (resp.StatusCode == 429 || retryAfter > 0 || strings.Contains(message, "secondary") || strings.Contains(message, "abuse") || strings.Contains(message, "rate limit"))
	limited := resp.StatusCode == 429 || secondary || retryAfter > 0 || remaining == 0
	if !limited {
		return false
	}
	resource := resp.Header.Get("X-RateLimit-Resource")
	if resource == "" {
		resource = "core"
	}
	// Conservatively share each origin/resource across ALL owners and refs:
	// without token fingerprints we must not assume independent token quotas.
	key := t.origin() + "|" + resource
	t.book.mu.Lock()
	defer t.book.mu.Unlock()
	entry := t.book.Entries[key]
	delay := time.Minute * time.Duration(1<<min(entry.Failures, 4))
	next := now.Add(delay)
	if now.Add(retryAfter).After(next) {
		next = now.Add(retryAfter)
	}
	if remaining == 0 && reset.After(next) {
		next = reset
	}
	jitter := time.Duration(0) // Zero jitter is nonnegative and never shortens server bounds.
	next = next.Add(jitter)
	if entry.Next.After(next) {
		next = entry.Next
	}
	entry.Next = next
	entry.Failures++
	entry.Isolated = entry.Failures >= 5
	t.book.Entries[key] = entry
	return true
}

func (t *transport) recordRecovery(resp *http.Response) {
	resource := resp.Header.Get("X-RateLimit-Resource")
	if resource == "" {
		resource = "core"
	}
	key := t.origin() + "|" + resource
	t.book.mu.Lock()
	defer t.book.mu.Unlock()
	entry, exists := t.book.Entries[key]
	// A success for this resource after its bound confirms recovery, not a
	// license to shorten an active window or undo five-failure isolation.
	if exists && !entry.Isolated && !t.now().Before(entry.Next) {
		entry.Failures = 0
		t.book.Entries[key] = entry
	}
}
