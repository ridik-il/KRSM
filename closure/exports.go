package closure

// This file exposes a minimal, additive surface that an out-of-tree State
// implementation (the informer-backed state/ package, v0.5) needs to reuse the
// engine's matching semantics VERBATIM rather than re-derive them. Re-deriving
// selector binding or cross-ref matching in a second package would risk the two
// State implementations diverging — exactly what the corpus parity oracle exists
// to prevent. These are thin delegators to the unexported logic; behaviour is
// identical and scanState keeps using the internal forms.
//
// Nothing here changes Safe's signature, the closure walk, the scope dimensions,
// Ref.human, or any golden — it only widens visibility (ADR-0002/0005, DESIGN §7).

// SelectorBinds reports whether a selector owned by ownerKind binds an object with
// the given labels, with the kind-aware empty-selector rule (an empty Service
// selector binds nothing; an empty NetworkPolicy/PDB/workload selector binds every
// pod in the namespace; a nil selector binds nothing). It is the exported form of
// the rule scanState applies, for the indexed State to reuse.
func SelectorBinds(ownerKind string, selector LabelSelector, labels map[string]string) bool {
	return selectorBinds(ownerKind, selector, labels)
}

// CrossRefMatches compares a cross-reference target against a resource, by uid when
// both carry one, else by Kind/namespace/name. It is the exported form of the match
// the closure walk uses for the cross-resource relation, for the indexed State to
// reuse (and to key its reverse cross-ref index consistently).
func CrossRefMatches(refd, target Ref) bool {
	return crossRefMatches(refd, target)
}

// IsSelectorKind reports whether kind is a selector-owner kind (Service,
// PodDisruptionBudget, NetworkPolicy) — the kinds whose spec.selector binds pods.
// The indexed State uses it to decide which objects populate its selector index.
func IsSelectorKind(kind string) bool {
	return selectorKinds[kind]
}
