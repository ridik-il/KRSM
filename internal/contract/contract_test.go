package contract_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ridik-il/krsm/closure"
	"github.com/ridik-il/krsm/internal/contract"
	"github.com/ridik-il/krsm/scope"
)

// goldenContract reads a scenario's taskcontract.yaml from the shared corpus. The
// tests below parse the REAL fixture rather than a hand-copied excerpt, so a wire
// form the corpus relies on cannot drift away from what Parse accepts.
func goldenContract(t *testing.T, scenario string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "closure", "testdata", "scenarios", scenario, "taskcontract.yaml"))
	if err != nil {
		t.Fatalf("read golden taskcontract: %v", err)
	}
	return raw
}

// Test-list entry 7 (slice 6, design rev 3 T3): Parse is structurally strict —
// unknown fields are rejected — and that strictness must NOT cost the corpus its
// selector clause. Golden 21's `matchExpressions` used to be read out-of-band (the
// raw clause struct never declared it), so turning on DisallowUnknownFields would
// have rejected the very fixture the compiler path is pinned by. This parses the
// real golden and compiles it, asserting the selector survives intact.
func TestParseGoldenSelectorContractUnderStrictDecoding(t *testing.T) {
	tc, err := contract.Parse(goldenContract(t, "21-taskcontract-selector-scope"))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
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
	if tc.Spec.MaxSeverity != scope.SeverityHigh {
		t.Errorf("MaxSeverity = %q, want %q", tc.Spec.MaxSeverity, scope.SeverityHigh)
	}
}

// Test-list entry 7 (slice 6, design rev 3 D1/T3): the ownership dimension's `root`
// is a declared field on the raw clause, so a contract can express the subtree scope
// the compiler now accepts. The root's namespace defaults and its offline identity
// resolve exactly as the scenario loader's scope.yaml `root` does, so the SAME
// ownership clause authorises the same subtree whichever wire form declared it.
func TestParseOwnershipRoot(t *testing.T) {
	raw := []byte(`
apiVersion: krsm.io/v1alpha1
kind: TaskContract
metadata:
  name: restart-frontend-tree
  namespace: prod
spec:
  allow:
    - dim: ownership
      root: {group: apps, version: v1, kind: Deployment, namespace: prod, name: frontend}
  maxSeverity: high
`)
	tc, err := contract.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}

	pred, err := scope.Compile(tc)
	if err != nil {
		t.Fatalf("Compile: unexpected error: %v", err)
	}
	want := []closure.ScopeClause{closure.OwnershipClause(closure.Ref{
		GVK:       closure.GVK{Group: "apps", Version: "v1", Kind: "Deployment"},
		Namespace: "prod",
		Name:      "frontend",
		UID:       contract.SyntheticUID("Deployment", "prod", "frontend"),
	})}
	if !reflect.DeepEqual(pred.Clauses, want) {
		t.Errorf("Clauses mismatch\n got: %#v\nwant: %#v", pred.Clauses, want)
	}
}

// Namespace defaulting must not manufacture an authorisation. For a resource or
// selector clause the namespace is an object GATE and defaults like the objects do;
// for a `namespace` clause it is the SUBJECT of the grant, so an omitted one has to
// stay omitted and be rejected by Compile. Defaulting it to "default" would silently
// hand out the whole default namespace to a contract that named no namespace at all —
// a scope widening invented by the parser, which the fail-closed compiler could then
// no longer catch.
func TestParseDoesNotDefaultAnOmittedNamespaceGrant(t *testing.T) {
	raw := []byte(`
apiVersion: krsm.io/v1alpha1
kind: TaskContract
metadata:
  name: no-namespace-named
spec:
  allow:
    - dim: namespace
      gvk: {version: v1, kind: Pod}
`)
	tc, err := contract.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	if got := tc.Spec.Allow[0].Namespace; got != "" {
		t.Errorf("Namespace = %q, want %q — an omitted namespace grant must not be defaulted", got, "")
	}
	if _, err := scope.Compile(tc); err == nil {
		t.Fatal("Compile: expected a malformed-namespace-clause error, got nil (the parser invented a namespace grant)")
	}
}

