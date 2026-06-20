package closure

import "testing"

// TestLabelSelectorMatches pins the four-operator semantics, especially the
// absence-sensitive NotIn/DoesNotExist (they match a MISSING key) — the trap a
// key-iterating implementation silently gets wrong.
func TestLabelSelectorMatches(t *testing.T) {
	req := func(k string, op SelectorOperator, vals ...string) SelectorRequirement {
		return SelectorRequirement{Key: k, Operator: op, Values: vals}
	}
	cases := []struct {
		name   string
		sel    LabelSelector
		labels map[string]string
		want   bool
	}{
		{"empty selector matches anything", LabelSelector{}, map[string]string{"a": "b"}, true},
		{"matchLabels hit", LabelSelector{MatchLabels: map[string]string{"app": "web"}}, map[string]string{"app": "web", "x": "y"}, true},
		{"matchLabels miss", LabelSelector{MatchLabels: map[string]string{"app": "web"}}, map[string]string{"app": "db"}, false},
		{"In hit", LabelSelector{MatchExpressions: []SelectorRequirement{req("tier", OpIn, "frontend", "web")}}, map[string]string{"tier": "web"}, true},
		{"In miss value", LabelSelector{MatchExpressions: []SelectorRequirement{req("tier", OpIn, "frontend")}}, map[string]string{"tier": "web"}, false},
		{"In miss absent", LabelSelector{MatchExpressions: []SelectorRequirement{req("tier", OpIn, "web")}}, map[string]string{"other": "x"}, false},
		{"NotIn hit value", LabelSelector{MatchExpressions: []SelectorRequirement{req("tier", OpNotIn, "db")}}, map[string]string{"tier": "web"}, true},
		{"NotIn hit absent (absence-sensitive)", LabelSelector{MatchExpressions: []SelectorRequirement{req("tier", OpNotIn, "db")}}, map[string]string{"other": "x"}, true},
		{"NotIn miss", LabelSelector{MatchExpressions: []SelectorRequirement{req("tier", OpNotIn, "db")}}, map[string]string{"tier": "db"}, false},
		{"Exists hit", LabelSelector{MatchExpressions: []SelectorRequirement{req("tier", OpExists)}}, map[string]string{"tier": "anything"}, true},
		{"Exists miss", LabelSelector{MatchExpressions: []SelectorRequirement{req("tier", OpExists)}}, map[string]string{"other": "x"}, false},
		{"DoesNotExist hit (absence-sensitive)", LabelSelector{MatchExpressions: []SelectorRequirement{req("tier", OpDoesNotExist)}}, map[string]string{"other": "x"}, true},
		{"DoesNotExist miss", LabelSelector{MatchExpressions: []SelectorRequirement{req("tier", OpDoesNotExist)}}, map[string]string{"tier": "web"}, false},
		{"AND of matchLabels and expression", LabelSelector{MatchLabels: map[string]string{"app": "web"}, MatchExpressions: []SelectorRequirement{req("tier", OpIn, "frontend")}}, map[string]string{"app": "web", "tier": "backend"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.sel.Matches(tc.labels); got != tc.want {
				t.Errorf("Matches = %v, want %v", got, tc.want)
			}
		})
	}
}
