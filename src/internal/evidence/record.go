// Package evidence implements the protected evidence v1 leaf file format,
// strict decoder, canonicalizer and immutable publisher/lease file mechanics
// described by spec 6.1-6.7. It is a leaf package: stdlib only, no imports of
// config/github/docker/host.
package evidence

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"time"
)

// SchemaVersion is the only accepted envelope schema.
const SchemaVersion = 1

const (
	// MaxObjectBytes bounds objects and catalog records (spec 6.2).
	MaxObjectBytes = 1 << 20
	// MaxTicketBytes bounds ticket records (spec 6.2).
	MaxTicketBytes = 64 << 10
)

// Fixed nonrenewable expiry for historical facts (spec 6.2).
var nonrenewableExpiry = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)

// Kinds fixed by spec 6.2.
const (
	KindIssuerQualification           = "issuer-qualification"
	KindCatalog                       = "catalog"
	KindTicket                        = "ticket"
	KindRevocation                    = "revocation"
	KindHostAudit                     = "host-audit"
	KindEvidenceSourceQualification   = "evidence-source-qualification"
	KindExecutionQualification        = "execution-qualification"
	KindDaemonEpoch                   = "daemon-epoch"
	KindStartIntent                   = "start-intent"
	KindOperationReceipt              = "operation-receipt"
	KindTerminalBarrier               = "terminal-barrier"
	KindEventChunk                    = "event-chunk"
	KindEventCoverage                 = "event-coverage"
	KindRemovalReceipt                = "removal-receipt"
	KindObservation                   = "observation"
	KindBudgetMeasurement             = "budget-measurement"
	KindCredentialSelection           = "credential-selection"
	KindCredentialInvocationBinding   = "credential-invocation-binding"
	KindStableCredentialQualification = "stable-credential-qualification"
	KindManagementQualification       = "management-qualification"
	KindScopeAuthorityQualification   = "scope-authority-qualification"
)

var validKinds = map[string]bool{
	KindIssuerQualification:           true,
	KindCatalog:                       true,
	KindTicket:                        true,
	KindRevocation:                    true,
	KindHostAudit:                     true,
	KindEvidenceSourceQualification:   true,
	KindExecutionQualification:        true,
	KindDaemonEpoch:                   true,
	KindStartIntent:                   true,
	KindOperationReceipt:              true,
	KindTerminalBarrier:               true,
	KindEventChunk:                    true,
	KindEventCoverage:                 true,
	KindRemovalReceipt:                true,
	KindObservation:                   true,
	KindBudgetMeasurement:             true,
	KindCredentialSelection:           true,
	KindCredentialInvocationBinding:   true,
	KindStableCredentialQualification: true,
	KindManagementQualification:       true,
	KindScopeAuthorityQualification:   true,
}

// Roles fixed by spec 6.2.
var validRoles = map[string]bool{
	"root-approver":        true,
	"host-auditor":         true,
	"source-qualifier":     true,
	"budget-qualifier":     true,
	"credential-qualifier": true,
	"runtime-producer":     true,
}

var (
	ErrEvidenceEncoding        = errors.New("EVIDENCE_ENCODING")
	ErrEvidenceTrust           = errors.New("EVIDENCE_TRUST")
	ErrUnsupportedEvidenceHost = errors.New("UNSUPPORTED_EVIDENCE_HOST")
	ErrNoCatalog               = errors.New("EVIDENCE_NO_CATALOG")
	ErrEvidenceChain           = errors.New("EVIDENCE_CHAIN")
	ErrEvidenceWriter          = errors.New("EVIDENCE_WRITER")
)

type Interval struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

type Header struct {
	SchemaVersion          int       `json:"schema_version"`
	Kind                   string    `json:"kind"`
	HostID                 string    `json:"host_id"`
	IssuerQualificationRef string    `json:"issuer_qualification_ref"`
	ObservedInterval       Interval  `json:"observed_interval"`
	IssuedAt               time.Time `json:"issued_at"`
	ExpiresAt              time.Time `json:"expires_at"`
	Renewable              bool      `json:"renewable"`
	ReferencedDigests      []string  `json:"referenced_digests"`
}

type Record struct {
	Header
	Payload json.RawMessage `json:"payload"`
}

type CatalogEntry struct {
	Kind         string `json:"kind"`
	Identity     string `json:"identity"`
	RecordDigest string `json:"record_digest"`
}

