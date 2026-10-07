package evidence

import (
	"fmt"
	"time"
)

// rootBootstrapMethod is the only method the root trust anchor may claim; the
// bootstrap approval digest is recomputed from the root's own typed MethodID so
// a stored field cannot be silently replaced by a constant (spec 6.2, 6.7).
const rootBootstrapMethod = "v4-root-provisioning"

// authorityView resolves the catalog's listed records once so issuer-grant and
// reference-closure checks never mix records read from different pinned roots.
type authorityView struct {
	reader  *Reader
	root    *pinnedRoot
	cat     CatalogRecord
	entries map[string]Record // catalog-listed record digest -> record
	cache   map[string]Record
}

func (r *Reader) newAuthorityView(root *pinnedRoot, cat CatalogRecord) (*authorityView, error) {
	v := &authorityView{reader: r, root: root, cat: cat, entries: map[string]Record{}, cache: map[string]Record{}}
	for _, e := range cat.Payload.Entries {
		rec, err := v.resolve(e.RecordDigest)
		if err != nil {
			return nil, fmt.Errorf("%w: catalog entry %s: %v", ErrEvidenceChain, e.RecordDigest, err)
		}
		if rec.Kind != e.Kind {
			return nil, fmt.Errorf("%w: catalog entry %s kind mismatch", ErrEvidenceChain, e.RecordDigest)
		}
		v.entries[e.RecordDigest] = rec
	}
	if _, err := v.resolve(cat.Payload.RootQualificationDigest); err != nil {
		return nil, fmt.Errorf("%w: root qualification %s: %v", ErrEvidenceChain, cat.Payload.RootQualificationDigest, err)
	}
	return v, nil
}

func (v *authorityView) resolve(digest string) (Record, error) {
	if rec, ok := v.cache[digest]; ok {
		return rec, nil
	}
	rec, err := v.reader.objectWithRoot(v.root, digest)
	if err != nil {
		return Record{}, err
	}
	v.cache[digest] = rec
	return rec, nil
}

// authorizeCatalogEntries verifies, before any caller may treat the catalog as
// authority, that the genesis root is a valid anchored root, that every listed
// record is host-bound, that every listed record's issuer is a catalog-listed
// qualification which grants that record's kind, and that every issuer chain
// terminates at the root without a cycle.
func (r *Reader) authorizeCatalogEntries(root *pinnedRoot, cat CatalogRecord) error {
	if len(cat.Payload.Entries) == 0 {
		return nil
	}
	v, err := r.newAuthorityView(root, cat)
	if err != nil {
		return err
	}
	rootRec, err := v.resolve(cat.Payload.RootQualificationDigest)
	if err != nil {
		return err
	}
	if _, err := validateRootQualificationAt(cat, cat.Payload.RootQualificationDigest, rootRec, time.Time{}); err != nil {
		return err
	}
	for digest, rec := range v.entries {
		if digest == cat.Payload.RootQualificationDigest {
			continue
		}
		if err := v.authorizeEntry(digest, rec); err != nil {
			return err
		}
	}
	return nil
}

func (v *authorityView) authorizeEntry(digest string, rec Record) error {
	if rec.HostID != v.cat.Header.HostID {
		return fmt.Errorf("%w: catalog entry %s host mismatch", ErrEvidenceChain, digest)
	}
	if rec.IssuerQualificationRef == "" {
		return fmt.Errorf("%w: catalog entry %s missing issuer", ErrEvidenceChain, digest)
	}
	issuerRec, err := v.resolve(rec.IssuerQualificationRef)
	if err != nil {
		return fmt.Errorf("%w: catalog entry issuer %s: %v", ErrEvidenceChain, rec.IssuerQualificationRef, err)
	}
	if issuerRec.Kind != KindIssuerQualification {
		return fmt.Errorf("%w: catalog entry issuer is not a qualification", ErrEvidenceChain)
	}
	var issuerQ IssuerQualificationRecord
	if err := strictJSON(issuerRec.Payload, &issuerQ); err != nil {
		return fmt.Errorf("%w: catalog entry issuer payload: %v", ErrEvidenceEncoding, err)
	}
	if !containsString(issuerQ.AllowedKinds, rec.Kind) {
		return fmt.Errorf("%w: issuer does not grant kind %q", ErrEvidenceChain, rec.Kind)
	}
	if err := v.checkAnchored(rec.IssuerQualificationRef); err != nil {
		return err
	}
	return nil
}

