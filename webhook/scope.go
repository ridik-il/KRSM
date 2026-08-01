package webhook

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ridik-il/krsm/closure"
	"github.com/ridik-il/krsm/scope"
)

// targetAnnotation is the Level-1 scope channel (ADR-0011): the pre-existing object
// names the ref the ownership derivation should be RE-ROOTED at, as
// "<Kind>[.<group>]/<namespace>/<name>". It is deliberately distinct from the agent
// IDENTITY key (krsm.io/task, the matcher's concern) so identity and scope can never
// collide on one key.
const targetAnnotation = "krsm.io/target"

// resolveScope selects the scope predicate for one request, highest declared level
// first: L1 (krsm.io/target re-root) over L0 (derived from the action target). L3
// (krsm.io/scope contract) drops in AHEAD of L1 without reshaping this function.
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
func (s *Server) resolveScope(_ context.Context, _ *admissionv1.AdmissionRequest, oldU *unstructured.Unstructured, target closure.Ref) (scope.ScopePredicate, reasonCode, error) {
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
// "Group-blind human key"). A uid is never SYNTHESIZED: an unresolvable root stays
// uid-less, the subtree walk then starts from a nonexistent root and covers only the
// root itself, and the verdict fails closed — the safe direction.
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
	root := closure.Ref{GVK: gvk, Namespace: ns, Name: name}
	if obj, ok := s.info.GetByGVK(gvk, ns, name); ok {
		root.UID = obj.Ref.UID
	}
	return root, "", nil
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
// A decision with an EMPTY escape set is returned untouched: that is closure.Safe's
// fail-closed Block (the target could not be resolved, so the blast radius is unknown,
// not computed). Softening it would trade an unbounded blast radius for an Allow.
func (a Allowlist) filterEscapes(dec closure.Decision, target closure.Ref) closure.Decision {
	if len(dec.Escaping) == 0 {
		return dec
	}
	residual := make([]closure.Ref, 0, len(dec.Escaping))
	for _, r := range dec.Escaping {
		if a.exempts(r, target) {
			continue
		}
		residual = append(residual, r)
	}
	if len(residual) == len(dec.Escaping) {
		return dec // nothing exempted — keep the decision (and its slice) as Safe built it
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
	return dec
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
