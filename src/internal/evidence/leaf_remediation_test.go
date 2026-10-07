package evidence

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// prepareStartTicketForTest appends a prepared ticket for the provisioned writer
// and returns the header used by every later ticket in the chain.
func prepareStartTicketForTest(t *testing.T, root, producerDigest, ticketDigest string) (*Lease, *Writer, TicketRecord) {
	t.Helper()
	l := testLease(t, root)
	w, err := l.NewWriter("h1")
	if err != nil {
		t.Fatal(err)
	}
	header := testHeader(KindTicket)
	header.IssuerQualificationRef = producerDigest
	prepared := TicketRecord{Header: header, Payload: TicketPayload{
		TicketDigest: ticketDigest, IntentDigest: d64("intent-" + ticketDigest), OperationID: ticketDigest + "/start",
		Action: "start", Phase: "prepared", FactDigests: []string{}, WriterEpoch: "",
	}}
	if _, err := w.AppendTicket(prepared); err != nil {
		t.Fatal(err)
	}
	return l, w, prepared
}

// Defect 2: Execute must reject a ticket that was never prepared, and must never
// run the effect closure or touch the filesystem for it.
func TestExecuteRejectsUnpreparedTicket(t *testing.T) {
	root := testTree(t)
	_, producerDigest := provisionCatalog(t, root)
	l := testLease(t, root)
	w, err := l.NewWriter("h1")
	if err != nil {
		t.Fatal(err)
	}
	header := testHeader(KindTicket)
	header.IssuerQualificationRef = producerDigest
	ghostDigest := d64("ghost-ticket")
	dispatch := TicketRecord{Header: header, Payload: TicketPayload{
		TicketDigest: ghostDigest, IntentDigest: d64("intent"), OperationID: "ghost/start",
		Action: "start", Phase: "dispatch-intent", FactDigests: []string{}, WriterEpoch: "",
	}}
	ran := 0
	if _, err := w.Execute(context.Background(), dispatch, func() (Record, error) {
		ran++
		return Record{}, nil
	}); err == nil {
		t.Fatal("execute without a prepared ticket was accepted")
	}
	if ran != 0 {
		t.Fatalf("closure ran %d times for an unprepared ticket", ran)
	}
	if _, err := os.Stat(filepath.Join(evidenceDir(root), ticketsSubdir, ghostDigest)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unprepared execute touched the filesystem: %v", err)
	}
}

// Defect 8/9: the exact canonical byte cap must reject an effect before the
// closure runs and before any ticket bytes are written.
func TestExecuteRejectsOversizeBeforeEffect(t *testing.T) {
	root := testTree(t)
	_, producerDigest := provisionCatalog(t, root)
	ticketDigest := d64("big-ticket")
	_, w, prepared := prepareStartTicketForTest(t, root, producerDigest, ticketDigest)

	facts := make([]string, 0, 2000)
	for i := 0; i < 2000; i++ {
		facts = append(facts, d64(strconv.Itoa(i)))
	}
	dispatch := TicketRecord{Header: prepared.Header, Payload: TicketPayload{
		TicketDigest: ticketDigest, IntentDigest: prepared.Payload.IntentDigest, OperationID: prepared.Payload.OperationID,
		Action: "start", Phase: "dispatch-intent", FactDigests: facts, WriterEpoch: "",
	}}
	ran := 0
	if _, err := w.Execute(context.Background(), dispatch, func() (Record, error) {
		ran++
		return Record{}, nil
	}); err == nil {
		t.Fatal("oversize dispatch accepted before effect")
	}
	if ran != 0 {
		t.Fatalf("closure ran %d times for an oversize dispatch", ran)
	}
	tickets, err := testReader(root).Ticket(ticketDigest)
	if err != nil {
		t.Fatal(err)
	}
	if len(tickets) != 1 {
		t.Fatalf("oversize dispatch wrote %d ticket records, want only the prepared record", len(tickets))
	}
}

