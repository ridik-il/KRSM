package webhook

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ridik-il/krsm/closure"
	"github.com/ridik-il/krsm/internal/contract"
	"github.com/ridik-il/krsm/scope"
)

// targetAnnotation is the Level-1 scope channel (ADR-0011): the pre-existing object
// names the ref the ownership derivation should be RE-ROOTED at, as
// "<Kind>[.<group>]/<namespace>/<name>". It is deliberately distinct from the agent
// IDENTITY key (krsm.io/task, the matcher's concern) so identity and scope can never
// collide on one key.
const targetAnnotation = "krsm.io/target"

// scopeAnnotation is the Level-3 scope channel (ADR-0003/ADR-0011): the pre-existing
// object names a TaskContract CR as "<namespace>/<name>" whose compiled allow-clauses
// ARE the request's scope. It outranks krsm.io/target — a declared contract is the most
// specific statement of intent available, and the two are never merged.
const scopeAnnotation = "krsm.io/scope"

// ErrContractNotFound is what a contractGetter reports when the named TaskContract does
// not exist — including when the CRD itself is absent. It is not a distinguished
// outcome for the caller (every resolution failure is equally fail-closed); it exists so
// an implementation can report "absent" without inventing an error string.
var ErrContractNotFound = errors.New("taskcontract not found")

// contractGetter resolves a TaskContract CR by an AUTHORITATIVE LIVE read — deliberately
// not an informer cache. A cached contract that has been deleted or tightened would keep
// authorising after its author revoked it: a false ALLOW, the one error class this gate
// must not have. One bounded synchronous read on the rare L3 path buys a zero-length
// revocation window, no L3 sync gate, and a CRD installed after startup with no restart.
//
// The deadline is the request's: Get MUST honour ctx, because a contract read that
// outlives the admission deadline has to fail closed rather than answer late.
type contractGetter interface {
	Get(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error)
}

// resolveScope selects the scope predicate for one request, highest declared level
// first: L3 (krsm.io/scope contract) over L1 (krsm.io/target re-root) over L0 (derived
// from the action target). The levels are a strict PRIORITY, never a union: two declared
// scopes are two different statements of intent, and merging them would silently grant
// the union of both — wider than either author asked for. A request carrying both
// annotations is therefore governed by the contract alone.
//
// Governing annotations are read from oldU — the request's oldObject — and NEVER from
// the incoming object: an UPDATE that adds krsm.io/target in the same request must not
// authorize that request. CREATE and payload-less sub-resources therefore have no
// governing annotation and fall to L0.
//
// On error the returned reasonCode is the fail-closed taxonomy code the caller denies
// with: reasonInvalid for malformed syntax, reasonScopeUnresolved for a well-formed
// reference that cannot be resolved. A referenced-but-unresolvable scope is never
// silently downgraded to L0 — that would hide the intent the agent declared.
func (s *Server) resolveScope(ctx context.Context, _ *admissionv1.AdmissionRequest, oldU *unstructured.Unstructured, target closure.Ref) (scope.ScopePredicate, reasonCode, error) {
	if v, ok := governingAnnotation(oldU, scopeAnnotation); ok {
		return s.resolveContract(ctx, v, target)
	}
	if v, ok := governingAnnotation(oldU, targetAnnotation); ok {
		root, rc, err := s.parseTargetRef(v)
		if err != nil {
			return scope.ScopePredicate{}, rc, err
		}
		pred := scope.Derive(root)
		pred.Provenance = scope.ProvenanceAnnotation
		return pred, "", nil
	}
	return scope.Derive(target), "", nil
}

