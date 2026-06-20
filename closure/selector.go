package closure

// LabelSelector is a pure-stdlib model of a Kubernetes label selector
// (metav1.LabelSelector). It carries both the sugar `matchLabels` form and the
// general `matchExpressions` form; the two are AND-ed. Keeping this type inside
// `closure` lets the embeddable SDK follow selector bindings faithfully without
// importing k8s.io/apimachinery (DESIGN §7, ADR-0002, ADR-0005).
//
// A nil-vs-present-empty distinction is preserved by callers: the kind-aware
// empty-selector rule (an empty Service selector binds nothing; an empty
// NetworkPolicy/PDB/workload selector binds all pods) lives in selectorBinds,
// keyed by owner kind, not here — Matches on an empty selector matches anything,
// the apimachinery semantics.
type LabelSelector struct {
	MatchLabels      map[string]string
	MatchExpressions []SelectorRequirement
}

// isNil reports whether the selector is absent (no matchLabels map and no
// matchExpressions). An absent selector binds nothing for every owner kind; this
// is distinct from a present-but-empty selector (`{}`), whose binding is decided
// by selectorBinds per owner kind. The loader preserves the distinction by
// leaving MatchLabels nil for an absent selector and a non-nil empty map for a
// present-empty one.
func (s LabelSelector) isNil() bool {
	return s.MatchLabels == nil && len(s.MatchExpressions) == 0
}

// equal reports whether two selectors are value-equal (used to detect a selector
// mutation). It mirrors the old map equality: length-then-membership, treating
// nil and empty as equal in length.
func (s LabelSelector) equal(o LabelSelector) bool {
	if len(s.MatchLabels) != len(o.MatchLabels) {
		return false
	}
	for k, v := range s.MatchLabels {
		if o.MatchLabels[k] != v {
			return false
		}
	}
	if len(s.MatchExpressions) != len(o.MatchExpressions) {
		return false
	}
	for i, r := range s.MatchExpressions {
		q := o.MatchExpressions[i]
		if r.Key != q.Key || r.Operator != q.Operator || len(r.Values) != len(q.Values) {
			return false
		}
		for j := range r.Values {
			if r.Values[j] != q.Values[j] {
				return false
			}
		}
	}
	return true
}

// SelectorOperator is a set-based requirement operator. Its constants equal the
// apimachinery wire values verbatim so the loader can map operator strings
// directly.
type SelectorOperator string

const (
	OpIn           SelectorOperator = "In"
	OpNotIn        SelectorOperator = "NotIn"
	OpExists       SelectorOperator = "Exists"
	OpDoesNotExist SelectorOperator = "DoesNotExist"
)

// SelectorRequirement is one matchExpressions clause: a key, an operator, and
// (for In/NotIn) a value set. Exists/DoesNotExist ignore Values.
type SelectorRequirement struct {
	Key      string
	Operator SelectorOperator
	Values   []string
}

// Matches reports whether the given label set satisfies every matchLabels pair
// and every matchExpressions requirement (all AND-ed). An empty selector matches
// any label set.
//
// NotIn and DoesNotExist are absence-sensitive: they are satisfied when the key
// is MISSING. A naive implementation that only iterates over present labels
// silently gets these wrong, so Matches evaluates each requirement against the
// full label set explicitly.
func (s LabelSelector) Matches(labels map[string]string) bool {
	for k, v := range s.MatchLabels {
		if labels[k] != v {
			return false
		}
	}
	for _, req := range s.MatchExpressions {
		if !req.matches(labels) {
			return false
		}
	}
	return true
}

// matches evaluates a single set-based requirement against a full label set.
func (r SelectorRequirement) matches(labels map[string]string) bool {
	val, present := labels[r.Key]
	switch r.Operator {
	case OpIn:
		return present && contains(r.Values, val)
	case OpNotIn:
		return !present || !contains(r.Values, val)
	case OpExists:
		return present
	case OpDoesNotExist:
		return !present
	default:
		// An unknown operator binds nothing: fail closed rather than silently
		// treat it as a match.
		return false
	}
}

func contains(vals []string, v string) bool {
	for _, x := range vals {
		if x == v {
			return true
		}
	}
	return false
}
