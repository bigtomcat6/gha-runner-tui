package evidence

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func nlinkOf(t *testing.T, info os.FileInfo) uint64 {
	t.Helper()
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("no stat_t")
	}
	return uint64(stat.Nlink)
}

func testTrust(_ string, _ os.FileInfo) error { return nil }

// fakeMountID is the in-package test seam for the mount identity: platforms
// without statx (Darwin) cannot read a real value, so tests inject a stable
// concrete identity. Production Linux uses platformMountID and is never
// overridden by this.
func fakeMountID(*os.File) (uint64, error) { return 1, nil }

func testTree(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The scheduler root must be exactly 0700 (spec 6.1); t.TempDir may be
	// created 0755, so the fixture asserts the production mode.
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "evidence", "v1"), 0700); err != nil {
		t.Fatal(err)
	}
	return root
}

func testPublisher(t *testing.T, root string) *immutablePublisher {
	t.Helper()
	p := newPublisher(root)
	p.trust = testTrust
	p.mountID = fakeMountID
	return p
}

func testReader(root string) *Reader {
	r := newReader(root)
	r.trust = testTrust
	r.mountID = fakeMountID
	return r
}

// testBinding is the runtime supervisor binding every valid fixture matches.
func testBinding() supervisorBinding {
	return supervisorBinding{ExecutableDigest: d64("sv-exe"), ServiceDefinitionDigest: d64("sv-svc"), ProvisioningRevision: 7}
}

type provisionOptions struct {
	bootstrapOverride string
	producerIssuerRef string
	producerExec      string
	producerSvc       string
	producerRev       uint64
	producerIssued    time.Time
	producerExpires   time.Time
	// rootStoredMethod is the MethodID written into the root qualification. The
	// approval digest is always recomputed from the fixed bootstrap method so a
	// mismatched stored value exposes a missing verification.
	rootStoredMethod string
	rootIssued       time.Time
	rootExpires      time.Time
	rootAllowedKinds []string
	rootIssuerRef    *string
	producerHostIDs  []string
}

func defaultProvisionOptions() provisionOptions {
	b := testBinding()
	return provisionOptions{producerExec: b.ExecutableDigest, producerSvc: b.ServiceDefinitionDigest, producerRev: b.ProvisioningRevision}
}

// evalTime is after the fixture header's issued_at and inside its validity.
func evalTime() time.Time { return baseTime.Add(2 * time.Second) }