// resolveContract resolves the Level-3 scope: the TaskContract CR that v names is read
// LIVE (bounded by the request deadline), parsed strictly and compiled. It is fail-closed
// in the strongest sense the taxonomy has — EVERY way this can go wrong, from a disabled
// seam to an RBAC denial to an uncompilable contract, is one scope-unresolved deny in
// BOTH modes. Audit softens a COMPUTED escape; it must never soften an authorisation
// claim that could not be verified, and falling back to the derived tree would grant a
// scope whose author asked for a different one.
//
// The reference is a BEARER capability for v0.5 (design §Capability model), bounded by
// two rules enforced here: it is read from oldObject only (the caller's job), and it must
// name a contract in the request target's OWN namespace — so a compromised agent cannot
// reach for another namespace's more permissive contract. Identity/UID/expiry binding is
// the deferred follow-up (issue #42), not a gap this function silently ignores.
func (s *Server) resolveContract(ctx context.Context, v string, target closure.Ref) (scope.ScopePredicate, reasonCode, error) {
	ns, name, rc, err := parseContractRef(v, target)
	if err != nil {
		return scope.ScopePredicate{}, rc, err
	}
	if s.contracts == nil {
		// L3 is not wired. The request DECLARED a contract scope, so it cannot be
		// served — but note this branch is reached only by an L3 request: L0/L1 never
		// touch the getter, so an unconfigured (or broken) L3 never disables them.
		return scope.ScopePredicate{}, reasonScopeUnresolved, fmt.Errorf("%s %q: contract resolution is not configured on this server", scopeAnnotation, v)
	}
	u, err := s.contracts.Get(ctx, ns, name)
	if err != nil {
		// Not-found, RBAC-forbidden, deadline, transport failure — one class. They are
		// deliberately NOT distinguished in the verdict: an agent must not be able to
		// probe which contracts exist or which its reader may read.
		return scope.ScopePredicate{}, reasonScopeUnresolved, fmt.Errorf("%s %q: %w", scopeAnnotation, v, err)
	}
	raw, err := u.MarshalJSON()
	if err != nil {
		return scope.ScopePredicate{}, reasonScopeUnresolved, fmt.Errorf("%s %q: re-encoding the contract failed: %w", scopeAnnotation, v, err)
	}
	// JSON is a subset of YAML 1.2, so contract.Parse — the SINGLE strict parse path,
	// shared with the offline scenario loader — reads the CR the API server returned
	// unchanged. Strictness matters most here: the CR is attacker-influenced, and
	// scope.Compile cannot reject a field a lax parser already discarded.
	tc, err := contract.Parse(raw)
	if err != nil {
		return scope.ScopePredicate{}, reasonScopeUnresolved, fmt.Errorf("%s %q: %w", scopeAnnotation, v, err)
	}
	pred, err := scope.Compile(tc)
	if err != nil {
		return scope.ScopePredicate{}, reasonScopeUnresolved, fmt.Errorf("%s %q: %w", scopeAnnotation, v, err)
	}
	if rc, err := s.resolveOwnershipRoots(pred.Clauses, v); err != nil {
		return scope.ScopePredicate{}, rc, err
	}
	return pred, "", nil
}

// resolveOwnershipRoots pins every compiled ownership clause's Root to the REAL uid of
// the live object it names, resolved group-aware, and fails closed on a miss — the same
// rule parseTargetRef applies to an L1 re-root, for the same reason.
//
// It is not optional book-keeping. contract.Parse stamps roots with contract.SyntheticUID,
// an OFFLINE convention: no object read from a cluster carries it, so the engine's lookup
// misses on the uid and falls through to the group-BLIND Kind/ns/name bucket, where two
// tracked kinds sharing a Kind across API groups collide. ownedSubtree then normalises the
// walk's start to whatever that bucket holds — so an ownership root naming an ABSENT
// Widget.a.example.com/prod/w would be authorised by a present Widget.b.example.com/prod/w's
// entire subtree. Stamping the real uid removes the fallback; refusing on a miss removes
// the remaining path to it.
func (s *Server) resolveOwnershipRoots(clauses []closure.ScopeClause, v string) (reasonCode, error) {
	for i := range clauses {
		if clauses[i].Dim != closure.DimOwnership {
			continue
		}
		root := clauses[i].Root
		obj, ok := s.info.GetByGVK(root.GVK, root.Namespace, root.Name)
		if !ok {
			return reasonScopeUnresolved, fmt.Errorf("%s %q: ownership root %s.%s/%s/%s names no tracked object",
				scopeAnnotation, v, root.GVK.Kind, root.GVK.Group, root.Namespace, root.Name)
		}
		clauses[i].Root.UID = obj.Ref.UID
	}
	return "", nil
}

