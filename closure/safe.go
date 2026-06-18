package closure

import (
	"path"
	"sort"
)

// Safe is the verdict: it computes the action's closure and decides whether it
// stays within the authorised scope. An in-cluster closure member not covered by
// any scope clause is an escape (→ Block). A cross-boundary effect with no
// escape is a Warn. Otherwise Allow. Precedence is Block > Warn > Allow.
func Safe(s State, a Action, scope []ScopeRef) Decision {
	c := Closure(s, a)
	external := ExternalEffects(s, a)

	var escaping []Ref
	for _, r := range c {
		if !matchScope(r, scope) {
			escaping = append(escaping, r)
		}
	}
	sort.Slice(escaping, func(i, j int) bool { return escaping[i].human() < escaping[j].human() })

	verdict := Allow
	switch {
	case len(escaping) > 0:
		verdict = Block
	case len(external) > 0:
		verdict = Warn
	}

	return Decision{
		Verdict:  verdict,
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
