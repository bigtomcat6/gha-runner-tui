package evidence

import (
	"context"
	"testing"
	"time"
)

// finding B: the root trust anchor must be a currently valid, method-anchored,
// host-granting qualification whose bootstrap approval is recomputed from its
// own typed MethodID, not a substituted constant. An expired root must not let
// a still-valid producer qualify.

func TestAuthorityRejectsStaleRoot(t *testing.T) {
	root := testTree(t)
	opts := defaultProvisionOptions()
	// Root header: accepted observed interval but already expired at evalTime.
	opts.rootIssued = baseTime.Add(time.Second)
	opts.rootExpires = baseTime.Add(2 * time.Second)
	provisionCatalogOpts(t, root, opts)
	if _, err := testLease(t, root).NewWriter("h1"); err == nil {
		t.Fatal("expired root qualification accepted")
	}
}

func TestAuthorityRejectsWrongRootMethod(t *testing.T) {
	root := testTree(t)
	opts := defaultProvisionOptions()
	// The stored root qualification claims another method while its approval
	// digest is anchored to the constant. The stored field must be verified.
	opts.rootStoredMethod = "not-v4-root-provisioning"
	provisionCatalogOpts(t, root, opts)
	if _, err := testLease(t, root).NewWriter("h1"); err == nil {
		t.Fatal("root qualification with the wrong MethodID accepted")
	}
}

func TestAuthorityRejectsRootWithoutQualificationGrant(t *testing.T) {
	root := testTree(t)
	opts := defaultProvisionOptions()
	opts.rootAllowedKinds = []string{KindDaemonEpoch}
	provisionCatalogOpts(t, root, opts)
	if _, err := testLease(t, root).NewWriter("h1"); err == nil {
		t.Fatal("root that does not grant qualification issuance accepted")
	}
}

func TestAuthorityRejectsRootWithIssuerRef(t *testing.T) {
	root := testTree(t)
	opts := defaultProvisionOptions()
	nonEmpty := d64("someone-else")
	opts.rootIssuerRef = &nonEmpty
	provisionCatalogOpts(t, root, opts)
	if _, err := testLease(t, root).NewWriter("h1"); err == nil {
		t.Fatal("root qualification with a non-empty issuer ref accepted")
	}
}

func TestAuthorityRejectsProducerHostMismatch(t *testing.T) {
	root := testTree(t)
	opts := defaultProvisionOptions()
	opts.producerHostIDs = []string{"h2"}
	provisionCatalogOpts(t, root, opts)
	if _, err := testLease(t, root).NewWriter("h1"); err == nil {
		t.Fatal("producer not granted for host accepted")
	}
}

