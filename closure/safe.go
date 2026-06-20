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
func Safe(s State, a Action, scope []ScopeRef) Decision {
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

	var escaping []Ref
	for _, r := range c {
		if !matchScope(r, scope) {
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

// matchScope reports whether r is covered by any scope clause. A clause matches
// when Kind (and Group/Version when specified) and namespace agree and the
// resource name matches the clause's name pattern (glob via path.Match).
func matchScope(r Ref, scope []ScopeRef) bool {
	for _, sc := range scope {
		if sc.GVK.Kind != "" && sc.GVK.Kind != r.GVK.Kind {
			continue
		}
		if sc.GVK.Group != "" && sc.GVK.Group != r.GVK.Group {
			continue
		}
		if sc.GVK.Version != "" && sc.GVK.Version != r.GVK.Version {
			continue
		}
		if sc.Namespace != r.Namespace {
			continue
		}
		if ok, _ := path.Match(sc.Name, r.Name); ok {
			return true
		}
	}
	return false
}
