package cluster

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ridik-il/krsm/closure"
)

// fakeScope is a hand-built ScopeInfo for tests: kinds listed as cluster-scoped
// return (false, true); every other kind defaults to namespaced (true, true).
// Unknown-scope (ok=false) is exercised explicitly where needed.
type fakeScope struct {
	clusterScoped map[string]bool
	unknown       map[string]bool
}

func (f fakeScope) Namespaced(gvk closure.GVK) (bool, bool) {
	if f.unknown[gvk.Kind] {
		return false, false
	}
	if f.clusterScoped[gvk.Kind] {
		return false, true
	}
	return true, true
}

// u builds an unstructured object with the given apiVersion/kind/namespace/name/uid
// and an optional spec/metadata extension applied by mutators.
func u(apiVersion, kind, ns, name, uid string, mut ...func(map[string]any)) unstructured.Unstructured {
	meta := map[string]any{"name": name}
	if ns != "" {
		meta["namespace"] = ns
	}
	if uid != "" {
		meta["uid"] = uid
	}
	obj := map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   meta,
	}
	for _, m := range mut {
		m(obj)
	}
	return unstructured.Unstructured{Object: obj}
}

func findObj(objs []closure.Object, kind, name string) (closure.Object, bool) {
	for _, o := range objs {
		if o.Ref.GVK.Kind == kind && o.Ref.Name == name {
			return o, true
		}
	}
	return closure.Object{}, false
}

// withOwner adds an ownerReferences entry carrying a REAL uid.
func withOwner(kind, name, uid string) func(map[string]any) {
	return func(obj map[string]any) {
		meta := obj["metadata"].(map[string]any)
		owners, _ := meta["ownerReferences"].([]any)
		meta["ownerReferences"] = append(owners, map[string]any{
			"kind": kind, "name": name, "uid": uid,
		})
	}
}

func TestBuildObjectsOwnerEdgeByRealUID(t *testing.T) {
	objs, err := BuildObjects([]unstructured.Unstructured{
		u("apps/v1", "ReplicaSet", "prod", "web-7f9", "uid-rs"),
		u("v1", "Pod", "prod", "web-1", "uid-p1", withOwner("ReplicaSet", "web-7f9", "uid-rs")),
	}, fakeScope{})
	if err != nil {
		t.Fatalf("BuildObjects: %v", err)
	}

	pod, ok := findObj(objs, "Pod", "web-1")
	if !ok {
		t.Fatal("pod web-1 not built")
	}
	if len(pod.Owners) != 1 || pod.Owners[0].UID != "uid-rs" {
		t.Fatalf("pod owners = %+v, want one owner with UID uid-rs", pod.Owners)
	}

	// Observable through the engine: OwnedChildren matches the child by REAL uid.
	state := closure.NewScanState(objs)
	rs := closure.Ref{GVK: closure.GVK{Group: "apps", Version: "v1", Kind: "ReplicaSet"}, Namespace: "prod", Name: "web-7f9", UID: "uid-rs"}
	children := state.OwnedChildren(rs)
	if len(children) != 1 || children[0].Name != "web-1" {
		t.Errorf("OwnedChildren(web-7f9) = %v, want [web-1]", children)
	}
}

func TestBuildObjectsNamespaceContainment(t *testing.T) {
	scope := fakeScope{clusterScoped: map[string]bool{"PersistentVolume": true}}
	objs, err := BuildObjects([]unstructured.Unstructured{
		u("v1", "Pod", "prod", "web-1", "uid-p1"),
		// cluster-scoped: a metadata.namespace is ignored, resolves to "".
		u("v1", "PersistentVolume", "prod", "pv-1", "uid-pv"),
	}, scope)
	if err != nil {
		t.Fatalf("BuildObjects: %v", err)
	}

	pv, ok := findObj(objs, "PersistentVolume", "pv-1")
	if !ok {
		t.Fatal("pv-1 not built")
	}
	if pv.Ref.Namespace != "" {
		t.Errorf("cluster-scoped PV namespace = %q, want \"\"", pv.Ref.Namespace)
	}

	// Observable: the namespaced pod is in the namespace contents; the cluster-scoped
	// PV is not.
	state := closure.NewScanState(objs)
	contents := state.NamespaceContents("prod")
	if len(contents) != 1 || contents[0].Name != "web-1" {
		t.Errorf("NamespaceContents(prod) = %v, want [web-1] (PV must not appear)", contents)
	}
}