// parseContractRef parses a krsm.io/scope value into the namespace/name of the
// TaskContract to read, strictly: exactly two non-empty, whitespace-free segments.
// Malformed SHAPE is reasonInvalid; a well-formed reference the capability model
// forbids is reasonScopeUnresolved.
//
// The same-namespace rule is the whole bound on a bearer capability in v0.5: the
// contract must live in the request target's own namespace, which is the namespace RBAC
// already lets this agent act in. A cluster-scoped request has no namespace to be bound
// BY, so it cannot use L3 at all in this slice — refused rather than defaulted, because
// defaulting would pick a namespace nobody named.
func parseContractRef(v string, target closure.Ref) (namespace, name string, rc reasonCode, err error) {
	parts := strings.Split(v, "/")
	if len(parts) != 2 {
		return "", "", reasonInvalid, fmt.Errorf("%s %q: want <namespace>/<name>", scopeAnnotation, v)
	}
	namespace, name = parts[0], parts[1]
	if namespace == "" || name == "" {
		return "", "", reasonInvalid, fmt.Errorf("%s %q: namespace and name must be non-empty", scopeAnnotation, v)
	}
	if hasSpace(namespace) || hasSpace(name) {
		return "", "", reasonInvalid, fmt.Errorf("%s %q: segments must not contain whitespace", scopeAnnotation, v)
	}
	if target.Namespace == "" {
		return "", "", reasonScopeUnresolved, fmt.Errorf("%s %q: a cluster-scoped request cannot reference a namespaced contract", scopeAnnotation, v)
	}
	if namespace != target.Namespace {
		return "", "", reasonScopeUnresolved, fmt.Errorf("%s %q: cross-namespace reference (the request acts in %q)", scopeAnnotation, v, target.Namespace)
	}
	return namespace, name, "", nil
}

// governingAnnotation reads one scope-governing annotation from the request's
// oldObject. A nil payload (CREATE, a sub-resource with no oldObject) carries none.
func governingAnnotation(oldU *unstructured.Unstructured, key string) (string, bool) {
	if oldU == nil {
		return "", false
	}
	v, ok := oldU.GetAnnotations()[key]
	return v, ok
}

// parseTargetRef parses a krsm.io/target value into the ref to re-root at, strictly:
// exactly three "/"-separated segments "<Kind>[.<group>]/<namespace>/<name>", the kind
// token and name non-empty, no segment carrying whitespace. Malformed syntax is
// reasonInvalid; a well-formed token naming no unique tracked kind is
// reasonScopeUnresolved.
//
// The ref carries the REAL uid of the object the token names, resolved group-aware via
// clusterInfo.GetByGVK. Without it the engine would fall back to the group-BLIND
// Kind/ns/name key, where two tracked kinds sharing a Kind in different API groups
// collide in one bucket and the last writer wins — so a re-root could walk a DIFFERENT
// object's subtree and authorise a blast radius the author never named (design rev 3,
// "Group-blind human key").
//
// A GetByGVK MISS is therefore reasonScopeUnresolved, never a uid-less (or synthesized-
// uid) ref. An earlier revision claimed a uid-less ref was safe because "the walk starts
// from a nonexistent root and authorises only the root itself"; that is false in exactly
// the collision case this resolution exists for. closure's lookup tries the uid and then
// falls through to byHuman[Kind/ns/name], and ownedSubtree normalises the walk's start to
// whatever that bucket holds — so an ABSENT Widget.a.example.com/prod/w authorises the
// whole subtree of a present Widget.b.example.com/prod/w (demonstrated in step 2b:
// allowed=true). Synthesizing a uid does not help, because the fallback fires whenever
// the uid misses. Refusing to resolve is the only construction that fails closed.
func (s *Server) parseTargetRef(v string) (closure.Ref, reasonCode, error) {
	parts := strings.Split(v, "/")
	if len(parts) != 3 {
		return closure.Ref{}, reasonInvalid, fmt.Errorf("%s %q: want <Kind>[.<group>]/<namespace>/<name>", targetAnnotation, v)
	}
	kindTok, ns, name := parts[0], parts[1], parts[2]
	if kindTok == "" || name == "" {
		return closure.Ref{}, reasonInvalid, fmt.Errorf("%s %q: kind and name must be non-empty", targetAnnotation, v)
	}
	if hasSpace(kindTok) || hasSpace(ns) || hasSpace(name) {
		return closure.Ref{}, reasonInvalid, fmt.Errorf("%s %q: segments must not contain whitespace", targetAnnotation, v)
	}

	gvk, err := s.resolveKindToken(kindTok)
	if err != nil {
		return closure.Ref{}, reasonScopeUnresolved, fmt.Errorf("%s %q: %w", targetAnnotation, v, err)
	}

	namespaced, ok := s.info.Namespaced(gvk)
	if !ok {
		return closure.Ref{}, reasonScopeUnresolved, fmt.Errorf("%s %q: scope of kind %s/%s is unknown", targetAnnotation, v, gvk.Group, gvk.Kind)
	}
	if namespaced && ns == "" {
		return closure.Ref{}, reasonInvalid, fmt.Errorf("%s %q: kind %s is namespaced and needs a namespace", targetAnnotation, v, gvk.Kind)
	}
	if !namespaced && ns != "" {
		return closure.Ref{}, reasonInvalid, fmt.Errorf("%s %q: kind %s is cluster-scoped and takes no namespace", targetAnnotation, v, gvk.Kind)
	}
	obj, ok := s.info.GetByGVK(gvk, ns, name)
	if !ok {
		return closure.Ref{}, reasonScopeUnresolved, fmt.Errorf("%s %q: names no tracked object", targetAnnotation, v)
	}
	return closure.Ref{GVK: gvk, Namespace: ns, Name: name, UID: obj.Ref.UID}, "", nil
}

