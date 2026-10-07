package evidence

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// Finding 7: the strict decoder and the canonicalizer must agree. Every
// malformed payload that CanonicalRecord rejects must also be rejected by
// decodeRecordBytes and vice versa.
func TestDecodeAndCanonicalRejectConsistently(t *testing.T) {
	cat := CatalogRecord{Header: testHeader(KindCatalog), Payload: CatalogPayload{
		Sequence: 1, PreviousDigest: "", RootQualificationDigest: d64("r"), BootstrapApprovalDigest: d64("b"),
		Entries:           []CatalogEntry{{Kind: KindDaemonEpoch, Identity: "x", RecordDigest: d64("1")}},
		RevocationDigests: []string{},
	}}
	good, _, err := CanonicalRecord(mustRecord(t, cat))
	if err != nil {
		t.Fatalf("valid catalog rejected: %v", err)
	}
	if _, err := decodeRecordBytes(good); err != nil {
		t.Fatalf("valid catalog not decodable: %v", err)
	}

	type tc struct {
		mutate func([]byte) []byte
	}
	cases := map[string]tc{
		"unknown payload key": {func(b []byte) []byte {
			return []byte(strings.Replace(string(b), `"entries":`, `"extra_key":1,"entries":`, 1))
		}},
		"duplicate payload key": {func(b []byte) []byte {
			return []byte(strings.Replace(string(b), `"sequence":`, `"sequence":1,"sequence":`, 1))
		}},
		"null payload slice": {func(b []byte) []byte {
			var m map[string]json.RawMessage
			_ = json.Unmarshal(b, &m)
			var p map[string]json.RawMessage
			_ = json.Unmarshal(m["payload"], &p)
			p["entries"] = json.RawMessage("null")
			m["payload"], _ = json.Marshal(p)
			out, _ := json.Marshal(m)
			return out
		}},
	}
	for name, c := range cases {
		bad := c.mutate(append([]byte(nil), good...))
		if _, err := decodeRecordBytes(bad); err == nil {
			t.Fatalf("%s: decoder accepted malformed payload", name)
		}
		// The canonicalizer must reject the same raw bytes: a raw payload has no
		// Go provenance, so an explicit null is never normalized away.
		var rec Record
		if err := json.Unmarshal(bad, &rec); err != nil {
			t.Fatalf("%s: raw unmarshal: %v", name, err)
		}
		if _, _, err := CanonicalRecord(rec); err == nil {
			t.Fatalf("%s: canonicalizer accepted malformed payload", name)
		}
	}
}

