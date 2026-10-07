package evidence

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// ProductionRoot is the fixed host scheduler root; production constructors
// never accept a caller-provided root (spec 6.1).
const ProductionRoot = "/var/lib/gha-runner-tui/host-scheduler"

const (
	objectsSubdir = "objects"
	catalogSubdir = "catalog"
	ticketsSubdir = "tickets"
)

var sequenceName = regexp.MustCompile(`^[0-9]{20}\.json$`)

// evidenceDir returns the fixed v1 evidence tree under the scheduler root.
func evidenceDir(root string) string {
	return filepath.Join(root, "evidence", "v1")
}

// Reader is a read-only view over the fixed evidence tree. It never creates
// directories or syncs; trust/mount/namespace checks are injectable only for
// in-package offline tests (spec 6.1, 6.7).
type Reader struct {
	root           string
	trust          func(path string, info os.FileInfo) error
	mountNamespace func() error
	mountID        mountIDFn
	// life is held shared for the whole duration of every read and exclusive by
	// Close, so the retained root FD cannot be released under an in-flight read.
	life sync.RWMutex
	// mu serializes the one-time pin and the closed transition. It is always
	// acquired under the matching life lock.
	mu sync.Mutex
	// pinned is the retained, validated root identity shared by every read. A
	// Lease installs its root; a standalone reader opens one on first use and
	// keeps it for the reader's lifetime so calls and ref reads cannot be
	// redirected through a newly reopened path. It is only mutated under mu;
	// life keeps it alive for readers.
	pinned     *pinnedRoot
	ownsPinned bool
	closed     bool
}

// NewReader returns a reader over the production fixed root.
func NewReader() *Reader { return newReader(ProductionRoot) }

func newReader(root string) *Reader {
	return &Reader{root: root, trust: productionTrust, mountNamespace: productionNamespaceCheck, mountID: platformMountID}
}

func productionTrust(path string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("%w: unsafe evidence inode %s", ErrEvidenceTrust, path)
	}
	if !info.Mode().IsRegular() && !info.IsDir() {
		return fmt.Errorf("%w: non-regular evidence inode %s", ErrEvidenceTrust, path)
	}
	return nil
}

// pinNamespace returns the mount-namespace check for the fixed production root;
// test and other roots have no production namespace to assert.
func (r *Reader) pinNamespace() func() error {
	if r.root == ProductionRoot {
		return r.mountNamespace
	}
	return nil
}

// openPinned validates the fixed root pathname from scratch, returning a fresh
// pinned descriptor. It is the only place the pathname is resolved for reads.
func (r *Reader) openPinned() (*pinnedRoot, error) {
	return openPinnedRoot(r.root, r.trust, r.pinNamespace(), r.mountID)
}

// revalidatePinned re-resolves the fixed root pathname and requires it to still
// name the retained inode, mount identity and exact root protection (all checked
// by openPinnedRoot) before a read proceeds. Reads continue on the retained FD,
// so a swap after this check cannot redirect them; a disappearing or replaced
// source is an error instead of a stale success.
func (r *Reader) revalidatePinned(root *pinnedRoot) error {
	fresh, err := r.openPinned()
	if err != nil {
		return fmt.Errorf("%w: evidence root unavailable: %v", ErrEvidenceTrust, err)
	}
	defer fresh.Close()
	if fresh.dev != root.dev || fresh.ino != root.ino || fresh.mountID != root.mountID {
		return fmt.Errorf("%w: evidence root replaced", ErrEvidenceTrust)
	}
	return nil
}

// rootFor returns the retained pinned root for a read. A Lease shares its root;
// a standalone reader opens one on first use. Every current use revalidates the
// fixed pathname against the retained identity before reading through the pin,
// and the returned release holds the root alive until the read finishes.
func (r *Reader) rootFor() (*pinnedRoot, func(), error) {
	r.life.RLock()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		r.life.RUnlock()
		return nil, nil, fmt.Errorf("%w: reader closed", ErrEvidenceTrust)
	}
	root := r.pinned
	if root == nil {
		opened, err := r.openPinned()
		if err != nil {
			r.mu.Unlock()
			r.life.RUnlock()
			return nil, nil, err
		}
		r.pinned = opened
		r.ownsPinned = true
		root = opened
	} else if err := r.revalidatePinned(root); err != nil {
		r.mu.Unlock()
		r.life.RUnlock()
		return nil, nil, err
	}
	r.mu.Unlock()
	return root, r.life.RUnlock, nil
}

