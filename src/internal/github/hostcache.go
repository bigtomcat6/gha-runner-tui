package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// hostCache lets every gha-ephemeral-loop process on one host share GitHub
// reads, so the user-wide REST quota does not scale with the number of loops:
//
//   - Conditional GETs: the last ETag and body of each GET path are kept on
//     disk. A 304 reply reuses the stored body and does not count against
//     GitHub's primary rate limit.
//   - Queued-job snapshots: per repository and identity, at most one process
//     per queueShareTTL scans the runs/jobs endpoints; the other loops reuse
//     the snapshot and apply their own label filter.
//
// The cache is an optimisation only. Any cache I/O problem falls back to a
// direct request, entries are keyed by API base URL, a token fingerprint and
// the request path (never the token itself), and failed responses are never
// stored or shared.
type hostCache struct {
	dir string
	ttl time.Duration
	now func() time.Time
}

const (
	// DefaultHostCacheDir is the per-host cache shared by loop processes. It
	// sits next to the host capacity lock and is cleared on reboot.
	DefaultHostCacheDir = "/run/gha-runner-tui/github"

	queueShareTTL          = 20 * time.Second
	conditionalMaxAge      = time.Hour
	conditionalMaxEntries  = 512
	conditionalMaxBodySize = 8 << 20
	queueLockPoll          = 100 * time.Millisecond
)

type conditionalEntry struct {
	ETag string          `json:"etag"`
	Body json.RawMessage `json:"body"`
}

type queueSnapshot struct {
	FetchedAt time.Time `json:"fetched_at"`
	Jobs      []Job     `json:"jobs"`
}

func newHostCache(dir string) *hostCache {
	if dir == "" {
		return nil
	}
	return &hostCache{dir: dir, ttl: queueShareTTL, now: time.Now}
}

// key derives a file name for one identity and request. The token only enters
// through its SHA-256, so a cache file never reveals it.
func (h *hostCache) key(baseURL, token, request string) string {
	tokenSum := sha256.Sum256([]byte(token))
	sum := sha256.Sum256([]byte(baseURL + "\x00" + hex.EncodeToString(tokenSum[:]) + "\x00" + request))
	return hex.EncodeToString(sum[:16])
}

// subdir returns a private (0700, owned by this user, not a symlink) cache
// directory. Anything else is refused so another user cannot plant entries.
func (h *hostCache) subdir(name string) (string, error) {
	for _, dir := range []string{h.dir, filepath.Join(h.dir, name)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return "", err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("github cache path %s is not a directory", dir)
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(st.Uid) != os.Geteuid() {
			return "", fmt.Errorf("github cache path %s is not owned by this user", dir)
		}
		if info.Mode().Perm()&0o077 != 0 {
			if err := os.Chmod(dir, 0o700); err != nil {
				return "", err
			}
		}
	}
	return filepath.Join(h.dir, name), nil
}

func (h *hostCache) loadConditional(key string) (conditionalEntry, bool) {
	dir, err := h.subdir("etag")
	if err != nil {
		return conditionalEntry{}, false
	}
	data, err := os.ReadFile(filepath.Join(dir, key+".json"))
	if err != nil {
		return conditionalEntry{}, false
	}
	var entry conditionalEntry
	if json.Unmarshal(data, &entry) != nil || entry.ETag == "" || len(entry.Body) == 0 {
		return conditionalEntry{}, false
	}
	return entry, true
}

// touchConditional keeps a still-valid entry from being pruned.
func (h *hostCache) touchConditional(key string) {
	dir, err := h.subdir("etag")
	if err != nil {
		return
	}
	now := h.now()
	_ = os.Chtimes(filepath.Join(dir, key+".json"), now, now)
}

func (h *hostCache) storeConditional(key, etag string, body []byte) {
	if etag == "" || len(body) == 0 || len(body) > conditionalMaxBodySize || !json.Valid(body) {
		return
	}
	dir, err := h.subdir("etag")
	if err != nil {
		return
	}
	data, err := json.Marshal(conditionalEntry{ETag: etag, Body: body})
	if err != nil {
		return
	}
	if writeFileAtomic(dir, key+".json", data) != nil {
		return
	}
	h.pruneConditional(dir)
}

// pruneConditional drops entries unused for conditionalMaxAge and keeps at
// most conditionalMaxEntries, so per-run job paths do not accumulate.
func (h *hostCache) pruneConditional(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) <= conditionalMaxEntries/2 {
		return
	}
	type file struct {
		name string
		mod  time.Time
	}
	cutoff := h.now().Add(-conditionalMaxAge)
	var kept []file
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
			continue
		}
		kept = append(kept, file{name: e.Name(), mod: info.ModTime()})
	}
	if len(kept) <= conditionalMaxEntries {
		return
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].mod.Before(kept[j].mod) })
	for _, f := range kept[:len(kept)-conditionalMaxEntries] {
		_ = os.Remove(filepath.Join(dir, f.name))
	}
}

// sharedQueue returns a fresh snapshot for key, or calls fetch under an
// exclusive per-key lock and stores its result. Concurrent loops for the same
// repository wait for that one fetch instead of querying GitHub themselves.
// When the lock or snapshot files cannot be used it simply calls fetch.
func (h *hostCache) sharedQueue(ctx context.Context, key string, fetch func() ([]Job, error)) ([]Job, error) {
	dir, err := h.subdir("queue")
	if err != nil {
		return fetch()
	}
	unlock, err := lockFile(ctx, filepath.Join(dir, key+".lock"))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return fetch()
	}
	defer unlock()

	path := filepath.Join(dir, key+".json")
	if data, err := os.ReadFile(path); err == nil {
		var snap queueSnapshot
		if json.Unmarshal(data, &snap) == nil {
			age := h.now().Sub(snap.FetchedAt)
			if age >= 0 && age < h.ttl {
				return snap.Jobs, nil
			}
		}
	}

	jobs, err := fetch()
	if err != nil {
		return nil, err
	}
	if data, err := json.Marshal(queueSnapshot{FetchedAt: h.now(), Jobs: jobs}); err == nil {
		_ = writeFileAtomic(dir, key+".json", data)
	}
	return jobs, nil
}

func (h *hostCache) invalidateQueue(key string) {
	dir, err := h.subdir("queue")
	if err != nil {
		return
	}
	_ = os.Remove(filepath.Join(dir, key+".json"))
}

func (h *hostCache) invalidateAllQueues() {
	dir, err := h.subdir("queue")
	if err != nil {
		return
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	for _, m := range matches {
		_ = os.Remove(m)
	}
}

// lockFile takes an exclusive flock, polling until ctx is done.
func lockFile(ctx context.Context, path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = f.Close()
			return nil, err
		}
		timer := time.NewTimer(queueLockPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = f.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func writeFileAtomic(dir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_, writeErr := tmp.Write(data)
	closeErr := tmp.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}
