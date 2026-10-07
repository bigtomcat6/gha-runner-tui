package evidence

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// --- Residual 1: trust/mode boundaries -------------------------------------

// The initial "/" descriptor of the ancestor walk must itself be trust-checked.
func TestOpenPinnedRootChecksInitialAncestor(t *testing.T) {
	root := testTree(t)
	trust := func(path string, info os.FileInfo) error {
		if path == "/" {
			return errors.New("untrusted initial ancestor")
		}
		return nil
	}
	if _, err := openPinnedRoot(root, trust, nil, fakeMountID); err == nil {
		t.Fatal("initial '/' ancestor was not trust-checked")
	}
}

// The scheduler root must be exactly 0700, not merely non-group/other writable.
func TestOpenPinnedRootRejectsUnsafeSchedulerRootMode(t *testing.T) {
	root := testTree(t)
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := openPinnedRoot(root, testTrust, nil, fakeMountID); err == nil {
		t.Fatal("0755 scheduler root accepted")
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	pinned, err := openPinnedRoot(root, testTrust, nil, fakeMountID)
	if err != nil {
		t.Fatalf("0700 scheduler root rejected: %v", err)
	}
	pinned.Close()
}

// An acquired lock chmod'd to 0644 must be rejected on the next effect
// revalidation, not accepted by a permissive trust seam.
func TestLeaseRejectsUnsafeLockMode(t *testing.T) {
	root := testTree(t)
	provisionCatalog(t, root)
	l := testLease(t, root)
	if err := l.verifyFDBindings(); err != nil {
		t.Fatalf("baseline lock revalidation: %v", err)
	}
	if err := os.Chmod(filepath.Join(root, "supervisor.lock"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := l.verifyFDBindings(); err == nil {
		t.Fatal("0644 lock inode accepted")
	}
}

// --- Residual 2: mount identity and retained pin ---------------------------

// A changed kernel mount identity must not be accepted even when dev/inode are
// unchanged (as a bind mount would present). This injects a concrete mount-id
// seam because the test host has no statx/bind-mount substrate.
func TestLeaseRejectsMountIdentityChange(t *testing.T) {
	root := testTree(t)
	provisionCatalog(t, root)
	var id uint64 = 1
	seam := func(*os.File) (uint64, error) { return id, nil }
	l, err := acquireHostLeaseAtMount(root, testTrust, seam)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	if err := l.verifyFDBindings(); err != nil {
		t.Fatalf("same mount identity rejected: %v", err)
	}
	id = 2
	if err := l.verifyFDBindings(); err == nil {
		t.Fatal("changed mount identity accepted with unchanged dev/inode")
	}
}

// A reader without a usable mount-id reader must fail closed.
func TestReaderFailsClosedWithoutMountIdentity(t *testing.T) {
	root := testTree(t)
	provisionCatalog(t, root)
	r := testReader(root)
	r.mountID = func(*os.File) (uint64, error) { return 0, errors.New("no mount identity") }
	if _, err := r.Catalog(); err == nil {
		t.Fatal("reader without mount identity accepted")
	}
}

// The reader must retain one pinned root for its lifetime instead of reopening
// the path on every call.
func TestReaderRetainsPinnedRoot(t *testing.T) {
	root := testTree(t)
	provisionCatalog(t, root)
	r := testReader(root)
	if _, err := r.Catalog(); err != nil {
		t.Fatal(err)
	}
	first := r.pinned
	if first == nil {
		t.Fatal("reader did not pin its root")
	}
	if _, err := r.Catalog(); err != nil {
		t.Fatal(err)
	}
	if r.pinned != first {
		t.Fatal("reader reopened its root between calls")
	}
}

// Recheck must not keep serving a tree whose fixed pathname has been renamed
// away: every current use revalidates the retained pin against the fixed path
// before reading through it, so a disappearing source is an error.
func TestReaderRecheckRejectsMovedRoot(t *testing.T) {
	root := testTree(t)
	_, producerDigest := provisionCatalog(t, root)
	r := testReader(root)
	cat, err := r.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	moved := root + "-moved"
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Rename(moved, root) })
	if _, err := r.Recheck(cat, []string{producerDigest}, evalTime()); err == nil {
		t.Fatal("Recheck accepted a renamed-away root path")
	}
}

// Recheck must reject a root replaced at the fixed path by a different inode
// even though the retained FD still resolves its (now-detached) original tree.
func TestReaderRecheckRejectsReplacedRoot(t *testing.T) {
	root := testTree(t)
	_, producerDigest := provisionCatalog(t, root)
	r := testReader(root)
	cat, err := r.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	moved := root + "-moved"
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root); _ = os.Rename(moved, root) })
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Recheck(cat, []string{producerDigest}, evalTime()); err == nil {
		t.Fatal("Recheck accepted a replaced root with a different inode")
	}
}

