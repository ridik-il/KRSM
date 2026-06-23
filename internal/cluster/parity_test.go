package cluster

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"sigs.k8s.io/yaml"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ridik-il/krsm/closure"
)

// humanRef is the Kind/namespace/name identity used in golden expected.yaml files.
type humanRef struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func (h humanRef) key() string { return h.Kind + "/" + h.Namespace + "/" + h.Name }

type expectedVerdict struct {
	Verdict  string     `json:"verdict"`
	Closure  []humanRef `json:"closure"`
	Escaping []humanRef `json:"escaping"`
	External []humanRef `json:"external"`
}

// TestParityWithGoldenScenario01 is the slice-1 acceptance oracle (the plan's KEY
// PARITY TEST): hand-built unstructured fixtures EQUIVALENT to golden scenario
// 01-memory-pressure-cascade are fed through BuildObjects → NewScanState, and the
// closure/escaping set computed by closure.Safe must MATCH the golden's expected.yaml.
// The corpus is the parity oracle: if the unstructured extractor diverges from the YAML
// loader, this test fails before any cluster is touched.
//
// Equivalence to the golden (closure/testdata/scenarios/01/cluster.yaml):
//   - Deployment web, ReplicaSet web-7f9 (owned by web), Pods web-1/2/3 (owned by
//     web-7f9), all labelled {app: web}; the owner edge is a REAL metadata.uid match.
//   - Service web-svc with spec.selector {app: web} (the label-selector binding).
//
// Action: delete deployment/web (cascade). Scope: the single Pod prod/web-1 (the
// golden's scope.yaml), as a dim-less DimResource clause.
func TestParityWithGoldenScenario01(t *testing.T) {
	const (
		uidDep = "uid-dep-web"
		uidRS  = "uid-rs-web-7f9"
	)
	webLabels := map[string]any{"app": "web"}

	objs, err := BuildObjects([]unstructured.Unstructured{
		u("apps/v1", "Deployment", "prod", "web", uidDep,
			withLabels(webLabels),
			withSpec(map[string]any{"selector": map[string]any{"matchLabels": map[string]any{"app": "web"}}}),
		),
		u("apps/v1", "ReplicaSet", "prod", "web-7f9", uidRS,
			withLabels(webLabels),
			withOwner("Deployment", "web", uidDep),
			withSpec(map[string]any{"selector": map[string]any{"matchLabels": map[string]any{"app": "web"}}}),
		),
		u("v1", "Pod", "prod", "web-1", "uid-p1", withLabels(webLabels), withOwner("ReplicaSet", "web-7f9", uidRS)),
		u("v1", "Pod", "prod", "web-2", "uid-p2", withLabels(webLabels), withOwner("ReplicaSet", "web-7f9", uidRS)),
		u("v1", "Pod", "prod", "web-3", "uid-p3", withLabels(webLabels), withOwner("ReplicaSet", "web-7f9", uidRS)),
		u("v1", "Service", "prod", "web-svc", "uid-svc", withSpec(map[string]any{"selector": map[string]any{"app": "web"}})),
	}, fakeScope{})
	if err != nil {
		t.Fatalf("BuildObjects: %v", err)
	}

	state := closure.NewScanState(objs)
	action := closure.Action{
		Verb:    closure.Delete,
		Target:  closure.Ref{GVK: closure.GVK{Group: "apps", Version: "v1", Kind: "Deployment"}, Namespace: "prod", Name: "web", UID: uidDep},
		Cascade: true,
	}
	scope := []closure.ScopeClause{
		closure.ResourceClause(closure.GVK{Version: "v1", Kind: "Pod"}, "prod", "web-1"),
	}

	got := closure.Safe(state, action, scope)

	exp := loadGolden01(t)
	if got.Verdict.String() != exp.Verdict {
		t.Errorf("verdict = %s, want %s", got.Verdict, exp.Verdict)
	}
	assertSet(t, "closure", got.Closure, exp.Closure)
	assertSet(t, "escaping", got.Escaping, exp.Escaping)
	assertSet(t, "external", got.External, exp.External)
}

func loadGolden01(t *testing.T) expectedVerdict {
	t.Helper()
	path := filepath.Join("..", "..", "closure", "testdata", "scenarios", "01-memory-pressure-cascade", "expected.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden expected.yaml: %v", err)
	}
	var exp expectedVerdict
	if err := yaml.Unmarshal(b, &exp); err != nil {
		t.Fatalf("parse golden: %v", err)
	}
	return exp
}

func assertSet(t *testing.T, label string, got []closure.Ref, want []humanRef) {
	t.Helper()
	gotKeys := make([]string, 0, len(got))
	for _, r := range got {
		gotKeys = append(gotKeys, r.String())
	}
	wantKeys := make([]string, 0, len(want))
	for _, w := range want {
		wantKeys = append(wantKeys, w.key())
	}
	sort.Strings(gotKeys)
	sort.Strings(wantKeys)
	if len(gotKeys) != len(wantKeys) {
		t.Errorf("%s set size mismatch\n got: %v\nwant: %v", label, gotKeys, wantKeys)
		return
	}
	for i := range gotKeys {
		if gotKeys[i] != wantKeys[i] {
			t.Errorf("%s set mismatch\n got: %v\nwant: %v", label, gotKeys, wantKeys)
			return
		}
	}
}