// provisionCatalogOpts writes a root-approver qualification whose bootstrap
// approval is anchored, a host-bound runtime-producer qualification and the
// genesis catalog that references them.
func provisionCatalogOpts(t *testing.T, root string, opts provisionOptions) (string, string) {
	t.Helper()
	p := testPublisher(t, root)

	rootContent := BootstrapApprovalContent{
		HostID: "h1", IssuerID: "issuer-root", ApprovalID: "approval-root", MethodID: "v4-root-provisioning",
		ExecutableDigest: d64("root-exe"), ServiceDefinitionDigest: d64("root-svc"), ProvisioningRevision: 3,
	}
	rootApproval, err := BootstrapApprovalDigest(rootContent)
	if err != nil {
		t.Fatal(err)
	}
	rootAllowedKinds := opts.rootAllowedKinds
	if len(rootAllowedKinds) == 0 {
		rootAllowedKinds = []string{KindIssuerQualification}
	}
	rootStoredMethod := opts.rootStoredMethod
	if rootStoredMethod == "" {
		rootStoredMethod = rootContent.MethodID
	}
	rootHeader := testHeader(KindIssuerQualification)
	if !opts.rootIssued.IsZero() {
		rootHeader.IssuedAt = opts.rootIssued
	}
	if !opts.rootExpires.IsZero() {
		rootHeader.ExpiresAt = opts.rootExpires
	}
	rootRec := Record{Header: rootHeader, Payload: payloadBytes(t, IssuerQualificationRecord{
		IssuerID: rootContent.IssuerID, Role: "root-approver",
		AllowedKinds: rootAllowedKinds, HostIDs: []string{"h1"},
		MethodID: rootStoredMethod, ApprovalID: rootContent.ApprovalID,
		ExecutableDigest: rootContent.ExecutableDigest, ServiceDefinitionDigest: rootContent.ServiceDefinitionDigest,
		ProvisioningRevision: rootContent.ProvisioningRevision, ApprovalDigest: rootApproval, SupportDigests: []string{},
	})}
	rootRec.IssuerQualificationRef = ""
	if opts.rootIssuerRef != nil {
		rootRec.IssuerQualificationRef = *opts.rootIssuerRef
	}
	rootDigest, err := p.publishObject(rootRec)
	if err != nil {
		t.Fatal(err)
	}

	issuerRef := opts.producerIssuerRef
	if issuerRef == "" {
		issuerRef = rootDigest
	}
	producer := validIssuerRecord(t, "runtime-producer", "h1")
	producer.IssuerQualificationRef = issuerRef
	if !opts.producerIssued.IsZero() {
		producer.IssuedAt = opts.producerIssued
	}
	if !opts.producerExpires.IsZero() {
		producer.ExpiresAt = opts.producerExpires
	}
	producerHostIDs := opts.producerHostIDs
	if len(producerHostIDs) == 0 {
		producerHostIDs = []string{"h1"}
	}
	producer.Payload = payloadBytes(t, IssuerQualificationRecord{
		IssuerID: "producer-1", Role: "runtime-producer",
		AllowedKinds: []string{KindDaemonEpoch, KindStartIntent, KindOperationReceipt},
		HostIDs:      producerHostIDs, MethodID: "v4-runtime-producer", ApprovalID: "approval-producer",
		ExecutableDigest: opts.producerExec, ServiceDefinitionDigest: opts.producerSvc,
		ProvisioningRevision: opts.producerRev, ApprovalDigest: d64("pa"), SupportDigests: []string{},
	})
	producerDigest, err := p.publishObject(producer)
	if err != nil {
		t.Fatal(err)
	}

	approval := rootApproval
	if opts.bootstrapOverride != "" {
		approval = opts.bootstrapOverride
	}
	cat := CatalogRecord{Header: testHeader(KindCatalog), Payload: CatalogPayload{
		Sequence: 1, PreviousDigest: "", RootQualificationDigest: rootDigest, BootstrapApprovalDigest: approval,
		Entries: []CatalogEntry{
			{Kind: KindIssuerQualification, Identity: "producer", RecordDigest: producerDigest},
			{Kind: KindIssuerQualification, Identity: "root", RecordDigest: rootDigest},
		},
		RevocationDigests: []string{},
	}}
	if _, err := p.putCatalog(cat); err != nil {
		t.Fatal(err)
	}
	return rootDigest, producerDigest
}

func provisionCatalog(t *testing.T, root string) (string, string) {
	t.Helper()
	return provisionCatalogOpts(t, root, defaultProvisionOptions())
}

func testLease(t *testing.T, root string) *Lease {
	t.Helper()
	l, err := acquireHostLeaseAtMount(root, testTrust, fakeMountID)
	if err != nil {
		t.Fatal(err)
	}
	l.trust = testTrust
	l.pub.trust = testTrust
	l.reader.trust = testTrust
	l.now = func() time.Time { return evalTime() }
	b := testBinding()
	l.supervisor = func() (supervisorBinding, error) { return b, nil }
	t.Cleanup(func() { l.Close() })
	return l
}