// checkAnchored walks issuer refs from digest to the genesis root, rejecting a
// missing link, a non-qualification link or a cycle.
func (v *authorityView) checkAnchored(digest string) error {
	seen := map[string]bool{}
	cur := digest
	for i := 0; i < 64; i++ {
		if cur == v.cat.Payload.RootQualificationDigest {
			return nil
		}
		if seen[cur] {
			return fmt.Errorf("%w: issuer chain cycle", ErrEvidenceChain)
		}
		seen[cur] = true
		rec, err := v.resolve(cur)
		if err != nil {
			return err
		}
		if rec.Kind != KindIssuerQualification {
			return fmt.Errorf("%w: issuer chain not terminated at root", ErrEvidenceChain)
		}
		next := rec.IssuerQualificationRef
		if next == "" {
			return fmt.Errorf("%w: issuer chain not terminated at root", ErrEvidenceChain)
		}
		cur = next
	}
	return fmt.Errorf("%w: issuer chain too deep", ErrEvidenceChain)
}

// validateRootQualificationAt validates the root trust anchor: it is the
// catalog genesis root, empty-issuer, host-granting, method-anchored and its
// bootstrap approval digest is recomputed from its own typed fields and equals
// the genesis catalog digest. When at is non-zero the root must be currently
// valid (issuing <= at < expiry).
func validateRootQualificationAt(cat CatalogRecord, digest string, rec Record, at time.Time) (IssuerQualificationRecord, error) {
	if digest != cat.Payload.RootQualificationDigest {
		return IssuerQualificationRecord{}, fmt.Errorf("%w: root digest", ErrEvidenceChain)
	}
	if rec.Kind != KindIssuerQualification {
		return IssuerQualificationRecord{}, fmt.Errorf("%w: root kind", ErrEvidenceChain)
	}
	if rec.IssuerQualificationRef != "" {
		return IssuerQualificationRecord{}, fmt.Errorf("%w: root issuer ref must be empty", ErrEvidenceChain)
	}
	if rec.HostID != cat.Header.HostID {
		return IssuerQualificationRecord{}, fmt.Errorf("%w: root host", ErrEvidenceChain)
	}
	var q IssuerQualificationRecord
	if err := strictJSON(rec.Payload, &q); err != nil {
		return IssuerQualificationRecord{}, fmt.Errorf("%w: root payload: %v", ErrEvidenceEncoding, err)
	}
	if q.Role != "root-approver" {
		return IssuerQualificationRecord{}, fmt.Errorf("%w: root role", ErrEvidenceChain)
	}
	if !containsString(q.HostIDs, cat.Header.HostID) {
		return IssuerQualificationRecord{}, fmt.Errorf("%w: root host grant", ErrEvidenceChain)
	}
	if q.MethodID != rootBootstrapMethod {
		return IssuerQualificationRecord{}, fmt.Errorf("%w: root method", ErrEvidenceChain)
	}
	if !containsString(q.AllowedKinds, KindIssuerQualification) {
		return IssuerQualificationRecord{}, fmt.Errorf("%w: root does not grant qualification issuance", ErrEvidenceChain)
	}
	if !at.IsZero() && (at.Before(rec.IssuedAt) || !at.Before(rec.ExpiresAt)) {
		return IssuerQualificationRecord{}, fmt.Errorf("%w: root not valid at time", ErrEvidenceChain)
	}
	expected, err := BootstrapApprovalDigest(BootstrapApprovalContent{
		HostID: rec.HostID, IssuerID: q.IssuerID, ApprovalID: q.ApprovalID, MethodID: q.MethodID,
		ExecutableDigest: q.ExecutableDigest, ServiceDefinitionDigest: q.ServiceDefinitionDigest,
		ProvisioningRevision: q.ProvisioningRevision,
	})
	if err != nil || expected != q.ApprovalDigest || expected != cat.Payload.BootstrapApprovalDigest {
		return IssuerQualificationRecord{}, fmt.Errorf("%w: root bootstrap approval not anchored", ErrEvidenceChain)
	}
	return q, nil
}

// effectiveRevocationsAt returns the set of digests/ids targeted by
// catalog-listed revocations that are effective at at. It fails closed when a
// listed revocation is missing, malformed or depends on a missing object.
// SupportDigests are dependencies only and are never treated as targets.
func (r *Reader) effectiveRevocationsAt(root *pinnedRoot, cat CatalogRecord, at time.Time) (map[string]bool, error) {
	resolve := func(digest string) (Record, error) {
		if root != nil {
			return r.objectWithRoot(root, digest)
		}
		return r.Object(digest)
	}
	targets := map[string]bool{}
	for _, d := range cat.Payload.RevocationDigests {
		rec, err := resolve(d)
		if err != nil {
			return nil, fmt.Errorf("%w: catalog revocation %s unavailable: %v", ErrEvidenceChain, d, err)
		}
		if rec.Kind != KindRevocation {
			return nil, fmt.Errorf("%w: catalog revocation %s kind", ErrEvidenceChain, d)
		}
		var p RevocationPayload
		if err := strictJSON(rec.Payload, &p); err != nil {
			return nil, fmt.Errorf("%w: catalog revocation %s payload: %v", ErrEvidenceEncoding, d, err)
		}
		if _, err := normalizeRevocationPayload(p); err != nil {
			return nil, err
		}
		for _, s := range p.SupportDigests {
			if _, err := resolve(s); err != nil {
				return nil, fmt.Errorf("%w: revocation dependency %s unavailable: %v", ErrEvidenceChain, s, err)
			}
		}
		if !p.EffectiveAt.After(at) {
			for _, t := range p.RecordDigests {
				targets[t] = true
			}
			for _, t := range p.ReferenceIDs {
				targets[t] = true
			}
		}
	}
	return targets, nil
}