// resolveKindToken maps a "<Kind>[.<group>]" token to one TRACKED GVK — the grammar the
// CLI and --allow-cluster-scoped-kind already use. A trailing-dot token ("Pod.") names
// the core group explicitly; an unqualified token means "any group, and it must be
// unique". Ambiguity is an error, never an arbitrary pick: silently choosing one of two
// groups' Kinds would authorize a tree the author did not name.
func (s *Server) resolveKindToken(tok string) (closure.GVK, error) {
	kind, group := tok, ""
	qualified := false
	if i := strings.Index(tok, "."); i >= 0 {
		kind, group, qualified = tok[:i], tok[i+1:], true
		if kind == "" {
			return closure.GVK{}, fmt.Errorf("kind token %q has an empty kind", tok)
		}
	}

	candidates := s.info.GVKsForKind(kind)
	if qualified {
		var matches []closure.GVK
		for _, c := range candidates {
			if c.Group == group {
				matches = append(matches, c)
			}
		}
		candidates = matches
	}
	switch {
	case len(candidates) == 0:
		return closure.GVK{}, fmt.Errorf("kind %q matches no tracked kind", tok)
	case len(candidates) > 1:
		return closure.GVK{}, fmt.Errorf("kind %q is ambiguous across tracked kinds %s — qualify it as <Kind>.<group>", tok, joinGVKs(candidates))
	}
	return candidates[0], nil
}

// joinGVKs renders candidate kinds as "Kind.group" (core group as "Kind.") for an
// ambiguity error. Refs only — never object contents.
func joinGVKs(gvks []closure.GVK) string {
	out := make([]string, 0, len(gvks))
	for _, g := range gvks {
		out = append(out, g.Kind+"."+g.Group)
	}
	return strings.Join(out, ", ")
}

func hasSpace(s string) bool {
	return strings.ContainsFunc(s, unicode.IsSpace)
}

// Allowlist is the cross-boundary escape hatch for DERIVED scopes (L0/L1): the
// operator names the shared boundaries an agent's collateral is allowed to reach, so a
// cluster whose workloads legitimately touch a shared namespace does not have to run
// KRSM permanently in audit. It is a post-Safe FILTER on the computed escape set, not a
// scope clause — a clause would widen scope(T) and re-admit unrelated collateral too.
type Allowlist struct {
	// Namespaces exempts escapes that live in one of these namespaces.
	Namespaces map[string]bool
	// ClusterScopedGroupKinds exempts cluster-scoped escapes by {Group, Kind}, with
	// the Version zeroed (an exemption is about a kind, not about which version of it
	// a request happens to name). It is keyed by GroupKind rather than Kind because a
	// bare Kind collides across CRD groups, and exempting "the other group's Node" is
	// exactly the identity confusion a safety gate must not have.
	ClusterScopedGroupKinds map[closure.GVK]bool
}