// The unchanged positive: while the fixed path still names the retained pin,
// Recheck reads its catalog and refs through that same retained pin.
func TestReaderRecheckUsesPinnedRoot(t *testing.T) {
	root := testTree(t)
	_, producerDigest := provisionCatalog(t, root)
	r := testReader(root)
	cat, err := r.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	first := r.pinned
	if _, err := r.Recheck(cat, []string{producerDigest}, evalTime()); err != nil {
		t.Fatalf("Recheck on an unchanged root failed: %v", err)
	}
	if r.pinned != first {
		t.Fatal("Recheck reopened the root instead of using its pin")
	}
}

// --- Residual 3: publisher temp name handling ------------------------------

func catalogDir(t *testing.T, root string) string {
	t.Helper()
	return filepath.Join(evidenceDir(root), catalogSubdir)
}

// A symlink whose name matches the publisher temp pattern is not a publisher
// artifact and must be rejected.
func TestCatalogRejectsTempNameSymlink(t *testing.T) {
	root := testTree(t)
	provisionCatalog(t, root)
	dir := catalogDir(t, root)
	target := filepath.Join(dir, "00000000000000000001.json")
	if err := os.Symlink(target, filepath.Join(dir, ".evidence-deadbeef")); err != nil {
		t.Fatal(err)
	}
	if _, err := testReader(root).Catalog(); err == nil {
		t.Fatal("symlinked temp name accepted")
	}
}

// A non-regular entry with a temp name is rejected.
func TestCatalogRejectsTempNameDirectory(t *testing.T) {
	root := testTree(t)
	provisionCatalog(t, root)
	if err := os.Mkdir(filepath.Join(catalogDir(t, root), ".evidence-deadbeef"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := testReader(root).Catalog(); err == nil {
		t.Fatal("directory with temp name accepted")
	}
}

// A temp-named regular file with the wrong protection is not a publisher temp.
func TestCatalogRejectsUnsafeTempMode(t *testing.T) {
	root := testTree(t)
	provisionCatalog(t, root)
	path := filepath.Join(catalogDir(t, root), ".evidence-deadbeef")
	if err := os.WriteFile(path, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := testReader(root).Catalog(); err == nil {
		t.Fatal("0644 temp-named file accepted")
	}
}

// A temp-named regular file with nlink > 1 is not a publisher temp.
func TestCatalogRejectsHardlinkedTemp(t *testing.T) {
	root := testTree(t)
	provisionCatalog(t, root)
	path := filepath.Join(catalogDir(t, root), ".evidence-deadbeef")
	if err := os.WriteFile(path, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, filepath.Join(root, "temp-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := testReader(root).Catalog(); err == nil {
		t.Fatal("hardlinked temp-named file accepted")
	}
}

// A genuine protected publisher temp (regular 0600 nlink=1) is still ignored.
func TestCatalogAcceptsProtectedTemp(t *testing.T) {
	root := testTree(t)
	provisionCatalog(t, root)
	path := filepath.Join(catalogDir(t, root), ".evidence-deadbeef")
	if err := os.WriteFile(path, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := testReader(root).Catalog(); err != nil {
		t.Fatalf("protected publisher temp rejected: %v", err)
	}
}
