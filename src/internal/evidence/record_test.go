package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var baseTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func d64(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func testHeader(kind string) Header {
	return Header{
		SchemaVersion:          SchemaVersion,
		Kind:                   kind,
		HostID:                 "h1",
		IssuerQualificationRef: d64("i"),
		ObservedInterval:       Interval{Start: baseTime, End: baseTime.Add(time.Second)},
		IssuedAt:               baseTime.Add(2 * time.Second),
		ExpiresAt:              baseTime.Add(time.Hour),
		Renewable:              true,
		ReferencedDigests:      []string{},
	}
}

func payloadBytes(t *testing.T, v any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func validEpochRecord(t *testing.T) Record {
	t.Helper()
	// daemon-epoch payload is opaque to the leaf; any object is fine here.
	return Record{Header: testHeader(KindDaemonEpoch), Payload: json.RawMessage(`{"x":1}`)}
}

func validIssuerRecord(t *testing.T, role string, hostID string) Record {
	t.Helper()
	p := IssuerQualificationRecord{
		IssuerID:                "issuer-1",
		Role:                    role,
		AllowedKinds:            []string{KindDaemonEpoch, KindStartIntent},
		HostIDs:                 []string{hostID},
		MethodID:                "method-1",
		ApprovalID:              "approval-1",
		ExecutableDigest:        d64("e"),
		ServiceDefinitionDigest: d64("s"),
		ProvisioningRevision:    1,
		ApprovalDigest:          d64("a"),
		SupportDigests:          []string{},
	}
	return Record{Header: testHeader(KindIssuerQualification), Payload: payloadBytes(t, p)}
}

func TestCanonicalRecordSortsSetsWithoutMutation(t *testing.T) {
	rec := validEpochRecord(t)
	rec.ReferencedDigests = []string{d64("b"), d64("a"), d64("c")}
	original := append([]string(nil), rec.ReferencedDigests...)
	_, first, err := CanonicalRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	for i, got := range rec.ReferencedDigests {
		if got != original[i] {
			t.Fatalf("CanonicalRecord mutated input: %v", rec.ReferencedDigests)
		}
	}
	reordered := rec
	reordered.ReferencedDigests = []string{d64("c"), d64("a"), d64("b")}
	_, second, err := CanonicalRecord(reordered)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("digest order changed canonical digest: %s vs %s", first, second)
	}
}

func TestDecodeRecordRejectsMalformed(t *testing.T) {
	rec := validEpochRecord(t)
	data, _, err := CanonicalRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeRecordBytes(data); err != nil {
		t.Fatalf("valid record rejected: %v", err)
	}

	cases := map[string]func(string) string{
		"unknown field": func(s string) string {
			return strings.Replace(s, `{"schema_version":`, `{"extra":1,"schema_version":`, 1)
		},
		"missing field": func(s string) string { return strings.Replace(s, `"host_id":"h1",`, ``, 1) },
		"null field":    func(s string) string { return strings.Replace(s, `"host_id":"h1"`, `"host_id":null`, 1) },
		"case alias":    func(s string) string { return strings.Replace(s, `"host_id"`, `"Host_ID"`, 1) },
		"trailing json": func(s string) string { return s + ` {}` },
		"wrong schema":  func(s string) string { return strings.Replace(s, `"schema_version":1`, `"schema_version":2`, 1) },
	}
	for name, mutate := range cases {
		if _, err := decodeRecordBytes([]byte(mutate(string(data)))); err == nil {
			t.Fatalf("%s: expected rejection", name)
		}
	}
}

func TestCanonicalRejectsMissingRequiredFalse(t *testing.T) {
	rec := validEpochRecord(t)
	rec.Renewable = false
	rec.ExpiresAt = nonrenewableExpiry
	data, _, err := CanonicalRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "renewable")
	bad, _ := json.Marshal(m)
	if _, err := decodeRecordBytes(bad); err == nil {
		t.Fatal("missing required renewable=false accepted")
	}
}

func TestDecoderRejectsUnknownKindAndOversize(t *testing.T) {
	rec := validEpochRecord(t)
	rec.Kind = "unknown-kind"
	if _, _, err := CanonicalRecord(rec); err == nil {
		t.Fatal("unknown kind accepted")
	}
	data, _, err := CanonicalRecord(validEpochRecord(t))
	if err != nil {
		t.Fatal(err)
	}
	padded := append([]byte(strings.Repeat(" ", MaxObjectBytes+1)), data...)
	if _, err := DecodeRecord(strings.NewReader(string(padded))); err == nil {
		t.Fatal("oversize accepted")
	}
}

func TestCatalogAndTicketPayloadValidation(t *testing.T) {
	cat := CatalogPayload{Sequence: 1, PreviousDigest: "", RootQualificationDigest: d64("r"), BootstrapApprovalDigest: d64("b"),
		Entries: []CatalogEntry{{Kind: KindDaemonEpoch, Identity: "x", RecordDigest: d64("1")}, {Kind: KindDaemonEpoch, Identity: "x", RecordDigest: d64("2")}}, RevocationDigests: []string{}}
	if _, err := normalizeCatalogPayload(cat); err == nil {
		t.Fatal("duplicate catalog identity accepted")
	}
	if _, err := normalizeCatalogPayload(CatalogPayload{Sequence: 1, PreviousDigest: "", RootQualificationDigest: d64("r"), BootstrapApprovalDigest: d64("b")}); err != nil {
		t.Fatalf("empty entries catalog rejected: %v", err)
	}
	if _, err := normalizeTicketPayload(TicketPayload{TicketDigest: d64("t"), Sequence: 1, OperationID: "op", Action: "bogus", Phase: "prepared", WriterEpoch: "e"}); err == nil {
		t.Fatal("bad action accepted")
	}
	if _, err := normalizeTicketPayload(TicketPayload{TicketDigest: d64("t"), Sequence: 1, OperationID: "op", Action: "start", Phase: "bogus", WriterEpoch: "e"}); err == nil {
		t.Fatal("bad phase accepted")
	}
	if _, err := normalizeRevocationPayload(RevocationPayload{EffectiveAt: baseTime, Reason: ""}); err == nil {
		t.Fatal("empty reason accepted")
	}
}

func TestBootstrapApprovalDigestFixedMethod(t *testing.T) {
	c := BootstrapApprovalContent{HostID: "h1", IssuerID: "i", ApprovalID: "a", MethodID: "v4-root-provisioning", ExecutableDigest: d64("e"), ServiceDefinitionDigest: d64("s"), ProvisioningRevision: 1}
	if _, err := BootstrapApprovalDigest(c); err != nil {
		t.Fatal(err)
	}
	c.MethodID = "other"
	if _, err := BootstrapApprovalDigest(c); err == nil {
		t.Fatal("wrong bootstrap method accepted")
	}
}

func TestIssuerQualificationRoleAndDigests(t *testing.T) {
	rec := validIssuerRecord(t, "runtime-producer", "h1")
	if _, _, err := CanonicalRecord(rec); err != nil {
		t.Fatalf("valid issuer qualification rejected: %v", err)
	}
	var p IssuerQualificationRecord
	_ = p
	rec2 := validIssuerRecord(t, "not-a-role", "h1")
	if _, _, err := CanonicalRecord(rec2); err == nil {
		t.Fatal("bad role accepted")
	}
}
