package scope_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ridik-il/krsm/closure"
	"github.com/ridik-il/krsm/scope"
)

// Test 1: a TaskContract with a resource clause and a selector clause compiles to
// the expected closure.ScopeClause values, with MaxSeverity carried through.
func TestCompileResourceAndSelectorClauses(t *testing.T) {
	tc := scope.TaskContract{
		APIVersion: "krsm.io/v1alpha1",
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

// Test-list entry 1 (slice 6): the TaskContract envelope is the CRD version slice 7
// ships — `krsm.io/v1alpha1`. The pre-CRD `krsm.io/v1` is no longer recognised, so a
// contract written against the old literal fails closed rather than compiling into a
// scope the cluster's CRD would never have admitted.
func TestCompileAcceptsV1Alpha1AndRejectsV1(t *testing.T) {
	base := func(apiVersion string) scope.TaskContract {
		return scope.TaskContract{
			APIVersion: apiVersion,
			Kind:       "TaskContract",
			Spec:       scope.Spec{Allow: nil},
		}
	}

	if _, err := scope.Compile(base("krsm.io/v1alpha1")); err != nil {
		t.Fatalf("Compile(krsm.io/v1alpha1): unexpected error: %v", err)
	}
	if _, err := scope.Compile(base("krsm.io/v1")); err == nil {
		t.Fatal("Compile(krsm.io/v1): expected an unrecognised-apiVersion error, got nil")
	}
}

// Test 1b: Compile stamps ProvenanceContract on its result — the verdict report can
// distinguish a declared scope from a derived one (ADR-0011).
func TestCompileSetsContractProvenance(t *testing.T) {
	tc := scope.TaskContract{
		APIVersion: "krsm.io/v1alpha1",
		Kind:       "TaskContract",
		Spec:       scope.Spec{Allow: nil},
	}
	pred, err := scope.Compile(tc)
	if err != nil {
		t.Fatalf("Compile: unexpected error: %v", err)
	}
	if pred.Provenance != scope.ProvenanceContract {
		t.Errorf("Provenance = %q, want %q", pred.Provenance, scope.ProvenanceContract)
	}
}

// TestDerive: the Level-0 synthesizer returns exactly one ownership clause rooted at
// the target — the target plus everything it owns — and NO namespace clause (a
// namespace allow-clause would re-admit same-namespace collateral under the union
// semantics of C ⊆ scope; ADR-0011). Provenance records the derivation.
func TestDerive(t *testing.T) {
	target := closure.Ref{
		GVK:       closure.GVK{Group: "apps", Version: "v1", Kind: "Deployment"},
		Namespace: "prod",
		Name:      "web",
		UID:       "uid:Deployment/prod/web",
	}

	pred := scope.Derive(target)

	want := []closure.ScopeClause{closure.OwnershipClause(target)}
	if !reflect.DeepEqual(pred.Clauses, want) {
		t.Errorf("Clauses mismatch\n got: %#v\nwant: %#v", pred.Clauses, want)
	}
	if pred.Provenance != scope.ProvenanceDerivedOwner {
		t.Errorf("Provenance = %q, want %q", pred.Provenance, scope.ProvenanceDerivedOwner)
	}
	for _, c := range pred.Clauses {
		if c.Dim == closure.DimNamespace {
			t.Errorf("derived scope must not carry a namespace clause; got %#v", c)
		}
	}
}

// Test-list entry 2 (slice 6, design rev 3 D1): a contract may now declare the
// ownership dimension the closure engine has implemented since v0.4 but the contract
// surface could not reach. An `ownership` allow-clause whose Root carries a Kind and
// a Name compiles to exactly the clause closure.OwnershipClause(root) produces — so a
// declared contract can authorise the very same subtree scope.Derive synthesizes at
// Level 0 (and v0.6's ownership-tree template has a compile target).
func TestCompileOwnershipClause(t *testing.T) {
	root := closure.Ref{
		GVK:       closure.GVK{Group: "apps", Version: "v1", Kind: "Deployment"},
		Namespace: "prod",
		Name:      "frontend",
		UID:       "uid:Deployment/prod/frontend",
	}
	tc := scope.TaskContract{
		APIVersion: "krsm.io/v1alpha1",
		Kind:       "TaskContract",
		Spec: scope.Spec{
			Allow: []scope.AllowClause{{Dim: closure.DimOwnership, Root: root}},
		},
	}

	pred, err := scope.Compile(tc)
	if err != nil {
		t.Fatalf("Compile: unexpected error: %v", err)
	}
	want := []closure.ScopeClause{closure.OwnershipClause(root)}
	if !reflect.DeepEqual(pred.Clauses, want) {
		t.Errorf("Clauses mismatch\n got: %#v\nwant: %#v", pred.Clauses, want)
	}
	if !reflect.DeepEqual(pred.Clauses, scope.Derive(root).Clauses) {
		t.Errorf("a declared ownership clause must authorise the same subtree Derive synthesizes\n got: %#v\nwant: %#v", pred.Clauses, scope.Derive(root).Clauses)
	}
}

// Test-list entry 3 (slice 6, design rev 3 D1): a `namespace` allow-clause compiles
// to the namespace-dimension clause covering that namespace, with the GVK kept as the
// optional kind gate closure.NamespaceClause defines.
func TestCompileNamespaceClause(t *testing.T) {
	tc := scope.TaskContract{
		APIVersion: "krsm.io/v1alpha1",
		Kind:       "TaskContract",
		Spec: scope.Spec{
			Allow: []scope.AllowClause{
				{Dim: closure.DimNamespace, Namespace: "prod"},
				{Dim: closure.DimNamespace, GVK: closure.GVK{Version: "v1", Kind: "Pod"}, Namespace: "staging"},
			},
		},
	}

	pred, err := scope.Compile(tc)
	if err != nil {
		t.Fatalf("Compile: unexpected error: %v", err)
	}
	want := []closure.ScopeClause{
		closure.NamespaceClause(closure.GVK{}, "prod"),
		closure.NamespaceClause(closure.GVK{Version: "v1", Kind: "Pod"}, "staging"),
	}
	if !reflect.DeepEqual(pred.Clauses, want) {
		t.Errorf("Clauses mismatch\n got: %#v\nwant: %#v", pred.Clauses, want)
	}
}

// Test-list entry 4 (slice 6, design rev 3 D1): admitting the ownership and namespace
// dimensions must not weaken their structural rules. Compile builds the clause
// preserving EVERY field the author declared and runs closure.ScopeClause.Validate
// over it, so a field the dimension would silently ignore is a loud compile error
// rather than a quietly widened or narrowed scope. (Building via the safe
// constructors instead would drop these fields unnoticed — that is the regression
// this test pins.)
func TestCompileStructurallyInvalidOwnershipAndNamespaceClausesError(t *testing.T) {
	root := closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Deployment"}, Namespace: "prod", Name: "frontend"}
	tests := []struct {
		name   string
		clause scope.AllowClause
	}{
		{"ownership with clause-level GVK", scope.AllowClause{
			Dim: closure.DimOwnership, Root: root,
			GVK: closure.GVK{Group: "apps", Version: "v1", Kind: "Deployment"},
		}},
		{"ownership with clause-level namespace", scope.AllowClause{
			Dim: closure.DimOwnership, Root: root, Namespace: "prod",
		}},
		{"ownership with clause-level name", scope.AllowClause{
			Dim: closure.DimOwnership, Root: root, Name: "frontend",
		}},
		{"ownership with clause-level selector", scope.AllowClause{
			Dim: closure.DimOwnership, Root: root,
			Selector: closure.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
		}},
		{"ownership with a root missing its name", scope.AllowClause{
			Dim: closure.DimOwnership, Root: closure.Ref{GVK: closure.GVK{Kind: "Deployment"}},
		}},
		{"namespace with no namespace", scope.AllowClause{
			Dim: closure.DimNamespace, GVK: closure.GVK{Version: "v1", Kind: "Pod"},
		}},
		{"namespace carrying a root", scope.AllowClause{
			Dim: closure.DimNamespace, Namespace: "prod", Root: root,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc := scope.TaskContract{
				APIVersion: "krsm.io/v1alpha1",
				Kind:       "TaskContract",
				Spec:       scope.Spec{Allow: []scope.AllowClause{tt.clause}},
			}
			if _, err := scope.Compile(tc); err == nil {
				t.Fatalf("Compile: expected an error for %s, got nil", tt.name)
			}
		})
	}
}

// Test 2 / test-list entry 5 (slice 6): an unknown dimension is a hard compile error —
// fail closed, not a silent skip that would yield a narrowed scope. Re-pointed from
// `ownership` (now supported, D1) to `reference`: DESIGN §6 once listed it as future
// syntax, but closure defines exactly four dimensions and `reference` is not one of
// them, so it is the standing proof that an unrecognised dim never compiles. The
// error must name the *dimension* — a structural complaint from Validate would mean
// the dim slipped through the compiler's accepted set.
func TestCompileUnsupportedDimensionErrors(t *testing.T) {
	tc := scope.TaskContract{
		APIVersion: "krsm.io/v1alpha1",
		Kind:       "TaskContract",
		Spec: scope.Spec{
			Allow: []scope.AllowClause{
				{
					Dim:       "reference",
					GVK:       closure.GVK{Group: "apps", Version: "v1", Kind: "Deployment"},
					Namespace: "prod",
					Name:      "frontend",
				},
			},
		},
	}

	_, err := scope.Compile(tc)
	if err == nil {
		t.Fatal("Compile: expected an error for an unsupported scope dimension, got nil")
	}
	if want := `unsupported scope dimension "reference"`; !strings.Contains(err.Error(), want) {
		t.Errorf("Compile error = %q, want it to contain %q", err.Error(), want)
	}
}

// Test 3: a wrong or absent apiVersion or kind is a hard compile error —
// fail-closed on an unrecognised contract envelope.
func TestCompileBadEnvelopeErrors(t *testing.T) {
	base := func() scope.TaskContract {
		return scope.TaskContract{
			APIVersion: "krsm.io/v1alpha1",
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
		APIVersion: "krsm.io/v1alpha1",
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
		APIVersion: "krsm.io/v1alpha1",
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
		APIVersion: "krsm.io/v1alpha1",
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