// Close waits for all in-flight reads, then releases the root this Reader opened
// itself. A lease-shared root is owned by the Lease and is left untouched (and
// usable), so its FD is never closed here.
func (r *Reader) Close() error {
	if r == nil {
		return nil
	}
	r.life.Lock()
	defer r.life.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.ownsPinned {
		// A lease-installed root is owned by the Lease and is left untouched
		// (and usable), so Close is a no-op for it. A standalone reader that
		// never opened a root owns no FD but must still transition to closed;
		// otherwise a later Object/Catalog would initialize a root after Close.
		if r.pinned == nil {
			r.closed = true
		}
		return nil
	}
	if r.pinned == nil || r.closed {
		return nil
	}
	r.closed = true
	err := r.pinned.Close()
	r.pinned = nil
	r.ownsPinned = false
	return err
}

// openUnder opens rel relative to the pinned root, one component at a time from
// the pinned parent. Every intermediate component must be a trusted 0700
// evidence directory; the final component is either such a directory (wantDir)
// or a 0600 nlink=1 regular file. No pathname lookup can follow an ancestor
// symlink because each step is openat O_NOFOLLOW on an already-pinned FD.
func (r *Reader) openUnder(root *pinnedRoot, rel string, wantDir bool) (*os.File, error) {
	relParts, err := splitRel(rel)
	if err != nil {
		return nil, err
	}
	parts := append([]string{"evidence", "v1"}, relParts...)
	dir := root.fd
	var opened []*os.File
	closeOpened := func() {
		for _, f := range opened {
			f.Close()
		}
	}
	for i, part := range parts {
		last := i == len(parts)-1
		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC
		if !last || wantDir {
			flags |= unix.O_DIRECTORY
		} else {
			flags |= unix.O_NONBLOCK
		}
		fd, err := unix.Openat(int(dir.Fd()), part, flags, 0)
		if err != nil {
			closeOpened()
			return nil, err
		}
		f := os.NewFile(uintptr(fd), part)
		info, err := f.Stat()
		if err != nil {
			f.Close()
			closeOpened()
			return nil, err
		}
		if !last {
			if !info.IsDir() || info.Mode().Perm() != evidenceDirMode {
				f.Close()
				closeOpened()
				return nil, fmt.Errorf("%w: unsafe evidence dir %s", ErrEvidenceTrust, part)
			}
			if r.trust != nil {
				if err := r.trust(part, info); err != nil {
					f.Close()
					closeOpened()
					return nil, err
				}
			}
			opened = append(opened, f)
			dir = f
			continue
		}
		if wantDir {
			if !info.IsDir() || info.Mode().Perm() != evidenceDirMode {
				f.Close()
				closeOpened()
				return nil, fmt.Errorf("%w: unsafe evidence dir %s", ErrEvidenceTrust, rel)
			}
			if r.trust != nil {
				if err := r.trust(rel, info); err != nil {
					f.Close()
					closeOpened()
					return nil, err
				}
			}
			closeOpened()
			return f, nil
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != evidenceFileMode {
			f.Close()
			closeOpened()
			return nil, fmt.Errorf("%w: unsafe evidence file %s", ErrEvidenceTrust, rel)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Nlink != 1 {
			f.Close()
			closeOpened()
			return nil, fmt.Errorf("%w: evidence nlink %s", ErrEvidenceTrust, rel)
		}
		if r.trust != nil {
			if err := r.trust(rel, info); err != nil {
				f.Close()
				closeOpened()
				return nil, err
			}
		}
		closeOpened()
		return f, nil
	}
	closeOpened()
	return nil, fmt.Errorf("%w: empty path", ErrEvidenceTrust)
}

func (r *Reader) readFile(root *pinnedRoot, rel string) ([]byte, error) {
	f, err := r.openUnder(root, rel, false)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxObjectBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxObjectBytes {
		return nil, fmt.Errorf("%w: evidence file too large", ErrEvidenceEncoding)
	}
	return data, nil
}

// publisherTemp matches only the same-directory temp names written by
// writeNoReplace (`.evidence-<decimal>`); those may be visible to a concurrent
// reader and are ignored. Any other dot-prefixed name is an unexplained final
// and blocks, so a hidden symlink or hidden unknown file is never skipped.
var publisherTemp = regexp.MustCompile(`^\.evidence-[0-9a-f]+$`)

// sequenceNames filters a directory listing. Only a recognized publisher temp
// file (".evidence-<hex>") that is an actual protected publisher artifact may be
// ignored; every entry is fstatat-checked first, so a symlink or special inode
// with a legitimate-looking temp name is rejected rather than skipped.
func sequenceNames(dir *os.File, entries []os.DirEntry, trust func(string, os.FileInfo) error) ([]string, error) {
	var names []string
	for _, e := range entries {
		name := e.Name()
		var st unix.Stat_t
		if err := unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return nil, fmt.Errorf("%w: entry %s: %v", ErrEvidenceTrust, name, err)
		}
		if st.Mode&unix.S_IFMT == unix.S_IFLNK {
			return nil, fmt.Errorf("%w: symlink %s", ErrEvidenceTrust, name)
		}
		if st.Mode&unix.S_IFMT != unix.S_IFREG {
			return nil, fmt.Errorf("%w: non-regular entry %s", ErrEvidenceTrust, name)
		}
		if publisherTemp.MatchString(name) {
			// Only ignore the real same-directory temp generated by
			// writeNoReplace: regular (checked above), exactly 0600, nlink=1
			// and trusted. Anything else that happens to match the name is not
			// a publisher artifact and blocks.
			if st.Mode&0o777 != evidenceFileMode || st.Nlink != 1 {
				return nil, fmt.Errorf("%w: unsafe publisher temp %s", ErrEvidenceTrust, name)
			}
			if trust != nil {
				info, err := statEntry(dir, name)
				if err != nil {
					return nil, err
				}
				if err := trust(name, info); err != nil {
					return nil, err
				}
			}
			continue
		}
		if !sequenceName.MatchString(name) {
			return nil, fmt.Errorf("%w: unexplained final %s", ErrEvidenceChain, name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// statEntry fstatat-opens one directory entry relative to the pinned directory
// FD without following a symlink, so trust can be evaluated on the opened inode
// rather than a pathname.
func statEntry(dir *os.File, name string) (os.FileInfo, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: entry %s: %v", ErrEvidenceTrust, name, err)
	}
	f := os.NewFile(uintptr(fd), name)
	info, err := f.Stat()
	f.Close()
	if err != nil {
		return nil, fmt.Errorf("%w: entry %s: %v", ErrEvidenceTrust, name, err)
	}
	return info, nil
}

func (r *Reader) listSequences(root *pinnedRoot, rel string) ([]string, error) {
	dir, err := r.openUnder(root, rel, true)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	return sequenceNames(dir, entries, r.trust)
}

func sequenceOf(name string) uint64 {
	n, _ := strconv.ParseUint(strings.TrimSuffix(name, ".json"), 10, 64)
	return n
}

// Object reads and identity-checks objects/<digest>.json (spec 6.2).
func (r *Reader) Object(digest string) (Record, error) {
	root, release, err := r.rootFor()
	if err != nil {
		return Record{}, err
	}
	defer release()
	return r.objectWithRoot(root, digest)
}

// objectWithRoot reads an object against the caller's already-pinned root so a
// caller (notably Recheck) never mixes a catalog read with a ref read taken
// from a different reopened root.
func (r *Reader) objectWithRoot(root *pinnedRoot, digest string) (Record, error) {
	if !validDigest(digest) {
		return Record{}, fmt.Errorf("%w: object digest", ErrEvidenceEncoding)
	}
	data, err := r.readFile(root, filepath.Join(objectsSubdir, digest+".json"))
	if err != nil {
		return Record{}, err
	}
	rec, err := decodeRecordBytes(data)
	if err != nil {
		return Record{}, err
	}
	_, got, err := CanonicalRecord(rec)
	if err != nil {
		return Record{}, err
	}
	if got != digest {
		return Record{}, fmt.Errorf("%w: object identity mismatch", ErrEvidenceChain)
	}
	return rec, nil
}

// catalogIdentity is the immutable (kind, identity) key of a catalog entry.
type catalogIdentity struct {
	kind     string
	identity string
}

// Catalog replays the complete contiguous catalog chain from the immutable
// genesis to the latest record, verifying sequence, predecessor digests, the
// fixed genesis authority and the immutable (kind, identity) -> record digest
// mapping. A malformed/gap/fork/replaced-mapping/unexplained latest blocks
// instead of falling back to an older record (spec 6.2).
func (r *Reader) Catalog() (CatalogRecord, error) {
	root, release, err := r.rootFor()
	if err != nil {
		return CatalogRecord{}, err
	}
	defer release()
	return r.catalogWithRoot(root)
}

func (r *Reader) catalogWithRoot(root *pinnedRoot) (CatalogRecord, error) {
	names, err := r.listSequences(root, catalogSubdir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return CatalogRecord{}, ErrNoCatalog
		}
		return CatalogRecord{}, err
	}
	if len(names) == 0 {
		return CatalogRecord{}, ErrNoCatalog
	}
	var prevDigest, genesisRoot, genesisBootstrap string
	seen := map[catalogIdentity]string{}
	var latest CatalogRecord
	for i, name := range names {
		want := uint64(i + 1)
		if sequenceOf(name) != want {
			return CatalogRecord{}, fmt.Errorf("%w: catalog sequence gap at %s", ErrEvidenceChain, name)
		}
		rec, err := r.readRecordFile(root, filepath.Join(catalogSubdir, name), MaxObjectBytes)
		if err != nil {
			return CatalogRecord{}, err
		}
		if rec.Kind != KindCatalog {
			return CatalogRecord{}, fmt.Errorf("%w: latest catalog kind", ErrEvidenceChain)
		}
		var payload CatalogPayload
		if err := strictJSON(rec.Payload, &payload); err != nil {
			return CatalogRecord{}, fmt.Errorf("%w: %v", ErrEvidenceEncoding, err)
		}
		payload, err = normalizeCatalogPayload(payload)
		if err != nil {
			return CatalogRecord{}, err
		}
		if payload.Sequence != want {
			return CatalogRecord{}, fmt.Errorf("%w: catalog filename/sequence mismatch", ErrEvidenceChain)
		}
		if i == 0 {
			if payload.PreviousDigest != "" {
				return CatalogRecord{}, fmt.Errorf("%w: catalog genesis predecessor", ErrEvidenceChain)
			}
			genesisRoot, genesisBootstrap = payload.RootQualificationDigest, payload.BootstrapApprovalDigest
		} else {
			if payload.PreviousDigest != prevDigest {
				return CatalogRecord{}, fmt.Errorf("%w: catalog predecessor", ErrEvidenceChain)
			}
			if payload.RootQualificationDigest != genesisRoot || payload.BootstrapApprovalDigest != genesisBootstrap {
				return CatalogRecord{}, fmt.Errorf("%w: catalog genesis changed", ErrEvidenceChain)
			}
		}
		// The (kind, identity) mapping is immutable across the whole chain: a
		// later catalog may not silently replace a record digest. A legitimate
		// revocation/qualification transition is a separate authority decision
		// and must not be smuggled in as a mapping rewrite.
		for _, entry := range payload.Entries {
			key := catalogIdentity{kind: entry.Kind, identity: entry.Identity}
			if prev, ok := seen[key]; ok && prev != entry.RecordDigest {
				return CatalogRecord{}, fmt.Errorf("%w: catalog identity mapping replaced", ErrEvidenceChain)
			}
			seen[key] = entry.RecordDigest
		}
		if _, prevDigest, err = CanonicalRecord(rec); err != nil {
			return CatalogRecord{}, err
		}
		latest = CatalogRecord{Header: rec.Header, Payload: payload}
	}
	// Structural replay alone is not authority: every listed issuer must be
	// anchored at the root and grant the record's kind, or the catalog cannot be
	// consumed (spec 6.2, 6.7).
	if err := r.authorizeCatalogEntries(root, latest); err != nil {
		return CatalogRecord{}, err
	}
	return latest, nil
}

func (r *Reader) readRecordFile(root *pinnedRoot, rel string, limit int) (Record, error) {
	data, err := r.readFile(root, rel)
	if err != nil {
		return Record{}, err
	}
	if len(data) > limit {
		return Record{}, fmt.Errorf("%w: record exceeds limit", ErrEvidenceEncoding)
	}
	return decodeRecordBytes(data)
}

// Ticket reads tickets/<ticketDigest>/ and verifies the contiguous chain
// (spec 6.2). It returns them in sequence order.
func (r *Reader) Ticket(ticketDigest string) ([]TicketRecord, error) {
	if !validDigest(ticketDigest) {
		return nil, fmt.Errorf("%w: ticket digest", ErrEvidenceEncoding)
	}
	root, release, err := r.rootFor()
	if err != nil {
		return nil, err
	}
	defer release()
	rel := filepath.Join(ticketsSubdir, ticketDigest)
	names, err := r.listSequences(root, rel)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("%w: no ticket records", ErrEvidenceChain)
	}
	var out []TicketRecord
	prev := ""
	for i, name := range names {
		rec, err := r.readRecordFile(root, filepath.Join(rel, name), MaxTicketBytes)
		if err != nil {
			return nil, err
		}
		if rec.Kind != KindTicket {
			return nil, fmt.Errorf("%w: ticket kind", ErrEvidenceChain)
		}
		var p TicketPayload
		if err := strictJSON(rec.Payload, &p); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrEvidenceEncoding, err)
		}
		p, err = normalizeTicketPayload(p)
		if err != nil {
			return nil, err
		}
		if p.TicketDigest != ticketDigest {
			return nil, fmt.Errorf("%w: ticket identity", ErrEvidenceChain)
		}
		if sequenceOf(name) != p.Sequence || p.Sequence != uint64(i+1) {
			return nil, fmt.Errorf("%w: ticket sequence gap", ErrEvidenceChain)
		}
		if p.PreviousDigest != prev {
			return nil, fmt.Errorf("%w: ticket predecessor", ErrEvidenceChain)
		}
		_, digest, err := CanonicalRecord(rec)
		if err != nil {
			return nil, err
		}
		prev = digest
		out = append(out, TicketRecord{Header: rec.Header, Payload: p})
	}
	return out, nil
}