// authorityRefDigests returns the leaf-concrete object references that must
// resolve to catalog authority: the envelope's ReferencedDigests plus, for a
// typed issuer-qualification, its SupportDigests. Opaque docker/github payloads
// are validated by their owning package.
func authorityRefDigests(rec Record) ([]string, error) {
	out := make([]string, 0, len(rec.ReferencedDigests)+4)
	out = append(out, rec.ReferencedDigests...)
	if rec.Kind == KindIssuerQualification {
		var p IssuerQualificationRecord
		if err := strictJSON(rec.Payload, &p); err != nil {
			return nil, fmt.Errorf("%w: issuer qualification payload: %v", ErrEvidenceEncoding, err)
		}
		out = append(out, p.SupportDigests...)
	}
	return sortedUnique(out), nil
}

// validateRefClosureAt verifies that every supplied ref is a current catalog
// mapping, that its leaf-concrete closure resolves, that every issuer-qualification
// encountered is currently valid at at, that the root is valid at at, and that
// no effective revocation targets a ref or the root.
func (r *Reader) validateRefClosureAt(root *pinnedRoot, cat CatalogRecord, refs []string, at time.Time) error {
	v, err := r.newAuthorityView(root, cat)
	if err != nil {
		return err
	}
	rootRec, err := v.resolve(cat.Payload.RootQualificationDigest)
	if err != nil {
		return err
	}
	if _, err := validateRootQualificationAt(cat, cat.Payload.RootQualificationDigest, rootRec, at); err != nil {
		return err
	}
	seen := map[string]bool{}
	var walk func(digest string) error
	walk = func(digest string) error {
		if seen[digest] {
			return nil
		}
		seen[digest] = true
		rec, err := v.resolve(digest)
		if err != nil {
			return fmt.Errorf("%w: ref %s: %v", ErrEvidenceChain, digest, err)
		}
		if digest != cat.Payload.RootQualificationDigest && rec.Kind == KindIssuerQualification {
			if at.Before(rec.IssuedAt) || !at.Before(rec.ExpiresAt) {
				return fmt.Errorf("%w: issuer %s not valid at time", ErrEvidenceChain, digest)
			}
		}
		inner, err := authorityRefDigests(rec)
		if err != nil {
			return err
		}
		for _, ref := range inner {
			if err := walk(ref); err != nil {
				return err
			}
		}
		return nil
	}
	for _, ref := range refs {
		if !v.isCatalogListed(ref) {
			return fmt.Errorf("%w: ref %s is not a current catalog mapping", ErrEvidenceChain, ref)
		}
		if err := walk(ref); err != nil {
			return err
		}
	}
	targets, err := r.effectiveRevocationsAt(root, cat, at)
	if err != nil {
		return err
	}
	if targets[cat.Payload.RootQualificationDigest] {
		return fmt.Errorf("%w: root qualification revoked", ErrEvidenceChain)
	}
	for _, ref := range refs {
		if targets[ref] {
			return fmt.Errorf("%w: ref %s revoked at time", ErrEvidenceChain, ref)
		}
	}
	return nil
}

func (v *authorityView) isCatalogListed(digest string) bool {
	_, ok := v.entries[digest]
	return ok
}

// checkReferenceCycle reports an error if the directed reference graph reachable
// from start contains a cycle. refs returns the outgoing edges of a node.
func checkReferenceCycle(start string, refs func(string) ([]string, error)) error {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var visit func(string) error
	visit = func(node string) error {
		color[node] = gray
		edges, err := refs(node)
		if err != nil {
			return err
		}
		for _, next := range edges {
			switch color[next] {
			case gray:
				return fmt.Errorf("%w: reference cycle at %s", ErrEvidenceChain, next)
			case white:
				if err := visit(next); err != nil {
					return err
				}
			}
		}
		color[node] = black
		return nil
	}
	return visit(start)
}