type CatalogPayload struct {
	Sequence                uint64         `json:"sequence"`
	PreviousDigest          string         `json:"previous_digest"`
	RootQualificationDigest string         `json:"root_qualification_digest"`
	BootstrapApprovalDigest string         `json:"bootstrap_approval_digest"`
	Entries                 []CatalogEntry `json:"entries"`
	RevocationDigests       []string       `json:"revocation_digests"`
}

type CatalogRecord struct {
	Header
	Payload CatalogPayload `json:"payload"`
}

type TicketPayload struct {
	TicketDigest   string   `json:"ticket_digest"`
	Sequence       uint64   `json:"sequence"`
	PreviousDigest string   `json:"previous_digest"`
	IntentDigest   string   `json:"intent_digest"`
	OperationID    string   `json:"operation_id"`
	Action         string   `json:"action"`
	Phase          string   `json:"phase"`
	FactDigests    []string `json:"fact_digests"`
	WriterEpoch    string   `json:"writer_epoch"`
}

type TicketRecord struct {
	Header
	Payload TicketPayload `json:"payload"`
}

type RevocationPayload struct {
	RecordDigests      []string  `json:"record_digests"`
	ReferenceIDs       []string  `json:"reference_ids"`
	EffectiveAt        time.Time `json:"effective_at"`
	InvalidatesHistory bool      `json:"invalidates_history"`
	Reason             string    `json:"reason"`
	SupportDigests     []string  `json:"support_digests"`
}

type RevocationRecord struct {
	Header
	Payload RevocationPayload `json:"payload"`
}

type IssuerQualificationRecord struct {
	IssuerID                string   `json:"issuer_id"`
	Role                    string   `json:"role"`
	AllowedKinds            []string `json:"allowed_kinds"`
	HostIDs                 []string `json:"host_ids"`
	MethodID                string   `json:"method_id"`
	ApprovalID              string   `json:"approval_id"`
	ExecutableDigest        string   `json:"executable_digest"`
	ServiceDefinitionDigest string   `json:"service_definition_digest"`
	ProvisioningRevision    uint64   `json:"provisioning_revision"`
	ApprovalDigest          string   `json:"approval_digest"`
	SupportDigests          []string `json:"support_digests"`
}

type BootstrapApprovalContent struct {
	HostID                  string `json:"host_id"`
	IssuerID                string `json:"issuer_id"`
	ApprovalID              string `json:"approval_id"`
	MethodID                string `json:"method_id"`
	ExecutableDigest        string `json:"executable_digest"`
	ServiceDefinitionDigest string `json:"service_definition_digest"`
	ProvisioningRevision    uint64 `json:"provisioning_revision"`
}

var (
	rawMessageType = reflect.TypeOf(json.RawMessage(nil))
	timeType       = reflect.TypeOf(time.Time{})
	headerType     = reflect.TypeOf(Header{})
)

func isLowerHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func validDigest(s string) bool { return isLowerHex64(s) }

// maxBytesForKind is the exact canonical byte cap for one envelope: tickets are
// 64KiB, every other object/catalog record is 1MiB (spec 6.2).
func maxBytesForKind(kind string) int {
	if kind == KindTicket {
		return MaxTicketBytes
	}
	return MaxObjectBytes
}

func validateDigestList(field string, list []string) error {
	for _, d := range list {
		if !validDigest(d) {
			return fmt.Errorf("%w: %s", ErrEvidenceEncoding, field)
		}
	}
	return nil
}

func validateStringList(field string, list []string) error {
	for _, s := range list {
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("%w: %s", ErrEvidenceEncoding, field)
		}
	}
	return nil
}

// strictPayload applies the same presence and unknown/duplicate/null checks in
// both the decoder and the canonicalizer so the two can never disagree. A raw
// payload carries no Go provenance, so an explicit JSON null is always rejected
// in either direction; a typed nil slice must be normalized to [] by the typed
// leaf normalizer before it is marshaled into a Record payload.
func strictPayload(kind string, raw json.RawMessage, out any) error {
	if len(raw) == 0 || isNull(raw) {
		return fmt.Errorf("%w: payload", ErrEvidenceEncoding)
	}
	t := payloadType(kind)
	if t == nil {
		return nil
	}
	if err := requirePresence(raw, t); err != nil {
		return err
	}
	if err := strictJSON(raw, out); err != nil {
		return fmt.Errorf("%w: %v", ErrEvidenceEncoding, err)
	}
	return nil
}