// filterEscapes drops the exempt members of dec.Escaping and recomputes the verdict
// over what is left. target is the admission request's own target and is passed so the
// filter can never exempt it (see exempts).
//
// It RETURNS the refs it exempted rather than only the filtered decision, because the
// filtered decision no longer contains them: the residual escape set deliberately reports
// only what an operator still has to act on, so a caller wanting to record what the
// allowlist softened would otherwise have to recompute the diff — a second, drifting copy
// of the exemption rule. A nil list means nothing was exempted, which is distinct from an
// empty one only in that callers must not report an exemption that did not happen.
//
// A decision with an EMPTY escape set is returned untouched: that is closure.Safe's
// fail-closed Block (the target could not be resolved, so the blast radius is unknown,
// not computed). Softening it would trade an unbounded blast radius for an Allow.
func (a Allowlist) filterEscapes(dec closure.Decision, target closure.Ref) (closure.Decision, []closure.Ref) {
	if len(dec.Escaping) == 0 {
		return dec, nil
	}
	residual := make([]closure.Ref, 0, len(dec.Escaping))
	var exempted []closure.Ref
	for _, r := range dec.Escaping {
		if a.exempts(r, target) {
			exempted = append(exempted, r)
			continue
		}
		residual = append(residual, r)
	}
	if len(exempted) == 0 {
		return dec, nil // nothing exempted — keep the decision (and its slice) as Safe built it
	}
	dec.Escaping = residual
	if len(residual) == 0 {
		// Every computed escape was exempted, so re-derive the verdict exactly as
		// closure.Safe does with no escapes: a cross-boundary external effect is still
		// a Warn, otherwise Allow. Safe's reason text is restated here because closure
		// is a frozen, stdlib-only package that exports no such constant — the wording
		// must match so both paths read identically in an audit log.
		dec.Verdict, dec.Reason = closure.Allow, ""
		if len(dec.External) > 0 {
			dec.Verdict = closure.Warn
			dec.Reason = "closure crosses the cluster boundary (external effect)"
		}
	}
	return dec, exempted
}

// exempts reports whether one escaping ref may be dropped from the escape set. The
// FIRST condition is that it is not the action target: the allowlist softens collateral
// the agent reached indirectly, never the resource the agent asked to act on. Then a
// NAMESPACED ref must live in an allowlisted namespace; a CLUSTER-SCOPED one
// (Namespace == "") must carry an allowlisted {Group, Kind}. The two lists do not
// cross over: a namespaced resource is never exempted by its GroupKind, so listing a
// kind cannot silently exempt every instance of it in every namespace.
func (a Allowlist) exempts(r, target closure.Ref) bool {
	if isTarget(r, target) {
		return false
	}
	if r.Namespace != "" {
		return a.Namespaces[r.Namespace]
	}
	return a.ClusterScopedGroupKinds[closure.GVK{Group: r.GVK.Group, Kind: r.GVK.Kind}]
}

// isTarget reports whether r denotes the action target, CONSERVATIVELY: matching uids
// prove identity, and so does a matching Kind/namespace/name even when only one side
// carries a uid (a request whose oldObject was dropped has no uid to compare, while a
// closure member read from live state always has one). Over-identifying only costs a
// refused exemption; under-identifying would exempt the target itself, which is the
// authorization bypass this guard exists to prevent — so the check errs toward "yes".
func isTarget(r, target closure.Ref) bool {
	if r.UID != "" && target.UID != "" && r.UID == target.UID {
		return true
	}
	return r.GVK.Kind == target.GVK.Kind && r.Namespace == target.Namespace && r.Name == target.Name
}

// allowlistApplies reports whether a decision reached under this provenance may be
// softened by the cross-boundary allowlist. Only the DERIVED provenances qualify: L0's
// synthesized ownership tree and L1's re-rooted one are approximations of an intent
// nobody wrote down, so the operator's allowlist supplies the missing "these shared
// boundaries are expected". A contract scope was declared explicitly, and widening it
// from a server flag would silently overrule its author.
func allowlistApplies(prov scope.Provenance) bool {
	return prov == scope.ProvenanceDerivedOwner || prov == scope.ProvenanceAnnotation
}