func TestPublisherObjectImmutableAndNoReplace(t *testing.T) {
	root := testTree(t)
	p := testPublisher(t, root)
	rec := validEpochRecord(t)
	digest, err := p.publishObject(rec)
	if err != nil {
		t.Fatal(err)
	}
	again, err := p.publishObject(rec)
	if err != nil || again != digest {
		t.Fatalf("re-publish: %v %s", err, again)
	}
	path := filepath.Join(evidenceDir(root), objectsSubdir, digest+".json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if nlink := nlinkOf(t, info); nlink != 1 {
		t.Fatalf("final nlink=%d, want 1", nlink)
	}
	// No temp hardlink may linger in the directory.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".evidence-") {
			t.Fatalf("lingering temp %s", e.Name())
		}
	}
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.publishObject(rec); err == nil {
		t.Fatal("tampered object accepted")
	}
	dir := filepath.Join(evidenceDir(root), "scratch")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := p.writeNoReplace(dir, "x.json", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := p.writeNoReplace(dir, "x.json", []byte("b")); err == nil {
		t.Fatal("writeNoReplace replaced existing file")
	}
}

func TestReaderObjectCatalogTicketRoundTrip(t *testing.T) {
	root := testTree(t)
	provisionCatalog(t, root)
	p := testPublisher(t, root)
	r := testReader(root)

	epoch := validEpochRecord(t)
	digest, err := p.publishObject(epoch)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Object(digest)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindDaemonEpoch {
		t.Fatalf("kind=%s", got.Kind)
	}
	if _, err := r.Object(d64("missing")); err == nil {
		t.Fatal("missing object accepted")
	}

	cat, err := r.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if cat.Payload.Sequence != 1 {
		t.Fatalf("catalog seq=%d", cat.Payload.Sequence)
	}

	base := testHeader(KindTicket)
	tp := TicketPayload{TicketDigest: d64("tkt"), Sequence: 1, PreviousDigest: "", IntentDigest: d64("intent"), OperationID: "op/start", Action: "start", Phase: "prepared", FactDigests: []string{}, WriterEpoch: "epoch"}
	if _, err := p.appendTicket(base, tp); err != nil {
		t.Fatal(err)
	}
	tickets, err := r.Ticket(d64("tkt"))
	if err != nil || len(tickets) != 1 {
		t.Fatalf("ticket read: %v %d", err, len(tickets))
	}
	bad := tp
	bad.Sequence = 2
	bad.PreviousDigest = d64("wrong")
	if _, err := p.appendTicket(base, bad); err == nil {
		t.Fatal("gap ticket accepted")
	}
}

func TestWriterRequiresQualificationAndPublishes(t *testing.T) {
	root := testTree(t)
	_, producerDigest := provisionCatalog(t, root)
	l := testLease(t, root)

	if _, err := l.NewWriter("nobody"); err == nil {
		t.Fatal("writer without qualification accepted")
	}
	w, err := l.NewWriter("h1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.NewWriter("h1"); err == nil {
		t.Fatal("second writer on one lease accepted")
	}
	id, err := w.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if id.HostID != "h1" || id.Epoch == "" || id.IssuerQualificationRef != producerDigest {
		t.Fatalf("identity=%+v", id)
	}

	fact := validEpochRecord(t)
	fact.IssuerQualificationRef = producerDigest
	if _, err := w.Publish(fact); err != nil {
		t.Fatal(err)
	}
	// Idempotent retry of the identical fact must not append a duplicate.
	if _, err := w.Publish(fact); err != nil {
		t.Fatalf("idempotent republish: %v", err)
	}
	cat, err := testReader(root).Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if cat.Payload.Sequence != 2 {
		t.Fatalf("idempotent republish appended: seq=%d", cat.Payload.Sequence)
	}
	bad := fact
	bad.Kind = KindObservation
	if _, err := w.Publish(bad); err == nil {
		t.Fatal("disallowed kind published")
	}
	mismatch := fact
	mismatch.HostID = "other"
	if _, err := w.Publish(mismatch); err == nil {
		t.Fatal("host mismatch published")
	}
}