// jsonFields mirrors the host store's strict field discovery and follows
// anonymous embedded structs so the flat header tags appear at top level.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	fields := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue
		}
		tag := strings.Split(f.Tag.Get("json"), ",")[0]
		if tag == "-" {
			continue
		}
		if f.Anonymous && tag == "" {
			for name, typ := range jsonFields(f.Type) {
				fields[name] = typ
			}
			continue
		}
		if tag == "" {
			tag = f.Name
		}
		fields[tag] = f.Type
	}
	return fields
}

func isNull(v json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(v), []byte("null"))
}

// requirePresence rejects missing, null and unknown-shape required fields so a
// Go zero value never substitutes for an explicit false/zero (spec 6.2). A null
// slice is rejected, not normalized: the decoder and canonicalizer share this
// rule and typed nil slices are normalized to [] before encoding.
func requirePresence(data []byte, t reflect.Type) error {
	if t == nil {
		return nil
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == rawMessageType || t == timeType {
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		m := map[string]json.RawMessage{}
		if err := json.Unmarshal(data, &m); err != nil || m == nil {
			return fmt.Errorf("%w: expected object", ErrEvidenceEncoding)
		}
		for name, ft := range jsonFields(t) {
			v, ok := m[name]
			if !ok {
				return fmt.Errorf("%w: missing required field %q", ErrEvidenceEncoding, name)
			}
			ft = derefType(ft)
			if isNull(v) {
				return fmt.Errorf("%w: missing required field %q", ErrEvidenceEncoding, name)
			}
			if err := requirePresence(v, ft); err != nil {
				return err
			}
		}
	case reflect.Slice:
		if isNull(data) {
			return fmt.Errorf("%w: null slice", ErrEvidenceEncoding)
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(data, &arr); err != nil {
			return fmt.Errorf("%w: expected array", ErrEvidenceEncoding)
		}
		for _, v := range arr {
			if err := requirePresence(v, t.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}

func derefType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// walkJSON token-walks before typed decoding so duplicates, case aliases and
// unknown fields are rejected; RawMessage payloads are consumed opaquely.
func walkJSON(d *json.Decoder, t reflect.Type) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == rawMessageType {
		var raw json.RawMessage
		return d.Decode(&raw)
	}
	tok, err := d.Token()
	if err != nil {
		return err
	}
	if tok == nil {
		return nil
	}
	if delim, ok := tok.(json.Delim); ok {
		switch delim {
		case '{':
			if t.Kind() != reflect.Struct {
				return errors.New("unexpected object")
			}
			fields, seen := jsonFields(t), map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok {
					return errors.New("invalid key")
				}
				typ, ok := fields[name]
				if !ok || seen[name] {
					return fmt.Errorf("unknown or duplicate field %q", name)
				}
				seen[name] = true
				if err := walkJSON(d, typ); err != nil {
					return err
				}
			}
		case '[':
			if t.Kind() != reflect.Slice && t.Kind() != reflect.Array {
				return errors.New("unexpected array")
			}
			for d.More() {
				if err := walkJSON(d, t.Elem()); err != nil {
					return err
				}
			}
		default:
			return errors.New("unexpected delimiter")
		}
		_, err = d.Token()
		return err
	}
	return nil
}

func strictJSON(data []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := walkJSON(d, reflect.TypeOf(out).Elem()); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(out)
}

// DecodeRecord strict-decodes one envelope object from r (spec 6.2).
func DecodeRecord(r io.Reader) (Record, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxObjectBytes+1))
	if err != nil {
		return Record{}, err
	}
	if len(data) > MaxObjectBytes {
		return Record{}, fmt.Errorf("%w: object exceeds %d bytes", ErrEvidenceEncoding, MaxObjectBytes)
	}
	return decodeRecordBytes(data)
}

func decodeRecordBytes(data []byte) (Record, error) {
	var rec Record
	if err := strictJSON(data, &rec); err != nil {
		return Record{}, fmt.Errorf("%w: %v", ErrEvidenceEncoding, err)
	}
	if err := requirePresence(data, reflect.TypeOf(Record{})); err != nil {
		return Record{}, err
	}
	if err := validateHeader(rec.Header); err != nil {
		return Record{}, err
	}
	if limit := maxBytesForKind(rec.Kind); len(data) > limit {
		return Record{}, fmt.Errorf("%w: %s record exceeds %d bytes", ErrEvidenceEncoding, rec.Kind, limit)
	}
	// Input mode: reject a null slice outright before normalization can turn it
	// into []; the canonicalizer accepts only Go nil slices emitted by a struct.
	if err := requirePresence(rec.Payload, payloadType(rec.Kind)); err != nil {
		return Record{}, err
	}
	payload, err := normalizePayload(rec.Kind, rec.Payload)
	if err != nil {
		return Record{}, err
	}
	rec.Payload = payload
	return rec, nil
}

func payloadType(kind string) reflect.Type {
	switch kind {
	case KindIssuerQualification:
		return reflect.TypeOf(IssuerQualificationRecord{})
	case KindCatalog:
		return reflect.TypeOf(CatalogPayload{})
	case KindTicket:
		return reflect.TypeOf(TicketPayload{})
	case KindRevocation:
		return reflect.TypeOf(RevocationPayload{})
	default:
		return nil
	}
}

func validateHeader(h Header) error {
	if h.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: schema_version", ErrEvidenceEncoding)
	}
	if !validKinds[h.Kind] {
		return fmt.Errorf("%w: kind %q", ErrEvidenceEncoding, h.Kind)
	}
	if strings.TrimSpace(h.HostID) == "" {
		return fmt.Errorf("%w: host_id", ErrEvidenceEncoding)
	}
	st, en, iss, exp := h.ObservedInterval.Start, h.ObservedInterval.End, h.IssuedAt, h.ExpiresAt
	if st.IsZero() || en.IsZero() || iss.IsZero() || exp.IsZero() {
		return fmt.Errorf("%w: zero time", ErrEvidenceEncoding)
	}
	if st.After(en) || en.After(iss) {
		return fmt.Errorf("%w: observed interval", ErrEvidenceEncoding)
	}
	if h.Renewable {
		if !iss.Before(exp) {
			return fmt.Errorf("%w: renewable expiry", ErrEvidenceEncoding)
		}
	} else if !exp.Equal(nonrenewableExpiry) {
		return fmt.Errorf("%w: nonrenewable expiry not fixed", ErrEvidenceEncoding)
	}
	if h.Kind != KindIssuerQualification {
		if !validDigest(h.IssuerQualificationRef) {
			return fmt.Errorf("%w: issuer qualification ref", ErrEvidenceEncoding)
		}
	} else if h.IssuerQualificationRef != "" && !validDigest(h.IssuerQualificationRef) {
		return fmt.Errorf("%w: issuer qualification ref", ErrEvidenceEncoding)
	}
	for _, d := range h.ReferencedDigests {
		if !validDigest(d) {
			return fmt.Errorf("%w: referenced digest", ErrEvidenceEncoding)
		}
	}
	return nil
}

