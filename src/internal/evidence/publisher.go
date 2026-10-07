package evidence

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// supervisorBinding is the runtime identity a runtime-producer qualification
// must match. Production reads it from qualified kernel/service metadata; tests
// inject it through the package-private lease seam.
type supervisorBinding struct {
	ExecutableDigest        string
	ServiceDefinitionDigest string
	ProvisioningRevision    uint64
}

// immutablePublisher implements the file half of the evidence writer: same-dir
// temp 0600, file sync, atomic no-replace rename, synced ancestry, final nlink=1
// (spec 6.2). Every operation is descriptor-relative to a pinned root FD; no
// pathname syscall can follow an ancestor symlink. It is a bounded file
// mechanism, not a registry.
type immutablePublisher struct {
	rootPath string
	dir      string // evidence v1 directory
	root     *pinnedRoot
	fsync    func(*os.File) error
	fsyncDir func(*os.File) error
	trust    func(string, os.FileInfo) error
	mountID  mountIDFn
}

func newPublisher(root string) *immutablePublisher {
	return &immutablePublisher{
		rootPath: root,
		dir:      evidenceDir(root),
		fsync:    func(f *os.File) error { return f.Sync() },
		fsyncDir: func(f *os.File) error { return f.Sync() },
		trust:    productionTrust,
		mountID:  platformMountID,
	}
}

// pinned returns the pinned root for this publisher. A lease installs its shared
// root so the publisher and reader observe the same retained inode identity.
func (p *immutablePublisher) pinned() (*pinnedRoot, error) {
	if p.root != nil {
		return p.root, nil
	}
	r, err := openPinnedRoot(p.rootPath, p.trust, nil, p.mountID)
	if err != nil {
		return nil, err
	}
	p.root = r
	return r, nil
}

// rel converts an absolute evidence path under the publisher root to a clean
// slash-relative path, rejecting any escape.
func (p *immutablePublisher) rel(abs string) (string, error) {
	rel, err := filepath.Rel(p.rootPath, abs)
	if err != nil || rel == "" || rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("%w: path escape %s", ErrEvidenceTrust, abs)
	}
	return filepath.ToSlash(rel), nil
}

// ensureDir creates each missing component under the evidence root one at a
// time. It fsyncs each parent including components that already exist, so a
// retry repairs a prior mkdir-then-failed-parent-sync before publication.
func (p *immutablePublisher) ensureDir(rel string) (string, error) {
	root, err := p.pinned()
	if err != nil {
		return "", err
	}
	abs := filepath.Join(p.dir, filepath.FromSlash(rel))
	full, err := p.rel(abs)
	if err != nil {
		return "", err
	}
	parts, err := splitRel(full)
	if err != nil {
		return "", err
	}
	dir, err := root.walkDir(parts, true, p.fsyncDir)
	if err != nil {
		return "", err
	}
	dir.Close()
	return abs, nil
}

// syncDirRel re-establishes durability for an existing evidence directory and
// every required ancestor from the evidence root down, without creating
// anything. It repairs a publication whose directory entry rename succeeded but
// whose directory sync failed before acknowledgement.
func (p *immutablePublisher) syncDirRel(rel string) error {
	root, err := p.pinned()
	if err != nil {
		return err
	}
	abs := filepath.Join(p.dir, filepath.FromSlash(rel))
	full, err := p.rel(abs)
	if err != nil {
		return err
	}
	parts, err := splitRel(full)
	if err != nil {
		return err
	}
	dir, err := root.walkDir(parts, false, p.fsyncDir)
	if err != nil {
		return err
	}
	// walkDir synced every ancestor; also fsync the final directory so a file
	// entry renamed into it is durable.
	if err := p.fsyncDir(dir); err != nil {
		dir.Close()
		return err
	}
	return dir.Close()
}

