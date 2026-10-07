package evidence

import (
	"errors"
	"sync"
	"testing"
)

// A standalone reader that never opened a root owns no FD, but Close must still
// move it to closed: a later Object/Catalog may not initialize a root after
// Close has returned (leaf fix 2a-4).
func TestReaderCloseBeforeFirstUseRejectsRead(t *testing.T) {
	root := testTree(t)
	_, digest := provisionCatalog(t, root)
	r := testReader(root)

	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if r.pinned != nil || r.ownsPinned {
		t.Fatalf("unused standalone reader retained state after Close: pinned=%v owns=%v", r.pinned, r.ownsPinned)
	}
	if _, err := r.Catalog(); err == nil {
		t.Fatal("Catalog after Close accepted")
	}
	if _, err := r.Object(digest); err == nil {
		t.Fatal("Object after Close accepted")
	}
	if r.pinned != nil {
		t.Fatal("reader initialized a root after Close")
	}
}

// Close must win a first-use race: once Close has returned, concurrently
// started reads must be rejected and must never install a root.
func TestReaderCloseWinsFirstUseRace(t *testing.T) {
	root := testTree(t)
	provisionCatalog(t, root)
	r := testReader(root)

	closeDone := make(chan error, 1)
	go func() { closeDone <- r.Close() }()
	if err := <-closeDone; err != nil {
		t.Fatalf("Close: %v", err)
	}

	const readers = 16
	start := make(chan struct{})
	failed := make(chan error, readers)
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := r.Catalog(); err == nil {
				failed <- errors.New("Catalog after Close accepted")
			}
		}()
	}
	close(start)
	wg.Wait()
	close(failed)
	for err := range failed {
		t.Fatal(err)
	}
	if r.pinned != nil {
		t.Fatal("reader installed a root after Close")
	}
}

// Unchanged positive: a standalone reader that opened its own root still
// releases that FD on Close and rejects later reads.
func TestReaderOwnedCloseStillReleasesRoot(t *testing.T) {
	root := testTree(t)
	provisionCatalog(t, root)
	r := testReader(root)
	if _, err := r.Catalog(); err != nil {
		t.Fatal(err)
	}
	owned := r.pinned
	if owned == nil || !r.ownsPinned {
		t.Fatal("reader did not own its root")
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := owned.fd.Stat(); err == nil {
		t.Fatal("owned root FD still open after Close")
	}
	if _, err := r.Catalog(); err == nil {
		t.Fatal("read after Close accepted")
	}
}

// A borrowed (lease-installed) root is never released by Reader.Close and the
// reader stays usable through the Lease, as before.
func TestLeaseBorrowedRootSurvivesReaderClose(t *testing.T) {
	root := testTree(t)
	provisionCatalog(t, root)
	l := testLease(t, root)
	if _, err := l.reader.Catalog(); err != nil {
		t.Fatal(err)
	}
	if err := l.reader.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.root.fd.Stat(); err != nil {
		t.Fatalf("reader Close closed the lease-owned root FD: %v", err)
	}
	if _, err := l.reader.Catalog(); err != nil {
		t.Fatalf("lease reader unusable after borrowed-root Close: %v", err)
	}
}