// CanonicalRecord normalizes and canonicalizes rec, returning the immutable
// bytes and their lowercase SHA256 digest. Sets are copied before sorting, so
// the caller's Record is never mutated.
func CanonicalRecord(rec Record) ([]byte, string, error) {
	norm, err := normalizeRecord(rec)
	if err != nil {
		return nil, "", err
	}
	data, err := json.Marshal(norm)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrEvidenceEncoding, err)
	}
	if limit := maxBytesForKind(norm.Kind); len(data) > limit {
		return nil, "", fmt.Errorf("%w: %s record exceeds %d bytes", ErrEvidenceEncoding, norm.Kind, limit)
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), nil
}

func normalizeRecord(rec Record) (Record, error) {
	if err := validateHeader(rec.Header); err != nil {
		return Record{}, err
	}
	out := rec
	out.ObservedInterval.Start = rec.ObservedInterval.Start.UTC()
	out.ObservedInterval.End = rec.ObservedInterval.End.UTC()
	out.IssuedAt = rec.IssuedAt.UTC()
	out.ExpiresAt = rec.ExpiresAt.UTC()
	out.ReferencedDigests = sortedUnique(rec.ReferencedDigests)

	payload, err := normalizePayload(rec.Kind, rec.Payload)
	if err != nil {
		return Record{}, err
	}
	out.Payload = payload
	return out, nil
}

