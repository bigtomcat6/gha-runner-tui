package evidence

import (
	"os"
	"sync"
	"testing"
)

// Concurrent first reads on a reader with no pin must still retain exactly one
// validated identity: the init critical section is serialized and every caller
// reads through the same retained pin.
func TestReaderConcurrentFirstReadsSinglePin(t *testing.T) {
	root := testTree(t)
	provisionCatalog(t, root)
	r := testReader(root)

	const readers = 32
	start := make(chan struct{})
	errs := make(chan error, readers)
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := r.Catalog(); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent first read failed: %v", err)
	}
	pin := r.pinned
	if pin == nil {
		t.Fatal("no pinned root retained")
	}
	// A subsequent read uses the same retained identity, never a reopened one.
	if _, err := r.Catalog(); err != nil {
		t.Fatal(err)
	}
	if r.pinned != pin {
		t.Fatal("concurrent first reads retained more than one identity")
	}
}

// Close must not release the retained root while a read is in flight (the read
// holds the lifetime lock), and once Close has returned every later read is
// rejected. The mount-id seam gates the in-flight read so the test is
// deterministic without sleeping.
func TestReaderCloseWaitsForActiveRead(t *testing.T) {
	root := testTree(t)
	provisionCatalog(t, root)
	r := testReader(root)
	if _, err := r.Catalog(); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	r.mountID = func(*os.File) (uint64, error) {
		once.Do(func() { close(entered) })
		<-release
		return 1, nil
	}

	readDone := make(chan error, 1)
	go func() {
		_, err := r.Catalog()
		readDone <- err
	}()
	<-entered // the read is inside revalidation, holding the retained root alive

	closeDone := make(chan error, 1)
	go func() { closeDone <- r.Close() }()
	select {
	case <-closeDone:
		t.Fatal("Close released the root while a read was active")
	default:
	}
	close(release)
	if err := <-readDone; err != nil {
		t.Fatalf("in-flight read failed across Close: %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := r.Catalog(); err == nil {
		t.Fatal("read after Close accepted")
	}
}

// A Reader that shares a Lease-owned root must not release that FD when the
// Reader itself is closed: ownership stays with the Lease.
func TestLeaseReaderCloseLeavesLeaseRoot(t *testing.T) {
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
}