// withSpec merges fields into spec.
func withSpec(spec map[string]any) func(map[string]any) {
	return func(obj map[string]any) {
		cur, _ := obj["spec"].(map[string]any)
		if cur == nil {
			cur = map[string]any{}
		}
		for k, v := range spec {
			cur[k] = v
		}
		obj["spec"] = cur
	}
}

// withLabels sets metadata.labels.
func withLabels(labels map[string]any) func(map[string]any) {
	return func(obj map[string]any) {
		meta := obj["metadata"].(map[string]any)
		meta["labels"] = labels
	}
}

func TestBuildObjectsServiceSelectorBinding(t *testing.T) {
	objs, err := BuildObjects([]unstructured.Unstructured{
		u("v1", "Service", "prod", "web-svc", "uid-svc", withSpec(map[string]any{
			"selector": map[string]any{"app": "web"},
		})),
		u("v1", "Pod", "prod", "web-1", "uid-p1", withLabels(map[string]any{"app": "web"})),
		u("v1", "Pod", "prod", "other", "uid-p2", withLabels(map[string]any{"app": "db"})),
		// absent selector → binds nothing.
		u("v1", "Service", "prod", "headless", "uid-h", withSpec(map[string]any{})),
	}, fakeScope{})
	if err != nil {
		t.Fatalf("BuildObjects: %v", err)
	}

	state := closure.NewScanState(objs)
	svc := closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Service"}, Namespace: "prod", Name: "web-svc", UID: "uid-svc"}
	pods := state.PodsSelectedBy(svc)
	if len(pods) != 1 || pods[0].Name != "web-1" {
		t.Errorf("PodsSelectedBy(web-svc) = %v, want [web-1]", pods)
	}

	headless := closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Service"}, Namespace: "prod", Name: "headless", UID: "uid-h"}
	if pods := state.PodsSelectedBy(headless); len(pods) != 0 {
		t.Errorf("absent-selector Service binds %v, want nothing", pods)
	}

	// A present-empty Service selector binds nothing (Service rule).
	emptySel, err := BuildObjects([]unstructured.Unstructured{
		u("v1", "Service", "prod", "empty", "uid-e", withSpec(map[string]any{"selector": map[string]any{}})),
		u("v1", "Pod", "prod", "p", "uid-p", withLabels(map[string]any{"app": "web"})),
	}, fakeScope{})
	if err != nil {
		t.Fatalf("BuildObjects: %v", err)
	}
	es := closure.NewScanState(emptySel)
	er := closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Service"}, Namespace: "prod", Name: "empty", UID: "uid-e"}
	if pods := es.PodsSelectedBy(er); len(pods) != 0 {
		t.Errorf("present-empty Service selector binds %v, want nothing", pods)
	}
}

func TestBuildObjectsNetworkPolicyPodSelector(t *testing.T) {
	objs, err := BuildObjects([]unstructured.Unstructured{
		u("networking.k8s.io/v1", "NetworkPolicy", "prod", "np", "uid-np", withSpec(map[string]any{
			"podSelector": map[string]any{"matchLabels": map[string]any{"app": "web"}},
		})),
		u("v1", "Pod", "prod", "web-1", "uid-p1", withLabels(map[string]any{"app": "web"})),
		u("v1", "Pod", "prod", "db-1", "uid-p2", withLabels(map[string]any{"app": "db"})),
	}, fakeScope{})
	if err != nil {
		t.Fatalf("BuildObjects: %v", err)
	}
	state := closure.NewScanState(objs)
	np := closure.Ref{GVK: closure.GVK{Group: "networking.k8s.io", Version: "v1", Kind: "NetworkPolicy"}, Namespace: "prod", Name: "np", UID: "uid-np"}
	pods := state.PodsSelectedBy(np)
	if len(pods) != 1 || pods[0].Name != "web-1" {
		t.Errorf("PodsSelectedBy(np) = %v, want [web-1]", pods)
	}

	// present-empty podSelector: {} binds ALL pods in ns (corpus #8).
	all, err := BuildObjects([]unstructured.Unstructured{
		u("networking.k8s.io/v1", "NetworkPolicy", "prod", "deny-all", "uid-da", withSpec(map[string]any{
			"podSelector": map[string]any{},
		})),
		u("v1", "Pod", "prod", "web-1", "uid-p1", withLabels(map[string]any{"app": "web"})),
		u("v1", "Pod", "prod", "db-1", "uid-p2", withLabels(map[string]any{"app": "db"})),
	}, fakeScope{})
	if err != nil {
		t.Fatalf("BuildObjects: %v", err)
	}
	as := closure.NewScanState(all)
	da := closure.Ref{GVK: closure.GVK{Group: "networking.k8s.io", Version: "v1", Kind: "NetworkPolicy"}, Namespace: "prod", Name: "deny-all", UID: "uid-da"}
	if pods := as.PodsSelectedBy(da); len(pods) != 2 {
		t.Errorf("present-empty podSelector binds %d pods, want 2 (all)", len(pods))
	}
}