// readFinal opens an existing final descriptor-relative with O_NOFOLLOW and
// revalidates the opened inode (regular, 0600, nlink=1, trusted) before
// returning its bytes.
func (p *immutablePublisher) readFinal(path string) ([]byte, error) {
	root, err := p.pinned()
	if err != nil {
		return nil, err
	}
	drel, err := p.rel(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	dir, err := root.openDir(drel)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	fd, err := unix.Openat(int(dir.Fd()), filepath.Base(path), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != evidenceFileMode {
		return nil, fmt.Errorf("%w: unsafe final %s", ErrEvidenceTrust, path)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Nlink != 1 {
		return nil, fmt.Errorf("%w: final nlink %s", ErrEvidenceTrust, path)
	}
	if p.trust != nil {
		if err := p.trust(path, info); err != nil {
			return nil, err
		}
	}
	return ioReadAll(f)
}

// writeNoReplace writes data to dir/name descriptor-relative without ever
// replacing an existing file. The final inode has nlink=1 and the directory is
// synced.
func (p *immutablePublisher) writeNoReplace(dir, name string, data []byte) error {
	root, err := p.pinned()
	if err != nil {
		return err
	}
	if name == "" || name != filepath.Base(name) || strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		return fmt.Errorf("%w: bad file name %q", ErrEvidenceTrust, name)
	}
	drel, err := p.rel(dir)
	if err != nil {
		return err
	}
	dfd, err := root.openDir(drel)
	if err != nil {
		return err
	}
	defer dfd.Close()
	final := filepath.Join(dir, name)
	var st unix.Stat_t
	if err := unix.Fstatat(int(dfd.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		return fmt.Errorf("%w: %s exists", ErrEvidenceChain, final)
	} else if !errors.Is(err, unix.ENOENT) {
		return err
	}
	suffix, err := randomHex(8)
	if err != nil {
		return err
	}
	tmp := ".evidence-" + suffix
	fd, err := unix.Openat(int(dfd.Fd()), tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, evidenceFileMode)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), tmp)
	committed := false
	defer func() {
		if f != nil {
			f.Close()
		}
		if !committed {
			unix.Unlinkat(int(dfd.Fd()), tmp, 0)
		}
	}()
	if err := unix.Fchmod(fd, evidenceFileMode); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := p.fsync(f); err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != evidenceFileMode {
		return fmt.Errorf("%w: unsafe temp %s", ErrEvidenceTrust, tmp)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Nlink != 1 {
		return fmt.Errorf("%w: temp nlink %s", ErrEvidenceTrust, tmp)
	}
	if p.trust != nil {
		if err := p.trust(tmp, info); err != nil {
			return err
		}
	}
	if err := f.Close(); err != nil {
		f = nil
		return err
	}
	f = nil
	if err := renameNoReplaceAt(dfd, tmp, name); err != nil {
		return err
	}
	committed = true
	if err := p.fsyncDir(dfd); err != nil {
		return err
	}
	return nil
}

// publishObject canonicalizes and immutably publishes a record under
// objects/<digest>.json, reopening an existing final only to verify equality
// and re-establish durability.
func (p *immutablePublisher) publishObject(rec Record) (string, error) {
	data, digest, err := CanonicalRecord(rec)
	if err != nil {
		return "", err
	}
	dir, err := p.ensureDir(objectsSubdir)
	if err != nil {
		return "", err
	}
	final := filepath.Join(dir, digest+".json")
	if existing, err := p.readFinal(final); err == nil {
		if !bytes.Equal(existing, data) {
			return "", fmt.Errorf("%w: object digest collision", ErrEvidenceChain)
		}
		if err := p.syncDirRel(objectsSubdir); err != nil {
			return "", err
		}
		return digest, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := p.writeNoReplace(dir, digest+".json", data); err != nil {
		return "", err
	}
	return digest, nil
}

// putCatalog writes a catalog record at its sequence with full chain checks
// (seq1 genesis, later prev = current tail digest).
func (p *immutablePublisher) putCatalog(rec CatalogRecord) (string, error) {
	if rec.Kind != KindCatalog {
		return "", fmt.Errorf("%w: not a catalog", ErrEvidenceEncoding)
	}
	payload, err := normalizeCatalogPayload(rec.Payload)
	if err != nil {
		return "", err
	}
	rec.Payload = payload
	data, digest, err := CanonicalRecord(Record{Header: rec.Header, Payload: mustMarshal(payload)})
	if err != nil {
		return "", err
	}
	dir, err := p.ensureDir(catalogSubdir)
	if err != nil {
		return "", err
	}
	names, err := p.listSequenceNames(dir)
	if err != nil {
		return "", err
	}
	if payload.Sequence == 1 {
		if len(names) != 0 || payload.PreviousDigest != "" {
			return "", fmt.Errorf("%w: catalog genesis not first", ErrEvidenceChain)
		}
	} else {
		if len(names) == 0 {
			return "", fmt.Errorf("%w: missing catalog predecessor", ErrEvidenceChain)
		}
		last := names[len(names)-1]
		if sequenceOf(last) != payload.Sequence-1 {
			return "", fmt.Errorf("%w: catalog sequence gap", ErrEvidenceChain)
		}
		prevData, err := p.readFinal(filepath.Join(dir, last))
		if err != nil {
			return "", err
		}
		prevRec, err := decodeRecordBytes(prevData)
		if err != nil {
			return "", err
		}
		_, prevDigest, err := CanonicalRecord(prevRec)
		if err != nil {
			return "", err
		}
		if payload.PreviousDigest != prevDigest {
			return "", fmt.Errorf("%w: catalog predecessor digest", ErrEvidenceChain)
		}
	}
	name := fmt.Sprintf("%020d.json", payload.Sequence)
	if err := p.writeNoReplace(dir, name, data); err != nil {
		return "", err
	}
	return digest, nil
}

func (p *immutablePublisher) listSequenceNames(dir string) ([]string, error) {
	root, err := p.pinned()
	if err != nil {
		return nil, err
	}
	drel, err := p.rel(dir)
	if err != nil {
		return nil, err
	}
	dfd, err := root.openDir(drel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer dfd.Close()
	entries, err := dfd.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	return sequenceNames(dfd, entries, p.trust)
}

func (p *immutablePublisher) ticketPath(ticketDigest string) (string, error) {
	if !validDigest(ticketDigest) {
		return "", fmt.Errorf("%w: ticket digest", ErrEvidenceEncoding)
	}
	return p.ensureDir(filepath.Join(ticketsSubdir, ticketDigest))
}

// appendTicket validates the complete payload before any filesystem
// interaction, then writes the next contiguous ticket record.
func (p *immutablePublisher) appendTicket(base Header, payload TicketPayload) (string, error) {
	payload, err := normalizeTicketPayload(payload)
	if err != nil {
		return "", err
	}
	dir, err := p.ticketPath(payload.TicketDigest)
	if err != nil {
		return "", err
	}
	names, err := p.listSequenceNames(dir)
	if err != nil {
		return "", err
	}
	if len(names) == 0 {
		if payload.Sequence != 1 || payload.PreviousDigest != "" {
			return "", fmt.Errorf("%w: ticket genesis", ErrEvidenceChain)
		}
	} else {
		last := names[len(names)-1]
		if payload.Sequence != sequenceOf(last)+1 {
			return "", fmt.Errorf("%w: ticket sequence gap", ErrEvidenceChain)
		}
		lastData, err := p.readFinal(filepath.Join(dir, last))
		if err != nil {
			return "", err
		}
		lastRec, err := decodeRecordBytes(lastData)
		if err != nil {
			return "", err
		}
		_, lastDigest, err := CanonicalRecord(lastRec)
		if err != nil {
			return "", err
		}
		if payload.PreviousDigest != lastDigest {
			return "", fmt.Errorf("%w: ticket predecessor", ErrEvidenceChain)
		}
	}
	base.Kind = KindTicket
	data, _, err := CanonicalRecord(Record{Header: base, Payload: mustMarshal(payload)})
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("%020d.json", payload.Sequence)
	if err := p.writeNoReplace(dir, name, data); err != nil {
		return "", err
	}
	return payload.TicketDigest, nil
}

// Lease holds the single fixed flock inherited from the T4 lock opener. Only
// one host-bound writer is ever bound and revoke happens before unlock.
type Lease struct {
	schedulerRoot string
	lock          *os.File
	epoch         string
	mu            sync.Mutex
	closing       atomic.Bool
	closed        bool
	writer        *Writer

	pub     *immutablePublisher
	reader  *Reader
	now     func() time.Time
	trust   func(string, os.FileInfo) error
	mountID mountIDFn
	lockKey string
	lockDev uint64
	lockIno uint64
	// root is the retained, validated scheduler root identity shared with the
	// reader and publisher; every effect re-verifies it.
	root      *pinnedRoot
	namespace func() error
	// supervisor is the runtime-binding seam; production is set by
	// AcquireHostLease and tests supply the fixture binding.
	supervisor func() (supervisorBinding, error)
}

// acquireHostLeaseAt is the package-private file mechanism seam used by tests;
// it still takes the real flock. Production callers use AcquireHostLease.
func acquireHostLeaseAt(schedulerRoot string) (*Lease, error) {
	return acquireHostLeaseAtTrust(schedulerRoot, nil)
}

func acquireHostLeaseAtTrust(schedulerRoot string, trust func(string, os.FileInfo) error) (*Lease, error) {
	return acquireHostLeaseAtMount(schedulerRoot, trust, platformMountID)
}

// acquireHostLeaseAtMount is the package-private mount-id seam; production always
// passes platformMountID and only in-package tests inject a concrete identity.
func acquireHostLeaseAtMount(schedulerRoot string, trust func(string, os.FileInfo) error, mountID mountIDFn) (*Lease, error) {
	root, err := openPinnedRoot(schedulerRoot, trust, nil, mountID)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Lease, error) {
		root.Close()
		return nil, err
	}
	fd, err := unix.Openat(int(root.fd.Fd()), "supervisor.lock", unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, evidenceFileMode)
	if err != nil {
		return fail(err)
	}
	lockKey := filepath.Join(schedulerRoot, "supervisor.lock")
	f := os.NewFile(uintptr(fd), lockKey)
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return fail(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != evidenceFileMode || stat.Nlink != 1 {
		f.Close()
		return fail(fmt.Errorf("%w: unsafe evidence lock inode", ErrEvidenceTrust))
	}
	if trust != nil {
		if err := trust(lockKey, info); err != nil {
			f.Close()
			return fail(err)
		}
	}
	if err := flockFile(f); err != nil {
		f.Close()
		return fail(err)
	}
	epoch, err := randomHex(32)
	if err != nil {
		f.Close()
		return fail(err)
	}
	leaseTrust := trust
	if leaseTrust == nil {
		leaseTrust = productionTrust
	}
	l := &Lease{
		schedulerRoot: schedulerRoot,
		lock:          f,
		epoch:         epoch,
		pub:           newPublisher(schedulerRoot),
		now:           time.Now,
		trust:         leaseTrust,
		mountID:       mountID,
		lockKey:       lockKey,
		lockDev:       uint64(stat.Dev),
		lockIno:       uint64(stat.Ino),
		root:          root,
		supervisor:    productionSupervisorBinding,
	}
	l.pub.root = root
	l.pub.trust = leaseTrust
	l.pub.mountID = mountID
	l.reader = newReader(schedulerRoot)
	l.reader.trust = leaseTrust
	l.reader.mountID = mountID
	l.reader.pinned = root
	return l, nil
}

func flockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

type WriterIdentity struct {
	HostID                 string
	Epoch                  string
	IssuerQualificationRef string
}

// Writer is bound to one lease, one host and one externally provisioned
// runtime-producer qualification; it cannot create authority (spec 6.7).
type Writer struct {
	lease     *Lease
	hostID    string
	epoch     string
	issuerRef string
	allowed   map[string]bool
	revoked   bool
	pub       *immutablePublisher
}

// qualifyRuntimeProducer validates the full anchored authority chain: the
// catalog genesis root qualification (method-anchored, host-granting and
// currently valid), and an externally provisioned host-bound runtime-producer
// qualification that grants the runtime kinds, is currently valid, matches the
// actual supervisor runtime binding and has a resolvable acyclic support
// closure.
func qualifyRuntimeProducer(r *Reader, hostID string, at time.Time, actual supervisorBinding) (string, []string, error) {
	if !validDigest(actual.ExecutableDigest) || !validDigest(actual.ServiceDefinitionDigest) || actual.ProvisioningRevision == 0 {
		return "", nil, fmt.Errorf("%w: runtime binding unavailable", ErrEvidenceWriter)
	}
	cat, err := r.Catalog()
	if err != nil {
		return "", nil, err
	}
	rootDigest := cat.Payload.RootQualificationDigest
	rootRec, err := r.Object(rootDigest)
	if err != nil {
		return "", nil, err
	}
	if _, err := validateRootQualificationAt(cat, rootDigest, rootRec, at); err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrEvidenceWriter, err)
	}
	for _, entry := range cat.Payload.Entries {
		if entry.Kind != KindIssuerQualification || entry.RecordDigest == rootDigest {
			continue
		}
		rec, err := r.Object(entry.RecordDigest)
		if err != nil {
			return "", nil, err
		}
		if rec.HostID != hostID || rec.IssuerQualificationRef != rootDigest {
			continue
		}
		var q IssuerQualificationRecord
		if err := strictJSON(rec.Payload, &q); err != nil || q.Role != "runtime-producer" {
			continue
		}
		if !containsString(q.HostIDs, hostID) || strings.TrimSpace(q.MethodID) == "" {
			continue
		}
		if at.Before(rec.IssuedAt) || !at.Before(rec.ExpiresAt) {
			continue
		}
		if q.ExecutableDigest != actual.ExecutableDigest ||
			q.ServiceDefinitionDigest != actual.ServiceDefinitionDigest ||
			q.ProvisioningRevision != actual.ProvisioningRevision {
			continue
		}
		if err := r.checkAuthorityClosure(cat, entry.RecordDigest); err != nil {
			continue
		}
		// A qualified runtime-producer may append its own operation tickets even
		// though KindTicket is not a runtime fact kind in AllowedKinds, so ticket
		// operations still pass the allowed-kind check instead of bypassing it
		// (spec 6.2, 6.7).
		kinds := append(append([]string(nil), q.AllowedKinds...), KindTicket)
		return entry.RecordDigest, sortedUnique(kinds), nil
	}
	return "", nil, fmt.Errorf("%w: no runtime-producer qualification for host", ErrEvidenceWriter)
}

// checkAuthorityClosure resolves the leaf-concrete support/reference closure of
// an authority record, requiring every referenced object to be a catalog
// mapping and the graph to be acyclic (spec 6.2).
func (r *Reader) checkAuthorityClosure(cat CatalogRecord, start string) error {
	listed := map[string]bool{}
	for _, e := range cat.Payload.Entries {
		listed[e.RecordDigest] = true
	}
	if !listed[start] {
		return fmt.Errorf("%w: authority %s not a catalog mapping", ErrEvidenceChain, start)
	}
	return checkReferenceCycle(start, func(d string) ([]string, error) {
		rec, err := r.Object(d)
		if err != nil {
			return nil, err
		}
		refs, err := authorityRefDigests(rec)
		if err != nil {
			return nil, err
		}
		for _, ref := range refs {
			if !listed[ref] {
				return nil, fmt.Errorf("%w: support ref %s not a catalog mapping", ErrEvidenceChain, ref)
			}
		}
		return refs, nil
	})
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// NewWriter binds the single writer to hostID only when a matching qualified
// runtime-producer authority exists in the current catalog and matches the
// actual supervisor runtime binding.
func (l *Lease) NewWriter(hostID string) (*Writer, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.closing.Load() {
		return nil, fmt.Errorf("%w: lease closed", ErrEvidenceWriter)
	}
	if l.writer != nil {
		return nil, fmt.Errorf("%w: writer already bound", ErrEvidenceWriter)
	}
	if hostID == "" || len(hostID) > 256 {
		return nil, fmt.Errorf("%w: host id", ErrEvidenceWriter)
	}
	actual, err := l.supervisorBindingLocked()
	if err != nil {
		return nil, err
	}
	issuer, kinds, err := qualifyRuntimeProducer(l.reader, hostID, l.now().UTC(), actual)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	for _, k := range kinds {
		allowed[k] = true
	}
	w := &Writer{lease: l, hostID: hostID, epoch: l.epoch, issuerRef: issuer, allowed: allowed, pub: l.pub}
	l.writer = w
	return w, nil
}

func (l *Lease) supervisorBindingLocked() (supervisorBinding, error) {
	if l.supervisor == nil {
		return supervisorBinding{}, fmt.Errorf("%w: runtime binding unavailable", ErrEvidenceWriter)
	}
	return l.supervisor()
}

// Close sets closing first (so new effects are refused while an in-flight
// effect finishes), waits for the effect mutex, revokes the writer, then
// releases the flock. It never unlocks before revoking (spec 6.7).
func (l *Lease) Close() error {
	if !l.closing.CompareAndSwap(false, true) {
		l.mu.Lock()
		l.mu.Unlock()
		return nil
	}
	l.mu.Lock()
	l.closed = true
	if l.writer != nil {
		l.writer.revoked = true
		l.writer = nil
	}
	l.mu.Unlock()
	err := syscall.Flock(int(l.lock.Fd()), syscall.LOCK_UN)
	closeErr := l.lock.Close()
	if l.root != nil {
		l.root.Close()
	}
	if err != nil {
		return err
	}
	return closeErr
}

// Identity returns the current writer identity only after re-deriving the
// current authority, revocation and FD binding, so a stale identity is never
// handed out after expiry/revocation. An invalid writer returns the zero
// identity and an error.
func (w *Writer) Identity() (WriterIdentity, error) {
	w.lease.mu.Lock()
	defer w.lease.mu.Unlock()
	if err := w.checkUsableLocked(); err != nil {
		return WriterIdentity{}, err
	}
	if err := w.revalidateLocked("", false); err != nil {
		return WriterIdentity{}, err
	}
	return WriterIdentity{HostID: w.hostID, Epoch: w.epoch, IssuerQualificationRef: w.issuerRef}, nil
}

var runtimeProducerKinds = map[string]bool{
	KindDaemonEpoch:      true,
	KindStartIntent:      true,
	KindOperationReceipt: true,
	KindRemovalReceipt:   true,
	KindObservation:      true,
	KindEventChunk:       true,
	KindEventCoverage:    true,
}

// Publish immutably stores an allowed runtime fact and appends its catalog
// reference under the same chain (spec 6.7). Re-publishing an identical fact is
// idempotent; a conflicting mapping is rejected.
func (w *Writer) Publish(rec Record) (string, error) {
	w.lease.mu.Lock()
	defer w.lease.mu.Unlock()
	if err := w.checkUsableLocked(); err != nil {
		return "", err
	}
	if !runtimeProducerKinds[rec.Kind] || !w.allowed[rec.Kind] {
		return "", fmt.Errorf("%w: kind %q not allowed", ErrEvidenceWriter, rec.Kind)
	}
	if rec.HostID != w.hostID || rec.IssuerQualificationRef != w.issuerRef {
		return "", fmt.Errorf("%w: writer binding mismatch", ErrEvidenceWriter)
	}
	if err := w.revalidateLocked(rec.Kind, true); err != nil {
		return "", err
	}
	digest, err := w.pub.publishObject(rec)
	if err != nil {
		return "", err
	}
	if err := w.appendCatalogLocked(CatalogEntry{Kind: rec.Kind, Identity: digest, RecordDigest: digest}); err != nil {
		return "", err
	}
	return digest, nil
}

func (w *Writer) PublishAll(records []Record) ([]string, error) {
	out := make([]string, 0, len(records))
	for _, rec := range records {
		digest, err := w.Publish(rec)
		if err != nil {
			return out, err
		}
		out = append(out, digest)
	}
	return out, nil
}

func (w *Writer) checkUsableLocked() error {
	if w.revoked || w.lease.closed || w.lease.closing.Load() {
		return fmt.Errorf("%w: revoked", ErrEvidenceWriter)
	}
	return nil
}

// revalidateLocked re-derives the current authority, revocation and FD binding
// before every prospective effect (spec 6.7). The current catalog's revocations
// are resolved fail-closed; only RecordDigests/ReferenceIDs are targets and
// SupportDigests are dependencies.
func (w *Writer) revalidateLocked(kind string, requireKind bool) error {
	now := w.lease.now().UTC()
	actual, err := w.lease.supervisorBindingLocked()
	if err != nil {
		return err
	}
	issuer, kinds, err := qualifyRuntimeProducer(w.lease.reader, w.hostID, now, actual)
	if err != nil {
		return err
	}
	if issuer != w.issuerRef {
		return fmt.Errorf("%w: issuer changed", ErrEvidenceWriter)
	}
	if requireKind && !containsString(kinds, kind) {
		return fmt.Errorf("%w: kind %q no longer allowed", ErrEvidenceWriter, kind)
	}
	cat, err := w.lease.reader.Catalog()
	if err != nil {
		return err
	}
	targets, err := w.lease.reader.effectiveRevocationsAt(nil, cat, now)
	if err != nil {
		return err
	}
	if targets[w.issuerRef] || targets[cat.Payload.RootQualificationDigest] {
		return fmt.Errorf("%w: issuer revoked", ErrEvidenceWriter)
	}
	return w.lease.verifyFDBindings()
}

// verifyFDBindings re-checks that the root, the held lock FD and the current
// supervisor.lock directory entry are all the same validated identities before
// any effect. The root is compared by dev/inode and by kernel mount identity so
// a bind mount that reuses the inode cannot masquerade as the retained root.
// Fstatat with AT_SYMLINK_NOFOLLOW ensures a renamed lock file followed by a
// fresh supervisor.lock cannot masquerade as the held inode, and the lock must
// still be exactly 0600 on both the current entry and the held descriptor.
func (l *Lease) verifyFDBindings() error {
	if l.namespace != nil {
		if err := l.namespace(); err != nil {
			return err
		}
	}
	// The path must still resolve to the retained root inode and mount: reject a
	// replaced root, a bind mount reusing the inode, or a swapped mount.
	fresh, err := openPinnedRoot(l.schedulerRoot, l.trust, l.namespace, l.mountID)
	if err != nil {
		return err
	}
	sameRoot := fresh.dev == l.root.dev && fresh.ino == l.root.ino && fresh.mountID == l.root.mountID
	fresh.Close()
	if !sameRoot {
		return fmt.Errorf("%w: evidence root replaced", ErrEvidenceTrust)
	}
	// The current directory entry must still be the held lock inode, with the
	// exact evidence-file protection (regular, 0600, nlink=1).
	var cur unix.Stat_t
	if err := unix.Fstatat(int(l.root.fd.Fd()), "supervisor.lock", &cur, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("%w: lock entry missing", ErrEvidenceTrust)
	}
	if cur.Mode&unix.S_IFMT != unix.S_IFREG || cur.Mode&0o777 != evidenceFileMode ||
		uint64(cur.Dev) != l.lockDev || uint64(cur.Ino) != l.lockIno || cur.Nlink != 1 {
		return fmt.Errorf("%w: lock entry replaced", ErrEvidenceTrust)
	}
	info, err := l.lock.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != evidenceFileMode ||
		uint64(stat.Dev) != l.lockDev || uint64(stat.Ino) != l.lockIno || stat.Nlink != 1 {
		return fmt.Errorf("%w: lock inode changed", ErrEvidenceTrust)
	}
	return l.trust(l.lockKey, info)
}

func (w *Writer) appendCatalogLocked(entries ...CatalogEntry) error {
	cat, err := w.lease.reader.Catalog()
	if err != nil {
		if !errors.Is(err, ErrNoCatalog) {
			return err
		}
		return fmt.Errorf("%w: append without catalog", ErrEvidenceChain)
	}
	existing := map[string]string{}
	for _, e := range cat.Payload.Entries {
		existing[e.Kind+"\x00"+e.Identity] = e.RecordDigest
	}
	var fresh []CatalogEntry
	for _, e := range entries {
		key := e.Kind + "\x00" + e.Identity
		if prev, ok := existing[key]; ok {
			if prev != e.RecordDigest {
				return fmt.Errorf("%w: conflicting catalog mapping", ErrEvidenceChain)
			}
			continue
		}
		existing[key] = e.RecordDigest
		fresh = append(fresh, e)
	}
	if len(fresh) == 0 {
		// Every mapping is already present, which can happen when a previous
		// attempt renamed the catalog file but failed to sync its directory
		// before acknowledgement. Re-establish durability of the catalog and its
		// required ancestry instead of returning a paper-idempotent success.
		return w.pub.syncDirRel(catalogSubdir)
	}
	if len(cat.Payload.Entries)+len(fresh) > 1_000_000 {
		return fmt.Errorf("%w: catalog capacity", ErrEvidenceChain)
	}
	_, tailDigest, err := CanonicalRecord(Record{Header: cat.Header, Payload: mustMarshal(cat.Payload)})
	if err != nil {
		return err
	}
	base := cat.Header
	base.Kind = KindCatalog
	base.IssuerQualificationRef = w.issuerRef
	payload := cat.Payload
	payload.Sequence++
	payload.PreviousDigest = tailDigest
	payload.Entries = append(append([]CatalogEntry(nil), payload.Entries...), fresh...)
	_, err = w.pub.putCatalog(CatalogRecord{Header: base, Payload: payload})
	return err
}

// AppendTicket appends the next ticket record for its ticket digest.
func (w *Writer) AppendTicket(rec TicketRecord) (string, error) {
	w.lease.mu.Lock()
	defer w.lease.mu.Unlock()
	if err := w.checkUsableLocked(); err != nil {
		return "", err
	}
	if rec.HostID != w.hostID || rec.IssuerQualificationRef != w.issuerRef {
		return "", fmt.Errorf("%w: writer binding mismatch", ErrEvidenceWriter)
	}
	payload := rec.Payload
	payload.WriterEpoch = w.epoch
	// Validate every caller-supplied field before touching the filesystem; the
	// writer then assigns and re-validates the chain position (spec 6.2, 6.7).
	payload, err := normalizeTicketDraft(payload)
	if err != nil {
		return "", err
	}
	if err := w.revalidateLocked(KindTicket, true); err != nil {
		return "", err
	}
	seq, prev, err := w.nextTicketPositionLocked(payload.TicketDigest)
	if err != nil {
		return "", err
	}
	payload.Sequence = seq
	payload.PreviousDigest = prev
	return w.pub.appendTicket(rec.Header, payload)
}

func (w *Writer) nextTicketPositionLocked(ticketDigest string) (uint64, string, error) {
	if !validDigest(ticketDigest) {
		return 0, "", fmt.Errorf("%w: ticket digest", ErrEvidenceEncoding)
	}
	dir, err := w.pub.ticketPath(ticketDigest)
	if err != nil {
		return 0, "", err
	}
	names, err := w.pub.listSequenceNames(dir)
	if err != nil {
		return 0, "", err
	}
	if len(names) == 0 {
		return 1, "", nil
	}
	last := names[len(names)-1]
	data, err := w.pub.readFinal(filepath.Join(dir, last))
	if err != nil {
		return 0, "", err
	}
	rec, err := decodeRecordBytes(data)
	if err != nil {
		return 0, "", err
	}
	_, digest, err := CanonicalRecord(rec)
	if err != nil {
		return 0, "", err
	}
	return sequenceOf(last) + 1, digest, nil
}

// Execute is the sole effects critical section. Under the lease mutex it
// requires a prepared ticket, durably appends exactly one dispatch-intent,
// calls run once, then durably publishes the receipt object and receipt ticket
// before returning. A command error keeps a durable uncertain receipt and never
// retries (spec 6.4, 6.7).
func (w *Writer) Execute(ctx context.Context, dispatch TicketRecord, run func() (Record, error)) (string, error) {
	w.lease.mu.Lock()
	defer w.lease.mu.Unlock()
	if err := w.checkUsableLocked(); err != nil {
		return "", err
	}
	if dispatch.HostID != w.hostID || dispatch.IssuerQualificationRef != w.issuerRef {
		return "", fmt.Errorf("%w: writer binding mismatch", ErrEvidenceWriter)
	}
	disp := dispatch.Payload
	disp.WriterEpoch = w.epoch
	if disp.Phase != "dispatch-intent" {
		return "", fmt.Errorf("%w: execute requires dispatch-intent", ErrEvidenceChain)
	}
	disp, err := normalizeTicketDraft(disp)
	if err != nil {
		return "", err
	}
	if err := w.revalidateLocked(KindTicket, true); err != nil {
		return "", err
	}
	// State machine: exactly prepared -> dispatch-intent -> receipt. Anything
	// already dispatched (dispatch-intent/receipt) or terminal is never
	// re-dispatched; only a fresh prepared ticket may enter the effect.
	existing, err := w.lease.reader.Ticket(disp.TicketDigest)
	if err == nil {
		last := existing[len(existing)-1].Payload
		switch last.Phase {
		case "prepared":
		case "dispatch-intent", "receipt":
			return "", fmt.Errorf("%w: operation already dispatched; no retry", ErrEvidenceChain)
		default:
			return "", fmt.Errorf("%w: terminal ticket cannot execute", ErrEvidenceChain)
		}
	} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, ErrEvidenceChain) {
		return "", err
	} else {
		return "", fmt.Errorf("%w: execute requires a prepared ticket", ErrEvidenceChain)
	}

	seq, prev, err := w.nextTicketPositionLocked(disp.TicketDigest)
	if err != nil {
		return "", err
	}
	disp.Sequence, disp.PreviousDigest = seq, prev
	if _, err := w.pub.appendTicket(dispatch.Header, disp); err != nil {
		return "", err
	}
	if run == nil {
		return "", fmt.Errorf("%w: nil run", ErrEvidenceWriter)
	}
	receipt, runErr := run()
	if receipt.Kind == "" {
		receipt = Record{Header: dispatch.Header, Payload: json.RawMessage(`{"outcome":"uncertain"}`)}
	}
	receipt.Kind = KindOperationReceipt
	if receipt.HostID != w.hostID || receipt.IssuerQualificationRef != w.issuerRef {
		if runErr == nil {
			return "", fmt.Errorf("%w: receipt binding mismatch", ErrEvidenceWriter)
		}
		receipt.HostID, receipt.IssuerQualificationRef = w.hostID, w.issuerRef
	}
	receiptDigest, pubErr := w.pub.publishObject(receipt)
	if pubErr != nil {
		if runErr != nil {
			return "", runErr
		}
		return "", pubErr
	}
	receiptTicket := TicketPayload{
		TicketDigest: disp.TicketDigest,
		IntentDigest: disp.IntentDigest,
		OperationID:  disp.OperationID,
		Action:       disp.Action,
		Phase:        "receipt",
		FactDigests:  []string{receiptDigest},
		WriterEpoch:  w.epoch,
	}
	rseq, rprev, err := w.nextTicketPositionLocked(receiptTicket.TicketDigest)
	if err != nil {
		return receiptDigest, err
	}
	receiptTicket.Sequence, receiptTicket.PreviousDigest = rseq, rprev
	if _, err := w.pub.appendTicket(dispatch.Header, receiptTicket); err != nil {
		return receiptDigest, err
	}
	if runErr != nil {
		return receiptDigest, runErr
	}
	return receiptDigest, nil
}

func ioReadAll(f *os.File) ([]byte, error) {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(f)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