func normalizePayload(kind string, raw json.RawMessage) (json.RawMessage, error) {
	switch kind {
	case KindIssuerQualification:
		var p IssuerQualificationRecord
		if err := strictPayload(kind, raw, &p); err != nil {
			return nil, err
		}
		np, err := normalizeIssuerQualification(p)
		if err != nil {
			return nil, err
		}
		return marshalPayload(np)
	case KindCatalog:
		var p CatalogPayload
		if err := strictPayload(kind, raw, &p); err != nil {
			return nil, err
		}
		np, err := normalizeCatalogPayload(p)
		if err != nil {
			return nil, err
		}
		return marshalPayload(np)
	case KindTicket:
		var p TicketPayload
		if err := strictPayload(kind, raw, &p); err != nil {
			return nil, err
		}
		np, err := normalizeTicketPayload(p)
		if err != nil {
			return nil, err
		}
		return marshalPayload(np)
	case KindRevocation:
		var p RevocationPayload
		if err := strictPayload(kind, raw, &p); err != nil {
			return nil, err
		}
		np, err := normalizeRevocationPayload(p)
		if err != nil {
			return nil, err
		}
		return marshalPayload(np)
	default:
		// Typed normalization for docker/github payloads happens in the owning
		// package before CanonicalRecord is called; the leaf still rejects a
		// missing/null payload and compacts the JSON so whitespace cannot
		// change the digest.
		if len(raw) == 0 || isNull(raw) {
			return nil, fmt.Errorf("%w: payload", ErrEvidenceEncoding)
		}
		var buf bytes.Buffer
		if err := json.Compact(&buf, raw); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrEvidenceEncoding, err)
		}
		trimmed := bytes.TrimSpace(buf.Bytes())
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return nil, fmt.Errorf("%w: payload not an object", ErrEvidenceEncoding)
		}
		return json.RawMessage(trimmed), nil
	}
}

func marshalPayload(v any) (json.RawMessage, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEvidenceEncoding, err)
	}
	return json.RawMessage(data), nil
}

func normalizeIssuerQualification(p IssuerQualificationRecord) (IssuerQualificationRecord, error) {
	if !validRoles[p.Role] {
		return p, fmt.Errorf("%w: issuer role %q", ErrEvidenceEncoding, p.Role)
	}
	if strings.TrimSpace(p.IssuerID) == "" || strings.TrimSpace(p.MethodID) == "" || strings.TrimSpace(p.ApprovalID) == "" {
		return p, fmt.Errorf("%w: issuer qualification identity", ErrEvidenceEncoding)
	}
	for _, d := range []string{p.ExecutableDigest, p.ServiceDefinitionDigest, p.ApprovalDigest} {
		if !validDigest(d) {
			return p, fmt.Errorf("%w: issuer qualification digest", ErrEvidenceEncoding)
		}
	}
	if p.ProvisioningRevision == 0 {
		return p, fmt.Errorf("%w: provisioning revision", ErrEvidenceEncoding)
	}
	if err := validateStringList("host ids", p.HostIDs); err != nil {
		return p, err
	}
	if err := validateDigestList("support digests", p.SupportDigests); err != nil {
		return p, err
	}
	for _, k := range p.AllowedKinds {
		if !validKinds[k] {
			return p, fmt.Errorf("%w: allowed kind %q", ErrEvidenceEncoding, k)
		}
	}
	p.AllowedKinds = sortedUnique(p.AllowedKinds)
	p.HostIDs = sortedUnique(p.HostIDs)
	p.SupportDigests = sortedUnique(p.SupportDigests)
	return p, nil
}

func normalizeCatalogPayload(p CatalogPayload) (CatalogPayload, error) {
	if p.Sequence == 0 {
		return p, fmt.Errorf("%w: catalog sequence", ErrEvidenceEncoding)
	}
	if !validDigest(p.RootQualificationDigest) || !validDigest(p.BootstrapApprovalDigest) {
		return p, fmt.Errorf("%w: catalog genesis digests", ErrEvidenceEncoding)
	}
	if p.Sequence == 1 {
		if p.PreviousDigest != "" {
			return p, fmt.Errorf("%w: catalog genesis predecessor", ErrEvidenceEncoding)
		}
	} else if !validDigest(p.PreviousDigest) {
		return p, fmt.Errorf("%w: catalog predecessor", ErrEvidenceEncoding)
	}
	if err := validateDigestList("revocation digests", p.RevocationDigests); err != nil {
		return p, err
	}
	entries := make([]CatalogEntry, 0, len(p.Entries))
	entries = append(entries, p.Entries...)
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Kind != entries[j].Kind {
			return entries[i].Kind < entries[j].Kind
		}
		return entries[i].Identity < entries[j].Identity
	})
	for i := range entries {
		if !validKinds[entries[i].Kind] || strings.TrimSpace(entries[i].Identity) == "" || !validDigest(entries[i].RecordDigest) {
			return p, fmt.Errorf("%w: catalog entry", ErrEvidenceEncoding)
		}
		if i > 0 && entries[i].Kind == entries[i-1].Kind && entries[i].Identity == entries[i-1].Identity {
			return p, fmt.Errorf("%w: duplicate catalog identity", ErrEvidenceEncoding)
		}
	}
	p.Entries = entries
	p.RevocationDigests = sortedUnique(p.RevocationDigests)
	return p, nil
}