func TestBuildObjectsMatchExpressions(t *testing.T) {
	objs, err := BuildObjects([]unstructured.Unstructured{
		// PDB selecting pods where tier In (web), and env DoesNotExist.
		u("policy/v1", "PodDisruptionBudget", "prod", "pdb", "uid-pdb", withSpec(map[string]any{
			"selector": map[string]any{
				"matchExpressions": []any{
					map[string]any{"key": "tier", "operator": "In", "values": []any{"web"}},
					map[string]any{"key": "env", "operator": "DoesNotExist"},
				},
			},
		})),
		u("v1", "Pod", "prod", "match", "uid-m", withLabels(map[string]any{"tier": "web"})),
		u("v1", "Pod", "prod", "wrong-tier", "uid-w", withLabels(map[string]any{"tier": "db"})),
		u("v1", "Pod", "prod", "has-env", "uid-e", withLabels(map[string]any{"tier": "web", "env": "prod"})),
	}, fakeScope{})
	if err != nil {
		t.Fatalf("BuildObjects: %v", err)
	}
	state := closure.NewScanState(objs)
	pdb := closure.Ref{GVK: closure.GVK{Group: "policy", Version: "v1", Kind: "PodDisruptionBudget"}, Namespace: "prod", Name: "pdb", UID: "uid-pdb"}
	pods := state.PodsSelectedBy(pdb)
	if len(pods) != 1 || pods[0].Name != "match" {
		t.Errorf("PodsSelectedBy(pdb) = %v, want [match] (In tier=web AND env DoesNotExist)", pods)
	}

	// An unrecognised operator fails closed at the build boundary.
	_, err = BuildObjects([]unstructured.Unstructured{
		u("policy/v1", "PodDisruptionBudget", "prod", "bad", "uid-b", withSpec(map[string]any{
			"selector": map[string]any{
				"matchExpressions": []any{
					map[string]any{"key": "tier", "operator": "Contains", "values": []any{"web"}},
				},
			},
		})),
	}, fakeScope{})
	if err == nil {
		t.Fatal("BuildObjects with invalid operator = nil error, want fail-closed error")
	}
}

// withTemplateSpec sets spec.template.spec (the workload pod template).
func withTemplateSpec(podSpec map[string]any) func(map[string]any) {
	return withSpec(map[string]any{
		"template": map[string]any{"spec": podSpec},
	})
}

func TestBuildObjectsTemplateVolumeCrossRefs(t *testing.T) {
	objs, err := BuildObjects([]unstructured.Unstructured{
		u("apps/v1", "Deployment", "prod", "web", "uid-web", withTemplateSpec(map[string]any{
			"volumes": []any{
				map[string]any{"name": "c", "configMap": map[string]any{"name": "cfg"}},
				map[string]any{"name": "s", "secret": map[string]any{"secretName": "sec"}},
				map[string]any{"name": "d", "persistentVolumeClaim": map[string]any{"claimName": "data"}},
				map[string]any{"name": "p", "projected": map[string]any{"sources": []any{
					map[string]any{"configMap": map[string]any{"name": "pcfg"}},
					map[string]any{"secret": map[string]any{"name": "psec"}},
				}}},
			},
		})),
		u("v1", "ConfigMap", "prod", "cfg", "uid-cfg"),
		u("v1", "Secret", "prod", "sec", "uid-sec"),
		u("v1", "PersistentVolumeClaim", "prod", "data", "uid-data"),
		u("v1", "ConfigMap", "prod", "pcfg", "uid-pcfg"),
		u("v1", "Secret", "prod", "psec", "uid-psec"),
	}, fakeScope{})
	if err != nil {
		t.Fatalf("BuildObjects: %v", err)
	}
	state := closure.NewScanState(objs)
	for _, tc := range []struct{ kind, name, uid string }{
		{"ConfigMap", "cfg", "uid-cfg"},
		{"Secret", "sec", "uid-sec"},
		{"PersistentVolumeClaim", "data", "uid-data"},
		{"ConfigMap", "pcfg", "uid-pcfg"},
		{"Secret", "psec", "uid-psec"},
	} {
		ref := closure.Ref{GVK: closure.GVK{Version: "v1", Kind: tc.kind}, Namespace: "prod", Name: tc.name, UID: tc.uid}
		cons := state.Consumers(ref)
		if len(cons) != 1 || cons[0].Name != "web" {
			t.Errorf("Consumers(%s/%s) = %v, want [web]", tc.kind, tc.name, cons)
		}
	}
}

