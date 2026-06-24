package cluster

import (
	"testing"

	"sigs.k8s.io/yaml"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ridik-il/krsm/closure"
)

// This file hardens BuildObjects against malformed and adversarial unstructured input.
// Two invariants are asserted: (1) fail-closed — the builder returns an error (never a
// silent half-result) on the malformed shapes the loader rejects at its parse boundary;
// (2) no-panic — arbitrary, deeply-malformed maps never panic, only error or skip.

// --- fail-closed: malformed objects the builder must reject -------------------

func TestBuildObjectsFailClosed(t *testing.T) {
	cases := []struct {
		name string
		objs []unstructured.Unstructured
	}{
		{
			name: "missing kind",
			objs: []unstructured.Unstructured{{Object: map[string]any{
				"apiVersion": "v1",
				"metadata":   map[string]any{"name": "x"},
			}}},
		},
		{
			name: "missing metadata.name",
			objs: []unstructured.Unstructured{u("v1", "Pod", "prod", "", "uid")},
		},
		{
			name: "no metadata at all",
			objs: []unstructured.Unstructured{{Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "Pod",
			}}},
		},
		{
			name: "invalid matchExpressions operator (workload)",
			objs: []unstructured.Unstructured{
				u("apps/v1", "Deployment", "prod", "web", "uid", withSpec(map[string]any{
					"selector": map[string]any{"matchExpressions": []any{
						map[string]any{"key": "tier", "operator": "Contains", "values": []any{"web"}},
					}},
				})),
			},
		},
		{
			name: "invalid matchExpressions operator (NetworkPolicy podSelector)",
			objs: []unstructured.Unstructured{
				u("networking.k8s.io/v1", "NetworkPolicy", "prod", "np", "uid", withSpec(map[string]any{
					"podSelector": map[string]any{"matchExpressions": []any{
						map[string]any{"key": "tier", "operator": "Exist"},
					}},
				})),
			},
		},
		{
			name: "empty operator string is invalid",
			objs: []unstructured.Unstructured{
				u("apps/v1", "StatefulSet", "prod", "db", "uid", withSpec(map[string]any{
					"selector": map[string]any{"matchExpressions": []any{
						map[string]any{"key": "tier", "operator": ""},
					}},
				})),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BuildObjects(tc.objs, fakeScope{}); err == nil {
				t.Errorf("BuildObjects(%s) = nil error, want fail-closed error", tc.name)
			}
		})
	}
}

// TestBuildObjectsOwnerRefMissingUID: an ownerReferences entry that omits uid must not
// fabricate one (no uidOf synthesis in the live path) — the owner edge carries UID="" and
// the engine falls back to no-uid behaviour rather than matching a wrong parent. This is
// not an error (a partial ownerReference is still a structurally valid object), but it
// must not panic and must not invent a uid.
func TestBuildObjectsOwnerRefMissingUID(t *testing.T) {
	objs, err := BuildObjects([]unstructured.Unstructured{
		u("apps/v1", "ReplicaSet", "prod", "rs", "uid-rs"),
		u("v1", "Pod", "prod", "p", "uid-p", func(obj map[string]any) {
			meta := obj["metadata"].(map[string]any)
			meta["ownerReferences"] = []any{map[string]any{"kind": "ReplicaSet", "name": "rs"}} // no uid
		}),
	}, fakeScope{})
	if err != nil {
		t.Fatalf("BuildObjects: %v", err)
	}
	pod, ok := findObj(objs, "Pod", "p")
	if !ok {
		t.Fatal("pod not built")
	}
	if len(pod.Owners) != 1 || pod.Owners[0].UID != "" {
		t.Fatalf("owner = %+v, want one owner with empty UID (no synthesis)", pod.Owners)
	}
	// The engine's uid-keyed OwnedChildren must NOT match a uid-less owner to its parent
	// (matching requires a non-empty owner uid), so no false owner edge is created.
	state := closure.NewScanState(objs)
	rs := closure.Ref{GVK: closure.GVK{Group: "apps", Version: "v1", Kind: "ReplicaSet"}, Namespace: "prod", Name: "rs", UID: "uid-rs"}
	if children := state.OwnedChildren(rs); len(children) != 0 {
		t.Errorf("OwnedChildren over a uid-less ownerReference = %v, want none (no synthesis, no false edge)", children)
	}
}

