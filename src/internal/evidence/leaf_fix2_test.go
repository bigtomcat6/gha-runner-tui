package evidence

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Finding 5: the (kind, identity) -> record digest map is immutable across the
// complete catalog chain. A later catalog may not silently replace a digest.
func TestCatalogRejectsIdentityDigestReplacement(t *testing.T) {
	root := testTree(t)
	provisionCatalog(t, root)
	p := testPublisher(t, root)
	r := testReader(root)
	cat, err := r.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	_, tail, err := CanonicalRecord(Record{Header: cat.Header, Payload: mustMarshal(cat.Payload)})
	if err != nil {
		t.Fatal(err)
	}
	next := cat.Payload
	next.Sequence++
	next.PreviousDigest = tail
	replaced := false
	for i := range next.Entries {
		if next.Entries[i].Kind == KindIssuerQualification && next.Entries[i].Identity == "producer" {
			next.Entries[i].RecordDigest = d64("replacement")
			replaced = true
		}
	}
	if !replaced {
		t.Fatal("fixture missing producer catalog entry")
	}
	base := cat.Header
	base.Kind = KindCatalog
	if _, err := p.putCatalog(CatalogRecord{Header: base, Payload: next}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Catalog(); err == nil {
		t.Fatal("catalog identity digest replacement accepted")
	}
}

// Finding 5: only recognized publisher temp files are ignored; dot-prefixed
// symlinks and dot-prefixed unexplained finals block.
func TestCatalogRejectsDotPrefixAndUnknownFinals(t *testing.T) {
	t.Run("dotprefix symlink", func(t *testing.T) {
		root := testTree(t)
		provisionCatalog(t, root)
		dir := filepath.Join(evidenceDir(root), catalogSubdir)
		target := filepath.Join(dir, "00000000000000000001.json")
		if err := os.Symlink(target, filepath.Join(dir, ".hidden")); err != nil {
			t.Fatal(err)
		}
		if _, err := testReader(root).Catalog(); err == nil {
			t.Fatal("dot-prefixed symlink accepted")
		}
	})
	t.Run("dotprefix unexplained final", func(t *testing.T) {
		root := testTree(t)
		provisionCatalog(t, root)
		dir := filepath.Join(evidenceDir(root), catalogSubdir)
		if err := os.WriteFile(filepath.Join(dir, ".hidden"), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := testReader(root).Catalog(); err == nil {
			t.Fatal("dot-prefixed unknown final accepted")
		}
	})
	t.Run("recognized temp ignored", func(t *testing.T) {
		root := testTree(t)
		provisionCatalog(t, root)
		dir := filepath.Join(evidenceDir(root), catalogSubdir)
		if err := os.WriteFile(filepath.Join(dir, ".evidence-deadbeef"), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := testReader(root).Catalog(); err != nil {
			t.Fatalf("recognized publisher temp rejected: %v", err)
		}
	})
}

// Finding 4: the publisher must not follow an ancestor symlink; a symlinked
// evidence subdirectory is rejected by the descriptor-relative O_NOFOLLOW walk
// and nothing is written through it.
func TestPublisherRejectsSymlinkedObjectsDir(t *testing.T) {
	root := testTree(t)
	p := testPublisher(t, root)
	outside := t.TempDir()
	obj := filepath.Join(evidenceDir(root), objectsSubdir)
	if err := os.Symlink(outside, obj); err != nil {
		t.Fatal(err)
	}
	if _, err := p.publishObject(validEpochRecord(t)); err == nil {
		t.Fatal("publication through symlinked ancestor accepted")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("publication wrote %d entries through symlink", len(entries))
	}
}

// Finding 4: evidence directories must be exactly 0700.
func TestPublisherRejectsUnsafeEvidenceDirMode(t *testing.T) {
	root := testTree(t)
	p := testPublisher(t, root)
	obj := filepath.Join(evidenceDir(root), objectsSubdir)
	if err := os.Mkdir(obj, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := p.publishObject(validEpochRecord(t)); err == nil {
		t.Fatal("publication into 0755 evidence dir accepted")
	}
}

// Finding 6: a mkdir that succeeded but whose parent sync failed must be
// repaired on retry even though the component now already exists.
func TestPublisherResyncsParentOnRetry(t *testing.T) {
	root := testTree(t)
	p := testPublisher(t, root)
	orig := p.fsyncDir
	var synced []string
	injected := false
	p.fsyncDir = func(f *os.File) error {
		name := filepath.Base(f.Name())
		synced = append(synced, name)
		if !injected && name == "v1" {
			injected = true
			return errors.New("injected parent sync failure")
		}
		return orig(f)
	}
	rec := validEpochRecord(t)
	if _, err := p.publishObject(rec); err == nil {
		t.Fatal("expected injected parent sync failure")
	}
	if _, err := p.publishObject(rec); err != nil {
		t.Fatalf("retry after parent sync failure: %v", err)
	}
	count := 0
	for _, name := range synced {
		if name == "v1" {
			count++
		}
	}
	if count < 2 {
		t.Fatalf("parent v1 not re-synced on retry: %v", synced)
	}
}

// Finding 11: a catalog rename whose directory sync failed must be re-synced by
// an idempotent retry, not returned as a paper-idempotent success.
func TestCatalogResyncOnIdempotentRetry(t *testing.T) {
	root := testTree(t)
	_, producerDigest := provisionCatalog(t, root)
	l := testLease(t, root)
	w, err := l.NewWriter("h1")
	if err != nil {
		t.Fatal(err)
	}
	orig := l.pub.fsyncDir
	catalogSyncs := 0
	failNext := true
	l.pub.fsyncDir = func(f *os.File) error {
		if filepath.Base(f.Name()) == "catalog" {
			catalogSyncs++
			if failNext {
				failNext = false
				return errors.New("injected catalog dir sync failure")
			}
		}
		return orig(f)
	}
	fact := validEpochRecord(t)
	fact.IssuerQualificationRef = producerDigest
	if _, err := w.Publish(fact); err == nil {
		t.Fatal("expected injected catalog sync failure")
	}
	if _, err := w.Publish(fact); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	if catalogSyncs < 2 {
		t.Fatalf("catalog dir not re-synced on retry (%d syncs)", catalogSyncs)
	}
	cat, err := testReader(root).Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if cat.Payload.Sequence != 2 {
		t.Fatalf("idempotent retry appended a duplicate: sequence=%d", cat.Payload.Sequence)
	}
}

// Finding 4: a renamed supervisor.lock followed by a fresh file must not
// masquerade as the held lock inode.
func TestLeaseRejectsReplacedLockEntry(t *testing.T) {
	root := testTree(t)
	provisionCatalog(t, root)
	l := testLease(t, root)
	lockPath := filepath.Join(root, "supervisor.lock")
	if err := os.Rename(lockPath, lockPath+".held"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, []byte{}, 0600); err != nil {
		t.Fatal(err)
	}
	if err := l.verifyFDBindings(); err == nil {
		t.Fatal("replaced lock directory entry accepted")
	}
}

// Finding 4: a hardlinked lock (nlink>1) is not a valid held identity.
func TestLeaseRejectsHardlinkedLock(t *testing.T) {
	root := testTree(t)
	provisionCatalog(t, root)
	l := testLease(t, root)
	lockPath := filepath.Join(root, "supervisor.lock")
	if err := os.Link(lockPath, filepath.Join(root, "supervisor.lock.link")); err != nil {
		t.Fatal(err)
	}
	if err := l.verifyFDBindings(); err == nil {
		t.Fatal("hardlinked lock accepted")
	}
}

// Finding 4: replacing the evidence root path with a symlink to the moved root
// is rejected because the retained root dev/inode no longer resolves.
func TestLeaseRejectsSwappedRoot(t *testing.T) {
	root := testTree(t)
	provisionCatalog(t, root)
	l := testLease(t, root)
	moved := root + "-moved"
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, root); err != nil {
		t.Fatal(err)
	}
	if err := l.verifyFDBindings(); err == nil {
		t.Fatal("swapped evidence root accepted")
	}
}