func TestBuildObjectsContainerEnvCrossRefs(t *testing.T) {
	objs, err := BuildObjects([]unstructured.Unstructured{
		u("apps/v1", "Deployment", "prod", "web", "uid-web", withTemplateSpec(map[string]any{
			"containers": []any{map[string]any{
				"name":    "app",
				"envFrom": []any{map[string]any{"configMapRef": map[string]any{"name": "cfg"}}},
			}},
			"initContainers": []any{map[string]any{
				"name": "init",
				"env": []any{map[string]any{"valueFrom": map[string]any{
					"secretKeyRef": map[string]any{"name": "sec"},
				}}},
			}},
			"ephemeralContainers": []any{map[string]any{
				"name":    "debug",
				"envFrom": []any{map[string]any{"secretRef": map[string]any{"name": "esec"}}},
			}},
		})),
		u("v1", "ConfigMap", "prod", "cfg", "uid-cfg"),
		u("v1", "Secret", "prod", "sec", "uid-sec"),
		u("v1", "Secret", "prod", "esec", "uid-esec"),
	}, fakeScope{})
	if err != nil {
		t.Fatalf("BuildObjects: %v", err)
	}
	state := closure.NewScanState(objs)
	for _, tc := range []struct{ kind, name, uid string }{
		{"ConfigMap", "cfg", "uid-cfg"},
		{"Secret", "sec", "uid-sec"},
		{"Secret", "esec", "uid-esec"},
	} {
		ref := closure.Ref{GVK: closure.GVK{Version: "v1", Kind: tc.kind}, Namespace: "prod", Name: tc.name, UID: tc.uid}
		if cons := state.Consumers(ref); len(cons) != 1 || cons[0].Name != "web" {
			t.Errorf("Consumers(%s/%s) = %v, want [web]", tc.kind, tc.name, cons)
		}
	}
}

func TestBuildObjectsImagePullSecretAndBarePod(t *testing.T) {
	objs, err := BuildObjects([]unstructured.Unstructured{
		// Bare pod: top-level spec.volumes + spec.imagePullSecrets (no template).
		u("v1", "Pod", "prod", "p", "uid-p", withSpec(map[string]any{
			"volumes":          []any{map[string]any{"name": "c", "configMap": map[string]any{"name": "cfg"}}},
			"imagePullSecrets": []any{map[string]any{"name": "pull"}},
		})),
		u("v1", "ConfigMap", "prod", "cfg", "uid-cfg"),
		u("v1", "Secret", "prod", "pull", "uid-pull"),
	}, fakeScope{})
	if err != nil {
		t.Fatalf("BuildObjects: %v", err)
	}
	state := closure.NewScanState(objs)
	cfg := closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "ConfigMap"}, Namespace: "prod", Name: "cfg", UID: "uid-cfg"}
	if cons := state.Consumers(cfg); len(cons) != 1 || cons[0].Name != "p" {
		t.Errorf("Consumers(cfg) via bare-pod top-level spec = %v, want [p]", cons)
	}
	pull := closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Secret"}, Namespace: "prod", Name: "pull", UID: "uid-pull"}
	if cons := state.Consumers(pull); len(cons) != 1 || cons[0].Name != "p" {
		t.Errorf("Consumers(pull) via imagePullSecrets = %v, want [p]", cons)
	}
}

func TestBuildObjectsScaleTargetRef(t *testing.T) {
	objs, err := BuildObjects([]unstructured.Unstructured{
		u("autoscaling/v2", "HorizontalPodAutoscaler", "prod", "hpa", "uid-hpa", withSpec(map[string]any{
			"scaleTargetRef": map[string]any{"kind": "Deployment", "name": "web"},
		})),
		u("apps/v1", "Deployment", "prod", "web", "uid-web"),
	}, fakeScope{})
	if err != nil {
		t.Fatalf("BuildObjects: %v", err)
	}
	state := closure.NewScanState(objs)
	web := closure.Ref{GVK: closure.GVK{Group: "apps", Version: "v1", Kind: "Deployment"}, Namespace: "prod", Name: "web", UID: "uid-web"}
	ctrls := state.ControllersTargeting(web)
	if len(ctrls) != 1 || ctrls[0].Name != "hpa" {
		t.Errorf("ControllersTargeting(web) = %v, want [hpa]", ctrls)
	}
}

