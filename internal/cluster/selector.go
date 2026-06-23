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
		m, found, err := unstructured.NestedStringMap(o.Object, "spec", "selector")
		if err != nil {
			return closure.LabelSelector{}, fmt.Errorf("parse Service selector: %w", err)
		}
		if !found {
			return closure.LabelSelector{}, nil // absent → binds nothing
		}
		// present (incl. {}) → non-nil map; selectorBinds decides empty-Service.
		if m == nil {
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
	wrap, found, err := unstructured.NestedMap(o.Object, path...)
	if err != nil {
		return closure.LabelSelector{}, fmt.Errorf("parse selector: %w", err)
	}
	if !found {
		return closure.LabelSelector{}, nil
	}

	sel := closure.LabelSelector{}
	if ml, ok, mlErr := unstructured.NestedStringMap(wrap, "matchLabels"); mlErr == nil && ok {
		sel.MatchLabels = ml
	}

	exprs, ok, exErr := unstructured.NestedSlice(wrap, "matchExpressions")
	if exErr != nil {
		return closure.LabelSelector{}, fmt.Errorf("parse matchExpressions: %w", exErr)
	}
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
			values, _, _ := unstructured.NestedStringSlice(m, "values")
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