func TestWriterExecuteStateMachineAndUncertainReceipt(t *testing.T) {
	root := testTree(t)
	_, producerDigest := provisionCatalog(t, root)
	l := testLease(t, root)
	w, err := l.NewWriter("h1")
	if err != nil {
		t.Fatal(err)
	}
	header := testHeader(KindTicket)
	header.IssuerQualificationRef = producerDigest
	ticketDigest := d64("ticket")
	prepared := TicketRecord{Header: header, Payload: TicketPayload{
		TicketDigest: ticketDigest, IntentDigest: d64("intent"), OperationID: "ticket/start",
		Action: "start", Phase: "prepared", FactDigests: []string{}, WriterEpoch: "",
	}}
	if _, err := w.AppendTicket(prepared); err != nil {
		t.Fatal(err)
	}
	dispatch := TicketRecord{Header: header, Payload: TicketPayload{
		TicketDigest: ticketDigest, IntentDigest: d64("intent"), OperationID: "ticket/start",
		Action: "start", Phase: "dispatch-intent", FactDigests: []string{}, WriterEpoch: "",
	}}
	ran := 0
	receiptDigest, err := w.Execute(context.Background(), dispatch, func() (Record, error) {
		ran++
		return Record{}, errors.New("command uncertain")
	})
	if err == nil || receiptDigest == "" {
		t.Fatalf("uncertain command err=%v receipt=%q", err, receiptDigest)
	}
	tickets, err := testReader(root).Ticket(ticketDigest)
	if err != nil {
		t.Fatal(err)
	}
	if len(tickets) != 3 || tickets[1].Payload.Phase != "dispatch-intent" || tickets[2].Payload.Phase != "receipt" {
		t.Fatalf("ticket chain=%+v", tickets)
	}
	// No retry after an uncertain dispatch/receipt.
	if _, err := w.Execute(context.Background(), dispatch, func() (Record, error) {
		ran++
		return Record{}, nil
	}); err == nil {
		t.Fatal("redispatch accepted")
	}
	if ran != 1 {
		t.Fatalf("closure ran %d times", ran)
	}
}

func TestAppendTicketRejectsBadDigestBeforeIO(t *testing.T) {
	root := testTree(t)
	_, producerDigest := provisionCatalog(t, root)
	l := testLease(t, root)
	w, err := l.NewWriter("h1")
	if err != nil {
		t.Fatal(err)
	}
	header := testHeader(KindTicket)
	header.IssuerQualificationRef = producerDigest
	for _, badDigest := range []string{"nothex", "../../escape", strings.Repeat("z", 64)} {
		rec := TicketRecord{Header: header, Payload: TicketPayload{
			TicketDigest: badDigest, IntentDigest: d64("i"), OperationID: "op",
			Action: "start", Phase: "prepared", FactDigests: []string{}, WriterEpoch: "",
		}}
		if _, err := w.AppendTicket(rec); err == nil {
			t.Fatalf("append accepted bad digest %q", badDigest)
		}
		if _, err := os.Stat(filepath.Join(evidenceDir(root), ticketsSubdir, badDigest)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("bad digest %q touched filesystem: %v", badDigest, err)
		}
	}
}

func TestWriterRevalidatesExpiryRevocationAndBinding(t *testing.T) {
	root := testTree(t)
	_, producerDigest := provisionCatalog(t, root)
	l := testLease(t, root)
	w, err := l.NewWriter("h1")
	if err != nil {
		t.Fatal(err)
	}
	fact := validEpochRecord(t)
	fact.IssuerQualificationRef = producerDigest
	if _, err := w.Publish(fact); err != nil {
		t.Fatal(err)
	}
	// Prospective expiry blocks new effects without retracting history.
	l.now = func() time.Time { return baseTime.Add(2 * time.Hour) }
	if _, err := w.Publish(fact); err == nil {
		t.Fatal("expired qualification still published")
	}
	l.now = func() time.Time { return evalTime() }
	if _, err := w.Publish(fact); err != nil {
		t.Fatalf("valid history rejected after clock reset: %v", err)
	}
	// A current revocation of the issuer blocks new effects.
	l.now = func() time.Time { return evalTime() }
	rev := Record{Header: testHeader(KindRevocation), Payload: payloadBytes(t, RevocationPayload{
		RecordDigests: []string{producerDigest}, ReferenceIDs: []string{}, EffectiveAt: baseTime,
		InvalidatesHistory: false, Reason: "superseded", SupportDigests: []string{},
	})}
	rev.IssuerQualificationRef = producerDigest
	revDigest, err := l.pub.publishObject(rev)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := l.reader.Catalog()
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
	next.RevocationDigests = []string{revDigest}
	base := cat.Header
	base.Kind = KindCatalog
	base.IssuerQualificationRef = w.issuerRef
	if _, err := l.pub.putCatalog(CatalogRecord{Header: base, Payload: next}); err != nil {
		t.Fatal(err)
	}
	other := validEpochRecord(t)
	other.Payload = []byte(`{"x":2}`)
	other.IssuerQualificationRef = producerDigest
	if _, err := w.Publish(other); err == nil {
		t.Fatal("revoked issuer still published")
	}
}