// --- namespace / scope resolution edges ---------------------------------------

func TestBuildObjectsClusterScopedNamespaceCleared(t *testing.T) {
	scope := fakeScope{clusterScoped: map[string]bool{"ClusterRole": true}}
	objs, err := BuildObjects([]unstructured.Unstructured{
		// a metadata.namespace on a cluster-scoped kind is ignored → "".
		u("rbac.authorization.k8s.io/v1", "ClusterRole", "ignored-ns", "admin", "uid-cr"),
	}, scope)
	if err != nil {
		t.Fatalf("BuildObjects: %v", err)
	}
	cr, _ := findObj(objs, "ClusterRole", "admin")
	if cr.Ref.Namespace != "" {
		t.Errorf("cluster-scoped namespace = %q, want \"\"", cr.Ref.Namespace)
	}
}

func TestBuildObjectsUnknownScopeDefaultsNamespaced(t *testing.T) {
	// An unknown-scope GVK (ScopeInfo returns ok=false) is treated as namespaced by the
	// pure builder; the slice-4 caller fails closed on unknown scope. Here it must
	// default to the metadata.namespace (not ""), so it is still counted in containment.
	scope := fakeScope{unknown: map[string]bool{"WidgetCRD": true}}
	objs, err := BuildObjects([]unstructured.Unstructured{
		u("example.com/v1", "WidgetCRD", "prod", "w", "uid-w"),
	}, scope)
	if err != nil {
		t.Fatalf("BuildObjects: %v", err)
	}
	w, _ := findObj(objs, "WidgetCRD", "w")
	if w.Ref.Namespace != "prod" {
		t.Errorf("unknown-scope namespace = %q, want \"prod\" (default namespaced)", w.Ref.Namespace)
	}
}

func TestBuildObjectsNamespacedEmptyDefaultsToDefault(t *testing.T) {
	// A namespaced object with no metadata.namespace defaults to "default", matching the
	// loader's nsOf so the parity oracle holds.
	objs, err := BuildObjects([]unstructured.Unstructured{
		u("v1", "Pod", "", "p", "uid-p"),
	}, fakeScope{})
	if err != nil {
		t.Fatalf("BuildObjects: %v", err)
	}
	p, _ := findObj(objs, "Pod", "p")
	if p.Ref.Namespace != "default" {
		t.Errorf("namespaced object with empty ns = %q, want \"default\"", p.Ref.Namespace)
	}
}

// TestBuildObjectsPresentEmptySelectorBindsAll: a present-empty workload/PDB selector
// (spec.selector: {}) must round-trip through the unstructured extractor to a non-nil
// empty map, so the kind-aware engine binds all pods in the namespace — distinct from an
// ABSENT selector (nil) which binds nothing. This is the nil-vs-present-empty invariant.
func TestBuildObjectsPresentEmptyVsAbsentSelector(t *testing.T) {
	// present-empty selector on a PDB → binds all pods in ns.
	present, err := BuildObjects([]unstructured.Unstructured{
		u("policy/v1", "PodDisruptionBudget", "prod", "pdb", "uid-pdb", withSpec(map[string]any{
			"selector": map[string]any{},
		})),
		u("v1", "Pod", "prod", "a", "uid-a", withLabels(map[string]any{"x": "y"})),
		u("v1", "Pod", "prod", "b", "uid-b"),
	}, fakeScope{})
	if err != nil {
		t.Fatalf("BuildObjects: %v", err)
	}
	ps := closure.NewScanState(present)
	pdb := closure.Ref{GVK: closure.GVK{Group: "policy", Version: "v1", Kind: "PodDisruptionBudget"}, Namespace: "prod", Name: "pdb", UID: "uid-pdb"}
	if pods := ps.PodsSelectedBy(pdb); len(pods) != 2 {
		t.Errorf("present-empty PDB selector binds %d pods, want 2 (all)", len(pods))
	}

	// absent selector on a PDB → binds nothing.
	absent, err := BuildObjects([]unstructured.Unstructured{
		u("policy/v1", "PodDisruptionBudget", "prod", "pdb", "uid-pdb", withSpec(map[string]any{})),
		u("v1", "Pod", "prod", "a", "uid-a"),
	}, fakeScope{})
	if err != nil {
		t.Fatalf("BuildObjects: %v", err)
	}
	as := closure.NewScanState(absent)
	if pods := as.PodsSelectedBy(pdb); len(pods) != 0 {
		t.Errorf("absent PDB selector binds %v, want nothing", pods)
	}
}