func normalizeTicketPayload(p TicketPayload) (TicketPayload, error) {
	return normalizeTicketPayloadMode(p, false)
}

// normalizeTicketDraft validates a caller-supplied ticket before the writer
// assigns its chain position: every field is checked except sequence and
// predecessor, which the writer computes from the immutable chain and then
// re-validates with normalizeTicketPayload. This keeps the "validate all content
// before any filesystem interaction" ordering even though the position is
// writer-owned (spec 6.2, 6.7).
func normalizeTicketDraft(p TicketPayload) (TicketPayload, error) {
	return normalizeTicketPayloadMode(p, true)
}

func normalizeTicketPayloadMode(p TicketPayload, draft bool) (TicketPayload, error) {
	if !validDigest(p.TicketDigest) {
		return p, fmt.Errorf("%w: ticket identity", ErrEvidenceEncoding)
	}
	if !draft {
		if p.Sequence == 0 {
			return p, fmt.Errorf("%w: ticket identity", ErrEvidenceEncoding)
		}
		if p.Sequence == 1 {
			if p.PreviousDigest != "" {
				return p, fmt.Errorf("%w: ticket genesis predecessor", ErrEvidenceEncoding)
			}
		} else if !validDigest(p.PreviousDigest) {
			return p, fmt.Errorf("%w: ticket predecessor", ErrEvidenceEncoding)
		}
	}
	if !validDigest(p.IntentDigest) {
		return p, fmt.Errorf("%w: ticket intent digest", ErrEvidenceEncoding)
	}
	if err := validateDigestList("ticket fact digests", p.FactDigests); err != nil {
		return p, err
	}
	if strings.TrimSpace(p.OperationID) == "" || strings.TrimSpace(p.WriterEpoch) == "" {
		return p, fmt.Errorf("%w: ticket operation/writer", ErrEvidenceEncoding)
	}
	switch p.Action {
	case "start", "remove":
	default:
		return p, fmt.Errorf("%w: ticket action %q", ErrEvidenceEncoding, p.Action)
	}
	switch p.Phase {
	case "prepared", "dispatch-intent", "receipt", "terminal-not-dispatched", "terminal":
	default:
		return p, fmt.Errorf("%w: ticket phase %q", ErrEvidenceEncoding, p.Phase)
	}
	p.FactDigests = sortedUnique(p.FactDigests)
	return p, nil
}

func normalizeRevocationPayload(p RevocationPayload) (RevocationPayload, error) {
	if p.EffectiveAt.IsZero() || strings.TrimSpace(p.Reason) == "" {
		return p, fmt.Errorf("%w: revocation", ErrEvidenceEncoding)
	}
	if err := validateDigestList("revocation record digests", p.RecordDigests); err != nil {
		return p, err
	}
	if err := validateStringList("revocation reference ids", p.ReferenceIDs); err != nil {
		return p, err
	}
	if err := validateDigestList("revocation support digests", p.SupportDigests); err != nil {
		return p, err
	}
	p.EffectiveAt = p.EffectiveAt.UTC()
	p.RecordDigests = sortedUnique(p.RecordDigests)
	p.ReferenceIDs = sortedUnique(p.ReferenceIDs)
	p.SupportDigests = sortedUnique(p.SupportDigests)
	return p, nil
}

func sortedUnique(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	n := 0
	for i, s := range out {
		if i > 0 && s == out[i-1] {
			continue
		}
		out[n] = s
		n++
	}
	return out[:n]
}

// BootstrapApprovalDigest computes the cycle-free anchor digest from the root
// qualification fields (spec 6.2); MethodID is fixed.
func BootstrapApprovalDigest(c BootstrapApprovalContent) (string, error) {
	if c.MethodID != "v4-root-provisioning" {
		return "", fmt.Errorf("%w: bootstrap method", ErrEvidenceEncoding)
	}
	data, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
