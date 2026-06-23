package closure

import (
	"path"
	"sort"
)

// Safe is the verdict: it computes the action's closure and decides whether it
// stays within the authorised scope. An in-cluster closure member not covered by
// any scope clause is an escape (→ Block). A cross-boundary effect with no
// escape is a Warn. Otherwise Allow. Precedence is Block > Warn > Allow.
//
// It fails closed: if the action's target cannot be resolved in the supplied
// state the closure is unknown, so the action is denied rather than admitted
// with an unbounded blast radius (DESIGN §5). This deny is reported with a
// Reason distinct from a scope escape.
func Safe(s State, a Action, scope []ScopeClause) Decision {
	// Closure seeds from the target only when it exists in state and always
	// includes it, so an empty closure means the target could not be resolved:
	// fail closed (DESIGN §5) rather than issue a second Get for the same fact.
	c := Closure(s, a)
	if len(c) == 0 {
		return Decision{
			Verdict: Block,
			Reason:  "fail-closed: action target not found in tracked state; closure cannot be computed",
		}
	}

	external := ExternalEffects(s, a)

	// res answers matchScope's state-dependent questions over the same snapshot the
	// closure was computed on: a selector clause's labels and an ownership clause's
	// owned-subtree membership both read the world the closure walked.
	res := &stateResolver{s: s, subtrees: map[string]map[string]bool{}}

	var escaping []Ref
	for _, r := range c {
		if !matchScope(r, scope, res) {
			escaping = append(escaping, r)
		}
	}
	sort.Slice(escaping, func(i, j int) bool { return escaping[i].human() < escaping[j].human() })

	verdict := Allow
	reason := ""
	switch {
	case len(escaping) > 0:
		verdict = Block
		reason = "affected-resource closure escapes task scope"
	case len(external) > 0:
		verdict = Warn
		reason = "closure crosses the cluster boundary (external effect)"
	}

	return Decision{
		Verdict:  verdict,
		Reason:   reason,
		Closure:  c,
		Escaping: escaping,
		External: external,
	}
}

// scopeResolver answers the state-dependent questions matchScope's dimensions ask,
// over the same snapshot the closure was computed on. It replaces the bare
// labels-by-ref function the selector dimension once took: selector needs labels,
// ownership needs owned-subtree membership, and a future reference dimension gets a
// method here rather than another positional parameter (the accumulating-function
// smell the v0.3 review flagged). matchScope takes this narrow interface rather than
// the whole State so scope matching stays decoupled from the full State surface and
// is trivially fakeable (DESIGN: alternatives considered).
type scopeResolver interface {
	// labels resolves a candidate's labels; (nil, false) when r is untracked.
	labels(Ref) (map[string]string, bool)
	// ownedSubtree returns the membership set of root's transitive owned subtree,
	// keyed by Ref.key() and including root itself.
	ownedSubtree(root Ref) map[string]bool
}

// stateResolver is the scopeResolver Safe builds over the State it already holds.
// labels delegates to Get; ownedSubtree BFS-walks OwnedChildren from the root —
// the identical traversal Closure cascades over — and memoizes per root.key() so an
// N-member closure costs one walk per distinct root, not one per member.
type stateResolver struct {
	s        State
	subtrees map[string]map[string]bool // root.key() → membership set
}

func (sr *stateResolver) labels(r Ref) (map[string]string, bool) {
	o, ok := sr.s.Get(r)
	if !ok {
		return nil, false
	}
	return o.Labels, true
}

// ownedSubtree walks owner→child from root following OwnedChildren with a
// visited-set guard, collecting Ref.key() for root and every transitively owned
// resource. It mirrors Closure's cascade step exactly, so "owned" means the same
// thing on both sides of C ⊆ scope(T). Finite and cycle-safe: each resource is
// visited once (|subtree| ≤ |R|). Memoized per the caller-provided root.key().
//
// The root is normalized via State.Get so a uid-less clause Root (built outside the
// loader — e.g. via the OwnershipClause constructor or scope.Derive on a uid-less
// target) still covers itself: closure members are keyed by their UID-bearing
// Ref.key() ("uid:<uid>"), so the BFS seeds from the canonical (looked-up) Ref and
// the returned membership admits BOTH the caller-provided root.key() AND the
// canonical o.Ref.key(). If Get returns !ok the root is absent from state, so today's
// fail-closed behavior is kept (seed only with the provided root.key()).
func (sr *stateResolver) ownedSubtree(root Ref) map[string]bool {
	if m, ok := sr.subtrees[root.key()]; ok {
		return m
	}
	members := map[string]bool{}
	// Normalize to the canonical Ref so OwnedChildren and the closure-side key agree
	// even when the clause Root lacks a UID. An absent root keeps the fail-closed path
	// (BFS from the provided Ref only). BFS keys members by the canonical Ref.key().
	start := root
	if o, ok := sr.s.Get(root); ok {
		start = o.Ref
	}
	queue := []Ref{start}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if members[cur.key()] {
			continue
		}
		members[cur.key()] = true
		queue = append(queue, sr.s.OwnedChildren(cur)...)
	}
	// Admit the caller-provided root key too: closure members carry their UID-bearing
	// key, while a uid-less clause Root keys itself by the human form — both must mean
	// "the root", so the root never escapes its own subtree.
	members[root.key()] = true
	sr.subtrees[root.key()] = members
	return members
}