// --- no-panic: arbitrary garbage must error or skip, never panic ---------------

// TestBuildObjectsNeverPanics feeds a table of deeply-malformed objects — wrong types at
// every documented field path (non-map metadata, non-slice owners/volumes/containers,
// non-map entries inside those slices, wrong-typed selector wrappers, etc.). The builder
// must return (a value, maybe error) without panicking. Wrong-typed scalar fields are
// silently coerced to "" by the Nested* readers (matching the loader's typed-decode
// tolerance); the only hard errors are missing kind/name and an invalid operator.
func TestBuildObjectsNeverPanics(t *testing.T) {
	garbage := []map[string]any{
		{},              // wholly empty
		{"kind": "Pod"}, // no metadata
		{"kind": "Pod", "metadata": "not-a-map", "apiVersion": "v1"},
		{"kind": "Pod", "metadata": map[string]any{"name": "p"}, "spec": "not-a-map"},
		{"kind": "Pod", "metadata": map[string]any{"name": "p", "labels": "not-a-map"}, "apiVersion": "v1"},
		{"kind": "Pod", "metadata": map[string]any{"name": "p", "finalizers": "not-a-slice"}, "apiVersion": "v1"},
		{"kind": "Pod", "metadata": map[string]any{"name": "p", "ownerReferences": "not-a-slice"}, "apiVersion": "v1"},
		{"kind": "Pod", "metadata": map[string]any{"name": "p", "ownerReferences": []any{"not-a-map", 42, nil}}, "apiVersion": "v1"},
		{"kind": "Pod", "metadata": map[string]any{"name": "p"}, "apiVersion": "v1",
			"spec": map[string]any{"volumes": "not-a-slice"}},
		{"kind": "Pod", "metadata": map[string]any{"name": "p"}, "apiVersion": "v1",
			"spec": map[string]any{"volumes": []any{"not-a-map", 7, nil, map[string]any{"configMap": "not-a-map"}}}},
		{"kind": "Pod", "metadata": map[string]any{"name": "p"}, "apiVersion": "v1",
			"spec": map[string]any{"containers": []any{"not-a-map", nil, map[string]any{"env": "not-a-slice", "envFrom": 3}}}},
		{"kind": "Pod", "metadata": map[string]any{"name": "p"}, "apiVersion": "v1",
			"spec": map[string]any{"containers": []any{map[string]any{
				"env": []any{"not-a-map", map[string]any{"valueFrom": "not-a-map"}, map[string]any{"valueFrom": map[string]any{"configMapKeyRef": 9}}},
			}}}},
		{"kind": "Pod", "metadata": map[string]any{"name": "p"}, "apiVersion": "v1",
			"spec": map[string]any{"imagePullSecrets": []any{"not-a-map", nil, map[string]any{"name": 5}}}},
		{"kind": "Pod", "metadata": map[string]any{"name": "p"}, "apiVersion": "v1",
			"spec": map[string]any{"volumes": []any{map[string]any{"projected": map[string]any{"sources": "not-a-slice"}}}}},
		{"kind": "Pod", "metadata": map[string]any{"name": "p"}, "apiVersion": "v1",
			"spec": map[string]any{"volumes": []any{map[string]any{"projected": map[string]any{"sources": []any{"not-a-map", nil, 3}}}}}},
		{"kind": "Service", "metadata": map[string]any{"name": "s"}, "apiVersion": "v1",
			"spec": map[string]any{"selector": "not-a-map"}},
		{"kind": "Deployment", "metadata": map[string]any{"name": "d"}, "apiVersion": "apps/v1",
			"spec": map[string]any{"selector": map[string]any{"matchLabels": "not-a-map"}}},
		{"kind": "Deployment", "metadata": map[string]any{"name": "d"}, "apiVersion": "apps/v1",
			"spec": map[string]any{"selector": map[string]any{"matchExpressions": "not-a-slice"}}},
		{"kind": "Deployment", "metadata": map[string]any{"name": "d"}, "apiVersion": "apps/v1",
			"spec": map[string]any{"selector": map[string]any{"matchExpressions": []any{"not-a-map", nil, 3}}}},
		{"kind": "Deployment", "metadata": map[string]any{"name": "d"}, "apiVersion": "apps/v1",
			"spec": map[string]any{"template": "not-a-map"}},
		{"kind": "Deployment", "metadata": map[string]any{"name": "d"}, "apiVersion": "apps/v1",
			"spec": map[string]any{"template": map[string]any{"spec": "not-a-map"}}},
		{"kind": "HorizontalPodAutoscaler", "metadata": map[string]any{"name": "h"}, "apiVersion": "autoscaling/v2",
			"spec": map[string]any{"scaleTargetRef": "not-a-map"}},
		{"kind": "Pod", "metadata": map[string]any{"name": "p", "namespace": 5}, "apiVersion": "v1"},
		{"kind": 7, "metadata": map[string]any{"name": "p"}, "apiVersion": "v1"},   // non-string kind
		{"kind": "Pod", "metadata": map[string]any{"name": 7}, "apiVersion": "v1"}, // non-string name
		{"kind": "Pod", "metadata": map[string]any{"name": "p"}, "apiVersion": []any{"weird"}},
	}
	for i, g := range garbage {
		g := g
		// Each garbage object alone, and mixed with a valid neighbour, must not panic.
		t.Run("", func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("garbage[%d] panicked: %v\ninput: %#v", i, r, g)
				}
			}()
			_, _ = BuildObjects([]unstructured.Unstructured{{Object: g}}, fakeScope{})
			_, _ = BuildObjects([]unstructured.Unstructured{
				u("v1", "Pod", "prod", "valid", "uid-v"),
				{Object: g},
			}, fakeScope{})
		})
	}
}

