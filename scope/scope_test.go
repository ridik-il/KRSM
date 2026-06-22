package scope_test

import (
	"reflect"
	"testing"

	"github.com/ridik-il/krsm/closure"
	"github.com/ridik-il/krsm/scope"
)

// Test 1: a TaskContract with a resource clause and a selector clause compiles to
// the expected closure.ScopeClause values, with MaxSeverity carried through.
func TestCompileResourceAndSelectorClauses(t *testing.T) {
	tc := scope.TaskContract{
		APIVersion: "krsm.io/v1",
		Kind:       "TaskContract",
		Metadata:   scope.Metadata{Name: "restart-frontend-web-pods", Namespace: "prod"},
		Spec: scope.Spec{
			Allow: []scope.AllowClause{
				{
					Dim:       closure.DimResource,
					GVK:       closure.GVK{Group: "apps", Version: "v1", Kind: "Deployment"},
					Namespace: "prod",
					Name:      "frontend",
				},
				{
					Dim:       closure.DimSelector,
					GVK:       closure.GVK{Version: "v1", Kind: "Pod"},
					Namespace: "prod",
					Selector: closure.LabelSelector{
						MatchExpressions: []closure.SelectorRequirement{
							{Key: "app", Operator: closure.OpIn, Values: []string{"web"}},
						},
					},
				},
			},
			MaxSeverity: scope.SeverityHigh,
		},
	}

	pred, err := scope.Compile(tc)
	if err != nil {
		t.Fatalf("Compile: unexpected error: %v", err)
	}

	want := []closure.ScopeClause{
		closure.ResourceClause(closure.GVK{Group: "apps", Version: "v1", Kind: "Deployment"}, "prod", "frontend"),
		closure.SelectorClause(closure.GVK{Version: "v1", Kind: "Pod"}, "prod", closure.LabelSelector{
			MatchExpressions: []closure.SelectorRequirement{
				{Key: "app", Operator: closure.OpIn, Values: []string{"web"}},
			},
		}),
	}
	if !reflect.DeepEqual(pred.Clauses, want) {
		t.Errorf("Clauses mismatch\n got: %#v\nwant: %#v", pred.Clauses, want)
	}
	if pred.MaxSeverity != scope.SeverityHigh {
		t.Errorf("MaxSeverity = %q, want %q", pred.MaxSeverity, scope.SeverityHigh)
	}
}

// Test 2: an unsupported/unknown dimension (a valid-future-syntax but unbuilt
// dim like ownership) is a hard compile error — fail closed, not a silent skip
// that would yield a narrowed scope.
func TestCompileUnsupportedDimensionErrors(t *testing.T) {
	tc := scope.TaskContract{
		APIVersion: "krsm.io/v1",
		Kind:       "TaskContract",
		Spec: scope.Spec{
			Allow: []scope.AllowClause{
				{
					Dim:       "ownership",
					GVK:       closure.GVK{Group: "apps", Version: "v1", Kind: "Deployment"},
					Namespace: "prod",
					Name:      "frontend",
				},
			},
		},
	}

	if _, err := scope.Compile(tc); err == nil {
		t.Fatal("Compile: expected an error for an unsupported scope dimension, got nil")
	}
}

// Test 3: a wrong or absent apiVersion or kind is a hard compile error —
// fail-closed on an unrecognised contract envelope.
func TestCompileBadEnvelopeErrors(t *testing.T) {
	base := func() scope.TaskContract {
		return scope.TaskContract{
			APIVersion: "krsm.io/v1",
			Kind:       "TaskContract",
			Spec:       scope.Spec{Allow: nil},
		}
	}
	tests := []struct {
		name   string
		mutate func(tc *scope.TaskContract)
	}{
		{"wrong apiVersion", func(tc *scope.TaskContract) { tc.APIVersion = "v1" }},
		{"absent apiVersion", func(tc *scope.TaskContract) { tc.APIVersion = "" }},
		{"wrong kind", func(tc *scope.TaskContract) { tc.Kind = "Pod" }},
		{"absent kind", func(tc *scope.TaskContract) { tc.Kind = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc := base()
			tt.mutate(&tc)
			if _, err := scope.Compile(tc); err == nil {
				t.Fatalf("Compile: expected an error for %s, got nil", tt.name)
			}
		})
	}
}

// Test 4: a maxSeverity outside the known set is a hard compile error so a typo is
// caught now (even though severity is carried, not enforced, this slice).
func TestCompileUnknownMaxSeverityErrors(t *testing.T) {
	tc := scope.TaskContract{
		APIVersion: "krsm.io/v1",
		Kind:       "TaskContract",
		Spec:       scope.Spec{MaxSeverity: "extreme"},
	}
	if _, err := scope.Compile(tc); err == nil {
		t.Fatal("Compile: expected an error for an unknown maxSeverity, got nil")
	}
}

// Test 5: an empty spec.allow compiles to an empty predicate (a contract that
// authorises nothing) with NO error — legal and fail-safe (everything escapes →
// Block), not a compile failure.
func TestCompileEmptyAllowYieldsEmptyPredicate(t *testing.T) {
	tc := scope.TaskContract{
		APIVersion: "krsm.io/v1",
		Kind:       "TaskContract",
		Spec:       scope.Spec{Allow: nil},
	}
	pred, err := scope.Compile(tc)
	if err != nil {
		t.Fatalf("Compile: unexpected error: %v", err)
	}
	if len(pred.Clauses) != 0 {
		t.Errorf("Clauses = %#v, want empty", pred.Clauses)
	}
}

// Test 6: a structurally invalid clause (a selector clause that also carries a
// Name) is a hard compile error — closure.ScopeClause.Validate surfaced through
// the compiler so a malformed contract fails closed.
func TestCompileInvalidClauseErrors(t *testing.T) {
	tc := scope.TaskContract{
		APIVersion: "krsm.io/v1",
		Kind:       "TaskContract",
		Spec: scope.Spec{
			Allow: []scope.AllowClause{
				{
					Dim:       closure.DimSelector,
					GVK:       closure.GVK{Version: "v1", Kind: "Pod"},
					Namespace: "prod",
					Name:      "frontend-aaa", // a selector clause must not carry a name
					Selector: closure.LabelSelector{
						MatchLabels: map[string]string{"app": "web"},
					},
				},
			},
		},
	}
	if _, err := scope.Compile(tc); err == nil {
		t.Fatal("Compile: expected an error for a selector clause carrying a name, got nil")
	}
}