// Recheck re-reads the current catalog and confirms the previous catalog is
// still an ancestor, that the supplied refs exist, and that at is usable.
func (r *Reader) Recheck(previous CatalogRecord, refs []string, at time.Time) (CatalogRecord, error) {
	root, release, err := r.rootFor()
	if err != nil {
		return CatalogRecord{}, err
	}
	defer release()
	current, err := r.catalogWithRoot(root)
	if err != nil {
		return CatalogRecord{}, err
	}
	if at.IsZero() {
		return CatalogRecord{}, fmt.Errorf("%w: recheck time", ErrEvidenceEncoding)
	}
	if current.Payload.Sequence < previous.Payload.Sequence {
		return CatalogRecord{}, fmt.Errorf("%w: catalog rolled back", ErrEvidenceChain)
	}
	older, err := r.catalogAt(root, previous.Payload.Sequence)
	if err != nil {
		return CatalogRecord{}, err
	}
	_, want, _ := CanonicalRecord(Record{Header: older.Header, Payload: mustMarshal(older.Payload)})
	_, got, _ := CanonicalRecord(Record{Header: previous.Header, Payload: mustMarshal(previous.Payload)})
	if want != got {
		return CatalogRecord{}, fmt.Errorf("%w: catalog fork at sequence", ErrEvidenceChain)
	}
	for _, ref := range refs {
		if !validDigest(ref) {
			return CatalogRecord{}, fmt.Errorf("%w: ref digest", ErrEvidenceEncoding)
		}
	}
	if err := r.validateRefClosureAt(root, current, refs, at); err != nil {
		return CatalogRecord{}, err
	}
	return current, nil
}

func (r *Reader) catalogAt(root *pinnedRoot, seq uint64) (CatalogRecord, error) {
	name := fmt.Sprintf("%020d.json", seq)
	data, err := r.readFile(root, filepath.Join(catalogSubdir, name))
	if err != nil {
		return CatalogRecord{}, err
	}
	if len(data) > MaxObjectBytes {
		return CatalogRecord{}, fmt.Errorf("%w: catalog record exceeds limit", ErrEvidenceEncoding)
	}
	rec, err := decodeRecordBytes(data)
	if err != nil {
		return CatalogRecord{}, err
	}
	if rec.Kind != KindCatalog {
		return CatalogRecord{}, fmt.Errorf("%w: catalog kind", ErrEvidenceChain)
	}
	var p CatalogPayload
	if err := strictJSON(rec.Payload, &p); err != nil {
		return CatalogRecord{}, fmt.Errorf("%w: %v", ErrEvidenceEncoding, err)
	}
	p, err = normalizeCatalogPayload(p)
	if err != nil {
		return CatalogRecord{}, err
	}
	return CatalogRecord{Header: rec.Header, Payload: p}, nil
}

func mustMarshal(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}
