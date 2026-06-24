package cluster

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ridik-il/krsm/closure"
)

// selectorFromUnstructured resolves the flattened selector for a kind, mirroring
// internal/scenario.selectorFrom exactly: Service uses a flat spec.selector map,
// NetworkPolicy uses spec.podSelector.{matchLabels,matchExpressions}, every other kind
// uses spec.selector.{matchLabels,matchExpressions}. The nil-vs-present-empty
// distinction (an absent selector binds nothing; a present-empty `{}` is kind-decided)
// is preserved via NestedMap's found return, as the loader preserves it via a nil
// json.RawMessage.
func selectorFromUnstructured(kind string, o unstructured.Unstructured) (closure.LabelSelector, error) {
	switch kind {
	case "Service":
		// A Service selector is a flat map. Distinguish ABSENT (binds nothing) from
		// PRESENT-empty `{}` (kind decides) via found, using a no-copy read so an
		// adversarial non-copyable value can never panic. A present-but-non-map selector
		// (malformed) is treated as absent → binds nothing (fail-safe; a Service with a
		// garbage selector has no resolvable endpoints).
		if _, found := nestedMap(o.Object, "spec", "selector"); !found {
			return closure.LabelSelector{}, nil // absent → binds nothing
		}
		m, ok := nestedStringMap(o.Object, "spec", "selector")
		if !ok || m == nil {
			// present but {} or non-string values: a non-nil empty map; the kind-aware
			// selectorBinds decides an empty Service selector binds nothing.
			m = map[string]string{}
		}
		return closure.LabelSelector{MatchLabels: m}, nil
	case "NetworkPolicy":
		return matchLabelsFrom(o, "spec", "podSelector")
	default:
		return matchLabelsFrom(o, "spec", "selector")
	}
}

// matchLabelsFrom resolves a `{matchLabels, matchExpressions}` selector wrapper at the
// given path, mirroring internal/scenario.matchLabels: an absent wrapper yields the nil
// selector (binds nothing); a wrapper with neither field yields a non-nil empty map
// (present-empty → kind decides). matchExpressions are captured so set-based
// requirements bind precisely; an unrecognised operator is REJECTED (fail-closed) — a
// silently dropped binding would be a missed escape.
func matchLabelsFrom(o unstructured.Unstructured, path ...string) (closure.LabelSelector, error) {
	wrap, found := nestedMap(o.Object, path...)
	if !found {
		return closure.LabelSelector{}, nil
	}

	sel := closure.LabelSelector{}
	if ml, ok := nestedStringMap(wrap, "matchLabels"); ok {
		sel.MatchLabels = ml
	}

	exprs, ok := nestedSlice(wrap, "matchExpressions")
	if ok {
		for _, e := range exprs {
			m, isMap := e.(map[string]any)
			if !isMap {
				continue
			}
			key := nestedString(m, "key")
			op := closure.SelectorOperator(nestedString(m, "operator"))
			if !op.Valid() {
				return closure.LabelSelector{}, fmt.Errorf("invalid selector operator %q for key %q (want In, NotIn, Exists or DoesNotExist)", op, key)
			}
			values, _ := nestedStringSlice(m, "values")
			sel.MatchExpressions = append(sel.MatchExpressions, closure.SelectorRequirement{
				Key:      key,
				Operator: op,
				Values:   values,
			})
		}
	}

	if sel.MatchLabels == nil && len(sel.MatchExpressions) == 0 {
		sel.MatchLabels = map[string]string{} // present but empty → matches all
	}
	return sel, nil
}