// Test-list entry 7 (slice 6): Parse is structurally strict. A contract is
// attacker-influenced input and scope.Compile cannot reject a field a lax parser has
// already thrown away, so an undeclared key is an error at the parse boundary. The
// dangerous case is an undeclared key where a scope-NARROWING one was meant: a
// `nameGlob:` that the parser ignores leaves the clause with an empty Name, and an
// empty name on a resource clause is not an error but a DIFFERENT grant — silently
// unrelated to the resource the author believed they had restricted the task to.
func TestParseRejectsUnknownFields(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"undeclared key in an allow clause", `
apiVersion: krsm.io/v1alpha1
kind: TaskContract
metadata: {name: c, namespace: prod}
spec:
  allow:
    - dim: resource
      gvk: {group: apps, version: v1, kind: Deployment}
      namespace: prod
      nameGlob: frontend
`},
		{"unknown key in spec", `
apiVersion: krsm.io/v1alpha1
kind: TaskContract
metadata: {name: c, namespace: prod}
spec:
  allow: []
  maxSeverty: high
`},
		{"unknown key at the top level", `
apiVersion: krsm.io/v1alpha1
kind: TaskContract
metadata: {name: c, namespace: prod}
status: {phase: Active}
spec:
  allow: []
`},
		{"unknown key in a clause gvk", `
apiVersion: krsm.io/v1alpha1
kind: TaskContract
metadata: {name: c, namespace: prod}
spec:
  allow:
    - dim: resource
      gvk: {group: apps, version: v1, kind: Deployment, resource: deployments}
      namespace: prod
      name: frontend
`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := contract.Parse([]byte(tt.raw)); err == nil {
				t.Fatalf("Parse: expected an unknown-field error for %s, got nil", tt.name)
			}
		})
	}
}

// Test-list entry 8 (slice 6): ONE parse path serves both sources of a contract. The
// scenario loader hands Parse a taskcontract.yaml off disk; the webhook will hand it
// the JSON an unstructured TaskContract CR marshals to. JSON is a subset of YAML 1.2,
// so the same decoder reads both — and this pins that they agree, so a contract
// cannot compile to one scope on the CLI and a different one in the cluster.
func TestParseJSONAndYAMLCompileIdentically(t *testing.T) {
	const asJSON = `{
	  "apiVersion": "krsm.io/v1alpha1",
	  "kind": "TaskContract",
	  "metadata": {"name": "restart-frontend-web-pods", "namespace": "prod"},
	  "spec": {
	    "allow": [
	      {"dim": "resource", "gvk": {"group": "apps", "version": "v1", "kind": "Deployment"}, "namespace": "prod", "name": "frontend"},
	      {"dim": "selector", "gvk": {"version": "v1", "kind": "Pod"}, "namespace": "prod",
	       "matchLabels": {"tier": "web"},
	       "matchExpressions": [{"key": "app", "operator": "In", "values": ["web"]}]},
	      {"dim": "namespace", "namespace": "staging"},
	      {"dim": "ownership", "root": {"group": "apps", "version": "v1", "kind": "Deployment", "namespace": "prod", "name": "frontend"}}
	    ],
	    "maxSeverity": "high"
	  }
	}`
	const asYAML = `
apiVersion: krsm.io/v1alpha1
kind: TaskContract
metadata:
  name: restart-frontend-web-pods
  namespace: prod
spec:
  allow:
    - dim: resource
      gvk: {group: apps, version: v1, kind: Deployment}
      namespace: prod
      name: frontend
    - dim: selector
      gvk: {version: v1, kind: Pod}
      namespace: prod
      matchLabels: {tier: web}
      matchExpressions:
        - {key: app, operator: In, values: [web]}
    - dim: namespace
      namespace: staging
    - dim: ownership
      root: {group: apps, version: v1, kind: Deployment, namespace: prod, name: frontend}
  maxSeverity: high
`

	fromJSON, err := contract.Parse([]byte(asJSON))
	if err != nil {
		t.Fatalf("Parse(JSON): unexpected error: %v", err)
	}
	fromYAML, err := contract.Parse([]byte(asYAML))
	if err != nil {
		t.Fatalf("Parse(YAML): unexpected error: %v", err)
	}
	if !reflect.DeepEqual(fromJSON, fromYAML) {
		t.Errorf("parsed contracts differ\nJSON: %#v\nYAML: %#v", fromJSON, fromYAML)
	}

	jsonPred, err := scope.Compile(fromJSON)
	if err != nil {
		t.Fatalf("Compile(JSON): unexpected error: %v", err)
	}
	yamlPred, err := scope.Compile(fromYAML)
	if err != nil {
		t.Fatalf("Compile(YAML): unexpected error: %v", err)
	}
	if !reflect.DeepEqual(jsonPred.Clauses, yamlPred.Clauses) {
		t.Errorf("compiled clauses differ\nJSON: %#v\nYAML: %#v", jsonPred.Clauses, yamlPred.Clauses)
	}
	if len(jsonPred.Clauses) != 4 {
		t.Fatalf("compiled %d clauses, want 4 (one per declared dimension)", len(jsonPred.Clauses))
	}
	for i, dim := range []closure.ScopeDim{closure.DimResource, closure.DimSelector, closure.DimNamespace, closure.DimOwnership} {
		if got := jsonPred.Clauses[i].Dim; got != dim {
			t.Errorf("clause %d Dim = %q, want %q", i, got, dim)
		}
	}
}