// Finding 7: canonicalization must not silently default an explicitly required
// false/zero/absent field, and must reject malformed digest arrays.
func TestCanonicalRejectsMissingRequiredAndBadDigests(t *testing.T) {
	// Revocation without invalidates_history: strict decoder already rejects
	// presence; canonicalization must reject too rather than defaulting false.
	rev := RevocationRecord{Header: testHeader(KindRevocation), Payload: RevocationPayload{
		RecordDigests: []string{}, ReferenceIDs: []string{}, EffectiveAt: baseTime, InvalidatesHistory: false,
		Reason: "r", SupportDigests: []string{},
	}}
	revBytes, _, err := CanonicalRecord(mustRecord(t, rev))
	if err != nil {
		t.Fatalf("valid revocation: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(revBytes, &m); err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(m["payload"], &payload); err != nil {
		t.Fatal(err)
	}
	delete(payload, "invalidates_history")
	m["payload"], _ = json.Marshal(payload)
	missing, _ := json.Marshal(m)
	if _, err := decodeRecordBytes(missing); err == nil {
		t.Fatal("decoder accepted missing required false")
	}
	var raw Record
	if err := json.Unmarshal(missing, &raw); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CanonicalRecord(raw); err == nil {
		t.Fatal("canonicalizer accepted missing required false")
	}

	// Malformed digest array: issuer qualification support digests.
	bad := validIssuerRecord(t, "runtime-producer", "h1")
	bad.Payload = payloadBytes(t, IssuerQualificationRecord{
		IssuerID: "producer-1", Role: "runtime-producer", AllowedKinds: []string{KindDaemonEpoch},
		HostIDs: []string{"h1"}, MethodID: "m", ApprovalID: "a",
		ExecutableDigest: d64("e"), ServiceDefinitionDigest: d64("s"), ProvisioningRevision: 1,
		ApprovalDigest: d64("a"), SupportDigests: []string{"not-a-digest"},
	})
	if _, _, err := CanonicalRecord(bad); err == nil {
		t.Fatal("canonicalizer accepted malformed digest array")
	}
	// Malformed catalog revocation digest array.
	badCat := CatalogRecord{Header: testHeader(KindCatalog), Payload: CatalogPayload{
		Sequence: 1, PreviousDigest: "", RootQualificationDigest: d64("r"), BootstrapApprovalDigest: d64("b"),
		Entries: []CatalogEntry{}, RevocationDigests: []string{"oops"},
	}}
	if _, _, err := CanonicalRecord(mustRecord(t, badCat)); err == nil {
		t.Fatal("canonicalizer accepted malformed revocation digest")
	}
}

// Finding 7: nil payload slices are normalized by the typed leaf normalizer to
// [] before encoding, so a valid empty catalog round-trips as JSON arrays while
// an explicit null raw payload is rejected by both APIs.
func TestEmptyPayloadSlicesCanonicalizeToArrays(t *testing.T) {
	cat := CatalogRecord{Header: testHeader(KindCatalog), Payload: CatalogPayload{
		Sequence: 1, PreviousDigest: "", RootQualificationDigest: d64("r"), BootstrapApprovalDigest: d64("b"),
		Entries: nil, RevocationDigests: nil,
	}}
	norm, err := normalizeCatalogPayload(cat.Payload)
	if err != nil {
		t.Fatalf("normalize empty catalog: %v", err)
	}
	data, _, err := CanonicalRecord(Record{Header: cat.Header, Payload: payloadBytes(t, norm)})
	if err != nil {
		t.Fatalf("empty catalog rejected: %v", err)
	}
	if !strings.Contains(string(data), `"entries":[]`) || !strings.Contains(string(data), `"revocation_digests":[]`) {
		t.Fatalf("nil slices not normalized to []: %s", data)
	}
	rec, err := decodeRecordBytes(data)
	if err != nil {
		t.Fatalf("canonicalized empty catalog not decodable: %v", err)
	}
	var p CatalogPayload
	if err := json.Unmarshal(rec.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Entries == nil || p.RevocationDigests == nil {
		t.Fatalf("decoded nil slices: %+v", p)
	}

	// The same typed nil slice marshaled directly (no normalization) is an
	// explicit null and must be rejected by the canonicalizer exactly as the
	// decoder rejects it.
	rawNull := Record{Header: cat.Header, Payload: payloadBytes(t, cat.Payload)}
	if _, _, err := CanonicalRecord(rawNull); err == nil {
		t.Fatal("canonicalizer accepted explicit null slices")
	}
}

// Finding 8: tickets are capped at 64KiB, and the cap is enforced before any
// publication/effect, not only at read.
func TestKindByteCapsEnforcedBeforeEffect(t *testing.T) {
	// Build a ticket payload that exceeds 64KiB but is below 1MiB.
	facts := make([]string, 0, 2000)
	for i := 0; i < 2000; i++ {
		facts = append(facts, d64(strconv.Itoa(i)))
	}
	tkt := TicketRecord{Header: testHeader(KindTicket), Payload: TicketPayload{
		TicketDigest: d64("tkt"), Sequence: 1, PreviousDigest: "", IntentDigest: d64("i"),
		OperationID: "op", Action: "start", Phase: "prepared", FactDigests: facts, WriterEpoch: "e",
	}}
	rec := mustRecord(t, tkt)
	canon, _, cerr := CanonicalRecord(rec)
	t.Logf("ticket canonical size=%d err=%v", len(canon), cerr)
	if cerr == nil {
		t.Fatalf("oversize ticket canonicalized (size=%d)", len(canon))
	}
	data, _ := json.Marshal(rec)
	if len(data) <= MaxTicketBytes || len(data) > MaxObjectBytes {
		t.Fatalf("fixture size %d not in (64KiB,1MiB]", len(data))
	}
	if _, err := DecodeRecord(strings.NewReader(string(data))); err == nil {
		t.Fatal("oversize ticket decoded")
	}
}

func mustRecord(t *testing.T, v any) Record {
	t.Helper()
	switch r := v.(type) {
	case Record:
		return r
	case CatalogRecord:
		return Record{Header: r.Header, Payload: payloadBytes(t, r.Payload)}
	case TicketRecord:
		return Record{Header: r.Header, Payload: payloadBytes(t, r.Payload)}
	case RevocationRecord:
		return Record{Header: r.Header, Payload: payloadBytes(t, r.Payload)}
	default:
		t.Fatalf("unsupported record type %T", v)
		return Record{}
	}
}