// Defect 5/4: the catalog must be a complete contiguous chain; gaps, a changed
// genesis, unexplained finals and symlinked finals all block rather than
// silently falling back.
func TestCatalogRejectsGapForkAndUnexplainedFinal(t *testing.T) {
	appendSeq2 := func(t *testing.T, root string) string {
		t.Helper()
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
		base := cat.Header
		base.Kind = KindCatalog
		if _, err := p.putCatalog(CatalogRecord{Header: base, Payload: next}); err != nil {
			t.Fatal(err)
		}
		return filepath.Join(evidenceDir(root), catalogSubdir)
	}

	t.Run("gap", func(t *testing.T) {
		root := testTree(t)
		provisionCatalog(t, root)
		dir := appendSeq2(t, root)
		if err := os.Remove(filepath.Join(dir, "00000000000000000001.json")); err != nil {
			t.Fatal(err)
		}
		if _, err := testReader(root).Catalog(); err == nil {
			t.Fatal("catalog with a missing predecessor accepted")
		}
	})

	t.Run("changed genesis", func(t *testing.T) {
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
		next.RootQualificationDigest = d64("other-root")
		base := cat.Header
		base.Kind = KindCatalog
		if _, err := p.putCatalog(CatalogRecord{Header: base, Payload: next}); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Catalog(); err == nil {
			t.Fatal("catalog with a changed genesis accepted")
		}
	})

	t.Run("unexplained final", func(t *testing.T) {
		root := testTree(t)
		provisionCatalog(t, root)
		dir := filepath.Join(evidenceDir(root), catalogSubdir)
		if err := os.WriteFile(filepath.Join(dir, "evil.json"), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := testReader(root).Catalog(); err == nil {
			t.Fatal("catalog with an unexplained final accepted")
		}
	})

	t.Run("symlinked final", func(t *testing.T) {
		root := testTree(t)
		provisionCatalog(t, root)
		dir := filepath.Join(evidenceDir(root), catalogSubdir)
		target := filepath.Join(dir, "00000000000000000001.json")
		if err := os.Symlink(target, filepath.Join(dir, "00000000000000000002.json")); err != nil {
			t.Fatal(err)
		}
		if _, err := testReader(root).Catalog(); err == nil {
			t.Fatal("catalog with a symlinked final accepted")
		}
	})
}

// Defect 4: a symlinked object final is rejected at the opened-inode boundary.
func TestReaderRejectsSymlinkedObjectFinal(t *testing.T) {
	root := testTree(t)
	p := testPublisher(t, root)
	r := testReader(root)
	rec := validEpochRecord(t)
	digest, err := p.publishObject(rec)
	if err != nil {
		t.Fatal(err)
	}
	data, _, err := CanonicalRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target.json")
	if err := os.WriteFile(target, data, 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(evidenceDir(root), objectsSubdir, digest+".json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Object(digest); err == nil {
		t.Fatal("symlinked object final accepted")
	}
}

// Defect 10: Close must set closing before waiting, wait for the in-flight
// effect, and revoke the writer before unlocking.
func TestCloseWaitsForInFlightExecuteAndRevokes(t *testing.T) {
	root := testTree(t)
	_, producerDigest := provisionCatalog(t, root)
	ticketDigest := d64("inflight")
	l, w, prepared := prepareStartTicketForTest(t, root, producerDigest, ticketDigest)
	dispatch := TicketRecord{Header: prepared.Header, Payload: TicketPayload{
		TicketDigest: ticketDigest, IntentDigest: prepared.Payload.IntentDigest, OperationID: prepared.Payload.OperationID,
		Action: "start", Phase: "dispatch-intent", FactDigests: []string{}, WriterEpoch: "",
	}}

	entered := make(chan struct{})
	release := make(chan struct{})
	execDone := make(chan error, 1)
	go func() {
		_, err := w.Execute(context.Background(), dispatch, func() (Record, error) {
			close(entered)
			<-release
			return Record{Header: dispatch.Header, Payload: json.RawMessage(`{"outcome":"success-known"}`)}, nil
		})
		execDone <- err
	}()
	<-entered

	closeDone := make(chan struct{})
	go func() {
		_ = l.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
		close(release)
		t.Fatal("Close returned while an Execute critical section was in flight")
	case <-time.After(75 * time.Millisecond):
	}
	close(release)
	if err := <-execDone; err != nil {
		t.Fatalf("in-flight execute: %v", err)
	}
	<-closeDone
	if _, err := w.Identity(); err == nil {
		t.Fatal("identity returned after Close revoked the writer")
	}
}
