package cluster

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ridik-il/krsm/closure"
)

// corpusShapedObjects exercises every relation the shared projection extracts: an owner
// edge (real uid), workload + Service selectors, a template-volume cross-ref to a LISTED
// ConfigMap and to a MISSING one, an imagePullSecret, and finalizers. It is the common
// input for the project↔BuildObjects parity assertions below.
func corpusShapedObjects() []unstructured.Unstructured {
	web := map[string]any{"app": "web"}
	return []unstructured.Unstructured{
		u("apps/v1", "Deployment", "prod", "web", "uid-dep",
			withLabels(web),
			withSpec(map[string]any{"selector": map[string]any{"matchLabels": web}}),
			withTemplateSpec(map[string]any{
				"volumes": []any{
					map[string]any{"name": "a", "configMap": map[string]any{"name": "cfg"}},
					map[string]any{"name": "b", "configMap": map[string]any{"name": "missing"}},
				},
				"imagePullSecrets": []any{map[string]any{"name": "regcred"}},
			}),
		),
		u("apps/v1", "ReplicaSet", "prod", "web-7f9", "uid-rs",
			withLabels(web), withOwner("Deployment", "web", "uid-dep")),
		u("v1", "Pod", "prod", "web-1", "uid-p1", withLabels(web), withOwner("ReplicaSet", "web-7f9", "uid-rs")),
		u("v1", "Service", "prod", "web-svc", "uid-svc", withSpec(map[string]any{"selector": web})),
		u("v1", "ConfigMap", "prod", "cfg", "uid-cfg"),
		u("v1", "PersistentVolumeClaim", "prod", "data", "uid-pvc", func(obj map[string]any) {
			meta := obj["metadata"].(map[string]any)
			meta["finalizers"] = []any{"example.com/guard"}
		}),
	}
}

// TestProjectMatchesBuildObjectsExceptCrossRefUID is the slice-2 parity assertion: the
// shared, INDEX-FREE project() agrees with BuildObjects on every relation EXCEPT the
// cross-ref referent uid, which BuildObjects fills via its one-shot post-pass and project
// leaves empty (the engine matches those by Kind/ns/name regardless). This is the
// guarantee that the informer index (slice 3, which calls bare project) stays faithful to
// the one-shot reader and therefore to the goldens.
func TestProjectMatchesBuildObjectsExceptCrossRefUID(t *testing.T) {
	in := corpusShapedObjects()
	built, err := BuildObjects(in, fakeScope{})
	if err != nil {
		t.Fatalf("BuildObjects: %v", err)
	}

	for i := range in {
		p, err := project(in[i], fakeScope{})
		if err != nil {
			t.Fatalf("project(%d): %v", i, err)
		}
		b, ok := findObj(built, p.Ref.GVK.Kind, p.Ref.Name)
		if !ok {
			t.Fatalf("BuildObjects produced no object for %s", p.Ref.String())
		}

		if p.Ref != b.Ref {
			t.Errorf("%s: Ref mismatch project=%+v build=%+v", p.Ref.String(), p.Ref, b.Ref)
		}
		assertStringMapEqual(t, p.Ref.String()+" labels", p.Labels, b.Labels)
		assertStringSliceEqual(t, p.Ref.String()+" finalizers", p.Finalizers, b.Finalizers)
		if len(p.Owners) != len(b.Owners) {
			t.Errorf("%s: owners len project=%d build=%d", p.Ref.String(), len(p.Owners), len(b.Owners))
		} else {
			for k := range p.Owners {
				if p.Owners[k] != b.Owners[k] {
					t.Errorf("%s: owner[%d] project=%+v build=%+v", p.Ref.String(), k, p.Owners[k], b.Owners[k])
				}
			}
		}
		// Cross-refs must agree on Kind + referent Kind/ns/name; project's uid is EMPTY,
		// BuildObjects' may be resolved. So compare with uid zeroed on both sides.
		if len(p.Owners) >= 0 && len(p.CrossRefs) != len(b.CrossRefs) {
			t.Errorf("%s: crossref len project=%d build=%d", p.Ref.String(), len(p.CrossRefs), len(b.CrossRefs))
		}
		for k := range p.CrossRefs {
			if p.CrossRefs[k].Ref.UID != "" {
				t.Errorf("%s: project cross-ref %d carries a uid %q, want empty (index-free)", p.Ref.String(), k, p.CrossRefs[k].Ref.UID)
			}
			pc := p.CrossRefs[k]
			bc := b.CrossRefs[k]
			pc.Ref.UID, bc.Ref.UID = "", ""
			if pc != bc {
				t.Errorf("%s: cross-ref %d (uid-zeroed) project=%+v build=%+v", p.Ref.String(), k, pc, bc)
			}
		}
	}
}

// TestProjectAndBuildObjectsYieldSameClosure proves the representational difference
// (empty vs resolved cross-ref uid) is closure-invisible: a State assembled purely from
// project() and one from BuildObjects produce the IDENTICAL closure/escaping/external set
// for the flagship cascade action.
func TestProjectAndBuildObjectsYieldSameClosure(t *testing.T) {
	in := corpusShapedObjects()

	built, err := BuildObjects(in, fakeScope{})
	if err != nil {
		t.Fatalf("BuildObjects: %v", err)
	}
	projected := make([]closure.Object, 0, len(in))
	for i := range in {
		o, err := project(in[i], fakeScope{})
		if err != nil {
			t.Fatalf("project: %v", err)
		}
		projected = append(projected, o)
	}

	action := closure.Action{
		Verb:    closure.Delete,
		Target:  closure.Ref{GVK: closure.GVK{Group: "apps", Version: "v1", Kind: "Deployment"}, Namespace: "prod", Name: "web", UID: "uid-dep"},
		Cascade: true,
	}
	scope := []closure.ScopeClause{
		closure.ResourceClause(closure.GVK{Version: "v1", Kind: "Pod"}, "prod", "web-1"),
	}

	gotBuilt := closure.Safe(closure.NewScanState(built), action, scope)
	gotProj := closure.Safe(closure.NewScanState(projected), action, scope)

	if gotBuilt.Verdict != gotProj.Verdict {
		t.Errorf("verdict: build=%s project=%s", gotBuilt.Verdict, gotProj.Verdict)
	}
	assertRefSetsEqual(t, "closure", gotBuilt.Closure, gotProj.Closure)
	assertRefSetsEqual(t, "escaping", gotBuilt.Escaping, gotProj.Escaping)
	assertRefSetsEqual(t, "external", gotBuilt.External, gotProj.External)
}

func assertStringMapEqual(t *testing.T, label string, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: len got=%d want=%d", label, len(got), len(want))
		return
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: key %q got=%q want=%q", label, k, got[k], v)
		}
	}
}

func assertStringSliceEqual(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: len got=%v want=%v", label, got, want)
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s: [%d] got=%q want=%q", label, i, got[i], want[i])
		}
	}
}

func assertRefSetsEqual(t *testing.T, label string, got, want []closure.Ref) {
	t.Helper()
	gm := map[string]bool{}
	for _, r := range got {
		gm[r.String()] = true
	}
	wm := map[string]bool{}
	for _, r := range want {
		wm[r.String()] = true
	}
	if len(gm) != len(wm) {
		t.Errorf("%s: size got=%d want=%d (%v vs %v)", label, len(gm), len(wm), got, want)
		return
	}
	for k := range wm {
		if !gm[k] {
			t.Errorf("%s: missing %s", label, k)
		}
	}
}