// matchScope reports whether r is covered by any scope clause (OR across clauses).
// The gate moves INTO the dimension switch because ownership is not GVK/namespace-
// gated (an owned child may differ in kind/namespace from its root):
//
//   - DimResource (and an empty Dim): pass the GVK/namespace gate, then match the
//     resource name against the clause's name pattern (glob via path.Match) —
//     unchanged from v0.1.
//   - DimSelector: pass the gate, then match r's live labels against the selector.
//   - DimNamespace: the GVK/namespace gate IS the membership test (every resource in
//     the gated namespace is in scope); passing it means in scope.
//   - DimOwnership: pure subtree membership (res.ownedSubtree(sc.Root)[r.key()]),
//     with NO gate — the subtree is rooted at sc.Root, not at the clause's own GVK/
//     namespace, so a cross-kind/cross-namespace owned child still matches.
//
// The dimension switch is explicit and fail-closed: a clause whose Dim is none of
// the known dimensions (a typo, or a not-yet-implemented dimension) covers
// NOTHING — it is skipped, never coerced into a resource grant. dimOf maps an empty
// Dim to DimResource so dim-less v0.1/v0.2 clauses still match as resource. The
// loader (parseScope/Validate) rejects unknown dimensions at load time, so the
// default branch is a defence-in-depth guard against a clause constructed in code.
func matchScope(r Ref, scope []ScopeClause, res scopeResolver) bool {
	for _, sc := range scope {
		switch dimOf(sc) {
		case DimResource:
			if gates(sc, r) {
				if ok, _ := path.Match(sc.Name, r.Name); ok {
					return true
				}
			}
		case DimSelector:
			if gates(sc, r) && matchSelectorClause(sc, r, res.labels) {
				return true
			}
		case DimNamespace:
			// The GVK/namespace gate IS the membership test for a namespace clause
			// (every resource in the gated namespace is in scope): no name or selector
			// to match, so passing the gate means in scope.
			if gates(sc, r) {
				return true
			}
		case DimOwnership:
			// Ungated subtree membership: r is in scope iff it is the root or lies in
			// the root's transitive owned subtree (the same owner→child walk Closure
			// uses). A child may legitimately differ in kind/namespace from the root,
			// so an ownership clause is deliberately NOT GVK/namespace-gated.
			if res.ownedSubtree(sc.Root)[r.key()] {
				return true
			}
		default:
			// Unknown dimension: grant nothing (fail-closed). Fall through to the next
			// clause without matching — never coerced into a resource grant. The loader
			// rejects this at load; this is defence-in-depth.
		}
	}
	return false
}

// dimOf reads a clause's dimension, treating an empty Dim as DimResource so
// dim-less v0.1/v0.2 scopes load and match unchanged.
func dimOf(sc ScopeClause) ScopeDim {
	if sc.Dim == "" {
		return DimResource
	}
	return sc.Dim
}

// gates reports whether r passes a clause's GVK/namespace gate. Group/Version are
// optional (ignored when empty); Kind and Namespace must agree. The gate is
// identical for both dimensions, so a Pod selector clause never matches a Service.
func gates(sc ScopeClause, r Ref) bool {
	if sc.GVK.Kind != "" && sc.GVK.Kind != r.GVK.Kind {
		return false
	}
	if sc.GVK.Group != "" && sc.GVK.Group != r.GVK.Group {
		return false
	}
	if sc.GVK.Version != "" && sc.GVK.Version != r.GVK.Version {
		return false
	}
	return sc.Namespace == r.Namespace
}

// matchSelectorClause evaluates a DimSelector clause against r's live labels. It
// fails safe twice: an empty/nil selector matches nothing (never a silent
// namespace-wide over-grant), and an untracked candidate whose labels cannot be
// resolved matches nothing (fail-closed, DESIGN §5).
func matchSelectorClause(sc ScopeClause, r Ref, labels func(Ref) (map[string]string, bool)) bool {
	// An empty selector — nil OR present-but-empty `{}` — matches nothing for a
	// scope clause. Unlike a binding (where kind-aware selectorBinds may treat `{}`
	// as "binds all"), an empty *authorisation* selector that matched everything in
	// the gate would be a silent namespace-wide over-grant. Fail safe.
	if len(sc.Selector.MatchLabels) == 0 && len(sc.Selector.MatchExpressions) == 0 {
		return false
	}
	lbls, ok := labels(r)
	if !ok {
		return false
	}
	return sc.Selector.Matches(lbls)
}