func TestBuildObjectsFinalizers(t *testing.T) {
	objs, err := BuildObjects([]unstructured.Unstructured{
		u("v1", "PersistentVolumeClaim", "prod", "data", "uid-d", func(obj map[string]any) {
			obj["metadata"].(map[string]any)["finalizers"] = []any{"kubernetes.io/pvc-protection", "example.com/guard"}
		}),
	}, fakeScope{})
	if err != nil {
		t.Fatalf("BuildObjects: %v", err)
	}
	o, ok := findObj(objs, "PersistentVolumeClaim", "data")
	if !ok {
		t.Fatal("pvc not built")
	}
	want := []string{"kubernetes.io/pvc-protection", "example.com/guard"}
	if len(o.Finalizers) != 2 || o.Finalizers[0] != want[0] || o.Finalizers[1] != want[1] {
		t.Errorf("Finalizers = %v, want %v", o.Finalizers, want)
	}
}

func TestBuildObjectsCrossRefUIDResolution(t *testing.T) {
	// "cfg" IS listed → real uid; "missing" is NOT listed → empty uid, Kind/ns/name fallback.
	objs, err := BuildObjects([]unstructured.Unstructured{
		u("apps/v1", "Deployment", "prod", "web", "uid-web", withTemplateSpec(map[string]any{
			"volumes": []any{
				map[string]any{"name": "a", "configMap": map[string]any{"name": "cfg"}},
				map[string]any{"name": "b", "configMap": map[string]any{"name": "missing"}},
			},
		})),
		u("v1", "ConfigMap", "prod", "cfg", "uid-cfg"),
	}, fakeScope{})
	if err != nil {
		t.Fatalf("BuildObjects: %v", err)
	}
	dep, _ := findObj(objs, "Deployment", "web")
	var listedUID, missingUID string
	var foundMissing bool
	for _, cr := range dep.CrossRefs {
		switch cr.Ref.Name {
		case "cfg":
			listedUID = cr.Ref.UID
		case "missing":
			missingUID = cr.Ref.UID
			foundMissing = true
		}
	}
	if listedUID != "uid-cfg" {
		t.Errorf("listed referent cfg uid = %q, want uid-cfg", listedUID)
	}
	if !foundMissing || missingUID != "" {
		t.Errorf("unlisted referent missing uid = %q (found=%v), want empty (Kind/ns/name fallback)", missingUID, foundMissing)
	}
}

func TestBuildObjectsMalformedFailsClosed(t *testing.T) {
	if _, err := BuildObjects([]unstructured.Unstructured{
		u("v1", "", "prod", "x", "uid"),
	}, fakeScope{}); err == nil {
		t.Error("BuildObjects with kindless object = nil error, want fail-closed error")
	}
	if _, err := BuildObjects([]unstructured.Unstructured{
		u("v1", "Pod", "prod", "", "uid"),
	}, fakeScope{}); err == nil {
		t.Error("BuildObjects with nameless object = nil error, want fail-closed error")
	}
}

func TestBuildObjectsProjectsIdentity(t *testing.T) {
	objs, err := BuildObjects([]unstructured.Unstructured{
		u("apps/v1", "Deployment", "prod", "web", "uid-web"),
	}, fakeScope{})
	if err != nil {
		t.Fatalf("BuildObjects: %v", err)
	}
	if len(objs) != 1 {
		t.Fatalf("got %d objects, want 1", len(objs))
	}
	got := objs[0]
	want := closure.Ref{
		GVK:       closure.GVK{Group: "apps", Version: "v1", Kind: "Deployment"},
		Namespace: "prod",
		Name:      "web",
		UID:       "uid-web",
	}
	if got.Ref != want {
		t.Errorf("Ref = %+v, want %+v", got.Ref, want)
	}

	// Observable through the engine: the object is retrievable by its real-uid Ref.
	state := closure.NewScanState(objs)
	if _, ok := state.Get(want); !ok {
		t.Errorf("Get(%v) = not found, want found", want)
	}
}