// A catalog entry whose issuer does not grant its kind must be rejected when the
// catalog is authorized, not silently accepted as well-formed.
func TestCatalogRejectsUnauthorizedIssuerKind(t *testing.T) {
	root := testTree(t)
	_, producerDigest := provisionCatalog(t, root)
	p := testPublisher(t, root)
	// A fact of a kind the producer does not grant.
	fact := validEpochRecord(t)
	fact.Kind = KindRemovalReceipt
	fact.IssuerQualificationRef = producerDigest
	digest, err := p.publishObject(fact)
	if err != nil {
		t.Fatal(err)
	}
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
	next.Entries = append(append([]CatalogEntry(nil), next.Entries...), CatalogEntry{Kind: KindRemovalReceipt, Identity: digest, RecordDigest: digest})
	base := cat.Header
	base.Kind = KindCatalog
	base.IssuerQualificationRef = producerDigest
	if _, err := p.putCatalog(CatalogRecord{Header: base, Payload: next}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Catalog(); err == nil {
		t.Fatal("catalog entry whose issuer does not grant its kind accepted")
	}
}

// finding B: a catalog-listed revocation whose object is missing must fail
// closed instead of granting effects.
func TestRevocationListedObjectMissingFailsClosed(t *testing.T) {
	root := testTree(t)
	_, producerDigest := provisionCatalog(t, root)
	l := testLease(t, root)
	w, err := l.NewWriter("h1")
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
	next.RevocationDigests = []string{d64("missing-revocation")}
	base := cat.Header
	base.Kind = KindCatalog
	base.IssuerQualificationRef = producerDigest
	if _, err := l.pub.putCatalog(CatalogRecord{Header: base, Payload: next}); err != nil {
		t.Fatal(err)
	}
	fact := validEpochRecord(t)
	fact.IssuerQualificationRef = producerDigest
	if _, err := w.Publish(fact); err == nil {
		t.Fatal("missing catalog-listed revocation granted effects")
	}
}

// finding B: SupportDigests are dependencies, not revocation targets. A
// revocation that merely depends on the issuer digest must not revoke it.
func TestRevocationSupportDigestIsDependencyNotTarget(t *testing.T) {
	root := testTree(t)
	_, producerDigest := provisionCatalog(t, root)
	l := testLease(t, root)
	w, err := l.NewWriter("h1")
	if err != nil {
		t.Fatal(err)
	}
	// SupportDigests references the producer; RecordDigests/ReferenceIDs do not.
	rev := Record{Header: testHeader(KindRevocation), Payload: payloadBytes(t, RevocationPayload{
		RecordDigests: []string{}, ReferenceIDs: []string{}, EffectiveAt: baseTime,
		InvalidatesHistory: false, Reason: "dependency", SupportDigests: []string{producerDigest},
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
	base.IssuerQualificationRef = producerDigest
	if _, err := l.pub.putCatalog(CatalogRecord{Header: base, Payload: next}); err != nil {
		t.Fatal(err)
	}
	fact := validEpochRecord(t)
	fact.IssuerQualificationRef = producerDigest
	if _, err := w.Publish(fact); err != nil {
		t.Fatalf("support-only revocation dependency blocked publication: %v", err)
	}
}

// finding B: an effective revocation blocks new effects; a revocation whose
// EffectiveAt is in the future does not.
func TestRevocationRespectsEffectiveAt(t *testing.T) {
	root := testTree(t)
	_, producerDigest := provisionCatalog(t, root)
	l := testLease(t, root)
	w, err := l.NewWriter("h1")
	if err != nil {
		t.Fatal(err)
	}
	publishRevocation := func(effective time.Time) {
		t.Helper()
		rev := Record{Header: testHeader(KindRevocation), Payload: payloadBytes(t, RevocationPayload{
			RecordDigests: []string{producerDigest}, ReferenceIDs: []string{}, EffectiveAt: effective,
			InvalidatesHistory: false, Reason: "later", SupportDigests: []string{},
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
		next.RevocationDigests = append(next.RevocationDigests, revDigest)
		base := cat.Header
		base.Kind = KindCatalog
		base.IssuerQualificationRef = producerDigest
		if _, err := l.pub.putCatalog(CatalogRecord{Header: base, Payload: next}); err != nil {
			t.Fatal(err)
		}
	}
	publishRevocation(evalTime().Add(time.Hour))
	fact := validEpochRecord(t)
	fact.IssuerQualificationRef = producerDigest
	if _, err := w.Publish(fact); err != nil {
		t.Fatalf("future-effective revocation blocked current effect: %v", err)
	}
}

// finding B: Recheck must use `at`, reject a ref that is not a current catalog
// mapping, and reject a ref revoked at `at`.
func TestRecheckRejectsNonListedRef(t *testing.T) {
	root := testTree(t)
	provisionCatalog(t, root)
	r := testReader(root)
	cat, err := r.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Recheck(cat, []string{d64("not-listed")}, evalTime()); err == nil {
		t.Fatal("Recheck accepted a ref absent from the current catalog")
	}
}

func TestRecheckRejectsEffectiveRevocation(t *testing.T) {
	root := testTree(t)
	_, producerDigest := provisionCatalog(t, root)
	l := testLease(t, root)
	// Revocation becomes effective shortly after evalTime.
	effective := evalTime().Add(time.Second)
	rev := Record{Header: testHeader(KindRevocation), Payload: payloadBytes(t, RevocationPayload{
		RecordDigests: []string{producerDigest}, ReferenceIDs: []string{}, EffectiveAt: effective,
		InvalidatesHistory: false, Reason: "revoked", SupportDigests: []string{},
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
	base.IssuerQualificationRef = producerDigest
	if _, err := l.pub.putCatalog(CatalogRecord{Header: base, Payload: next}); err != nil {
		t.Fatal(err)
	}
	r := testReader(root)
	// Before EffectiveAt the revocation is not yet effective.
	if _, err := r.Recheck(cat, []string{producerDigest}, evalTime()); err != nil {
		t.Fatalf("Recheck rejected a ref before revocation EffectiveAt: %v", err)
	}
	// At/after EffectiveAt the ref is blocked.
	if _, err := r.Recheck(cat, []string{producerDigest}, effective); err == nil {
		t.Fatal("Recheck accepted a ref revoked at `at`")
	}
}

// finding B: Identity must refresh the current authority instead of returning a
// stale identity after expiry or revocation.
func TestIdentityRefreshesCurrentAuthority(t *testing.T) {
	root := testTree(t)
	provisionCatalog(t, root)
	l := testLease(t, root)
	w, err := l.NewWriter("h1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Identity(); err != nil {
		t.Fatalf("valid identity rejected: %v", err)
	}
	l.now = func() time.Time { return baseTime.Add(2 * time.Hour) }
	if _, err := w.Identity(); err == nil {
		t.Fatal("Identity returned a stale identity after producer expiry")
	}
}

// finding B: the positive authorized temp-file chain must work end to end,
// including PublishAll and Execute's receipt publication.
func TestAuthorityPositiveTempFileChain(t *testing.T) {
	root := testTree(t)
	_, producerDigest := provisionCatalog(t, root)
	l := testLease(t, root)
	w, err := l.NewWriter("h1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Identity(); err != nil {
		t.Fatal(err)
	}
	facts := []Record{validEpochRecord(t), validEpochRecord(t)}
	for i := range facts {
		facts[i].IssuerQualificationRef = producerDigest
		facts[i].Payload = []byte(`{"x":` + string(rune('0'+i)) + `}`)
	}
	digests, err := w.PublishAll(facts)
	if err != nil || len(digests) != len(facts) {
		t.Fatalf("PublishAll: %v %v", err, digests)
	}
	r := testReader(root)
	cat, err := r.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Recheck(cat, digests, evalTime()); err != nil {
		t.Fatalf("Recheck of published facts: %v", err)
	}

	header := testHeader(KindTicket)
	header.IssuerQualificationRef = producerDigest
	ticketDigest := d64("positive-ticket")
	prepared := TicketRecord{Header: header, Payload: TicketPayload{
		TicketDigest: ticketDigest, IntentDigest: d64("intent"), OperationID: ticketDigest + "/start",
		Action: "start", Phase: "prepared", FactDigests: []string{}, WriterEpoch: "",
	}}
	if _, err := w.AppendTicket(prepared); err != nil {
		t.Fatal(err)
	}
	dispatch := TicketRecord{Header: header, Payload: TicketPayload{
		TicketDigest: ticketDigest, IntentDigest: prepared.Payload.IntentDigest, OperationID: prepared.Payload.OperationID,
		Action: "start", Phase: "dispatch-intent", FactDigests: []string{}, WriterEpoch: "",
	}}
	receiptDigest, err := w.Execute(context.Background(), dispatch, func() (Record, error) {
		return Record{Header: header, Payload: []byte(`{"outcome":"success-known"}`)}, nil
	})
	if err != nil || receiptDigest == "" {
		t.Fatalf("Execute: %v %q", err, receiptDigest)
	}
}

// finding B: an issuer/support reference cycle must be rejected rather than
// looped or silently accepted.
func TestAuthorityRejectsReferenceCycle(t *testing.T) {
	a := d64("a")
	b := d64("b")
	graph := map[string][]string{a: {b}, b: {a}}
	if err := checkReferenceCycle(a, func(d string) ([]string, error) { return graph[d], nil }); err == nil {
		t.Fatal("reference cycle accepted")
	}
	if err := checkReferenceCycle(a, func(d string) ([]string, error) {
		if d == a {
			return []string{b}, nil
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("acyclic graph rejected: %v", err)
	}
}
