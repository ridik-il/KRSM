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

	// labels resolves a candidate's labels from the same snapshot the closure was
	// computed over, so a selector clause expands over the world the closure walked.
	labels := func(r Ref) (map[string]string, bool) {
		o, ok := s.Get(r)
		if !ok {
			return nil, false
		}
		return o.Labels, true
	}

	var escaping []Ref
	for _, r := range c {
		if !matchScope(r, scope, labels) {
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

// labelsOf resolves a candidate's labels from the same snapshot the closure was
// computed over. It returns (labels, true) when the ref is tracked; (nil, false)
// otherwise. matchScope takes this narrow lookup rather than the whole State so a
// selector clause can read a candidate's labels without coupling scope matching to
// the full State interface (DESIGN: alternatives considered).
type labelsOf func(Ref) (map[string]string, bool)

// matchScope reports whether r is covered by any scope clause (OR across clauses).
// Both dimensions gate on GVK (Kind, and Group/Version when specified) and
// namespace. A DimResource clause then matches the resource name against the
// clause's name pattern (glob via path.Match) — unchanged from v0.1. A DimSelector
// clause instead matches r's live labels against the clause's selector.
//
// The dimension switch is explicit and fail-closed: a clause whose Dim is neither
// resource nor selector (a typo, or a not-yet-implemented dimension) covers
// NOTHING — it is skipped, never coerced into a resource grant. dimOf maps an empty
// Dim to DimResource so dim-less v0.1/v0.2 clauses still match as resource; any
// other unrecognised Dim falls through to the default skip. The loader
// (parseScope/Validate) rejects unknown dimensions at load time, so this engine
// branch is a defence-in-depth guard against a clause constructed in code.
func matchScope(r Ref, scope []ScopeClause, labels labelsOf) bool {
	for _, sc := range scope {
		if !gates(sc, r) {
			continue
		}
		switch dimOf(sc) {
		case DimResource:
			if ok, _ := path.Match(sc.Name, r.Name); ok {
				return true
			}
		case DimSelector:
			if matchSelectorClause(sc, r, labels) {
				return true
			}
		default:
			// Unknown dimension (not resource/selector): grant nothing (fail-closed).
			// Fall through to the next clause without matching — never coerced into a
			// resource grant. The loader rejects this at load; this is defence-in-depth.
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
func matchSelectorClause(sc ScopeClause, r Ref, labels labelsOf) bool {
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