func TestQualifyRejectsUnanchoredAuthority(t *testing.T) {
	mutated := map[string]provisionOptions{
		"wrong bootstrap":             {bootstrapOverride: d64("not-the-root-approval")},
		"issuer ref not root":         {producerIssuerRef: d64("someotherroot")},
		"runtime executable mismatch": {producerExec: d64("other-exe")},
		"revision mismatch":           {producerRev: 8},
	}
	for name, opts := range mutated {
		opts := opts
		opts.producerExec = firstNonEmpty(opts.producerExec, testBinding().ExecutableDigest)
		opts.producerSvc = firstNonEmpty(opts.producerSvc, testBinding().ServiceDefinitionDigest)
		if opts.producerRev == 0 {
			opts.producerRev = testBinding().ProvisioningRevision
		}
		root := testTree(t)
		provisionCatalogOpts(t, root, opts)
		if _, _, err := qualifyRuntimeProducer(testReader(root), "h1", evalTime(), testBinding()); err == nil {
			t.Fatalf("%s: unanchored authority accepted", name)
		}
	}

	// Expiry equality must be rejected, not accepted; the same fixture at a
	// time before expiry is valid.
	root := testTree(t)
	provisionCatalog(t, root)
	if _, _, err := qualifyRuntimeProducer(testReader(root), "h1", baseTime.Add(time.Hour), testBinding()); err == nil {
		t.Fatal("expiry equality accepted")
	}
	if _, _, err := qualifyRuntimeProducer(testReader(root), "h1", evalTime(), testBinding()); err != nil {
		t.Fatalf("valid authority at valid time rejected: %v", err)
	}

	// Future-issued qualification is rejected.
	future := defaultProvisionOptions()
	future.producerIssued = evalTime().Add(time.Second)
	futureRoot := testTree(t)
	provisionCatalogOpts(t, futureRoot, future)
	if _, _, err := qualifyRuntimeProducer(testReader(futureRoot), "h1", evalTime(), testBinding()); err == nil {
		t.Fatal("future-issued qualification accepted")
	}
}

func firstNonEmpty(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

func TestLeaseCloseSynchronizesWriter(t *testing.T) {
	root := testTree(t)
	provisionCatalog(t, root)
	l := testLease(t, root)
	w, err := l.NewWriter("h1")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		l.Close()
		close(done)
	}()
	if _, err := w.Identity(); err == nil {
		// Identity may succeed if Close has not started; it must never race, and
		// after Close completes it must fail.
	}
	<-done
	if _, err := w.Identity(); err == nil {
		t.Fatal("identity after close accepted")
	}
	if _, err := l.NewWriter("h1"); err == nil {
		t.Fatal("new writer after close accepted")
	}
	l2, err := acquireHostLeaseAtMount(root, testTrust, fakeMountID)
	if err != nil {
		t.Fatalf("reacquire after close: %v", err)
	}
	l2.Close()
}

func TestLeaseExcludesSecondHolder(t *testing.T) {
	root := testTree(t)
	l1, err := acquireHostLeaseAtMount(root, testTrust, fakeMountID)
	if err != nil {
		t.Fatal(err)
	}
	defer l1.Close()
	if _, err := acquireHostLeaseAtMount(root, testTrust, fakeMountID); err == nil {
		t.Fatal("second lease on the same fixed lock accepted")
	}
}