// TestBuildObjectsNilAndEmptyInput: nil/empty input is a valid empty cluster, not an
// error or panic.
func TestBuildObjectsNilAndEmptyInput(t *testing.T) {
	for _, in := range [][]unstructured.Unstructured{nil, {}} {
		objs, err := BuildObjects(in, fakeScope{})
		if err != nil {
			t.Errorf("BuildObjects(%v) = error %v, want nil", in, err)
		}
		if len(objs) != 0 {
			t.Errorf("BuildObjects(%v) = %d objects, want 0", in, len(objs))
		}
	}
}

// FuzzBuildObjects proves BuildObjects never panics on arbitrary input: the fuzzer
// feeds random bytes, decoded as YAML into an unstructured map, straight into the
// builder. A real cluster read only ever yields JSON-shaped values, but the builder's
// contract is "error or skip, never panic" on ANY map — this exercises that across
// machine-generated shapes the hand-written table cannot enumerate. A decode failure or
// non-map document is simply skipped (not interesting input); the assertion is solely
// the absence of a panic.
func FuzzBuildObjects(f *testing.F) {
	seeds := []string{
		"kind: Pod\nmetadata: {name: p, namespace: prod}\n",
		"kind: Deployment\napiVersion: apps/v1\nmetadata: {name: d}\nspec:\n  selector:\n    matchExpressions:\n      - {key: a, operator: In, values: [x]}\n",
		"kind: Service\napiVersion: v1\nmetadata: {name: s}\nspec: {selector: {app: web}}\n",
		"{}",
		"[]",
		"kind: 7\n",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var m map[string]any
		if err := yaml.Unmarshal(data, &m); err != nil || m == nil {
			return // not a YAML object: nothing to build
		}
		// Must not panic regardless of what the random map contains; surface any
		// panic through t so the crashing input is recorded as a fuzz failure.
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("BuildObjects panicked on fuzzed input: %v\ninput: %#v", r, m)
			}
		}()
		_, _ = BuildObjects([]unstructured.Unstructured{{Object: m}}, fakeScope{})
	})
}
