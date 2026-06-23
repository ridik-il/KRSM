package cluster

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"

	"github.com/ridik-il/krsm/closure"
)

// resourceLists is the broad, safe discovery answer used by the Reader tests: the
// well-known namespaced kinds plus the cluster-scoped Namespace. It mirrors what a
// real ServerPreferredResources returns (one APIResourceList per group/version).
func resourceLists() []*metav1.APIResourceList {
	return []*metav1.APIResourceList{
		{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{
				{Name: "pods", Kind: "Pod", Namespaced: true},
				{Name: "services", Kind: "Service", Namespaced: true},
				{Name: "configmaps", Kind: "ConfigMap", Namespaced: true},
				{Name: "secrets", Kind: "Secret", Namespaced: true},
				{Name: "persistentvolumeclaims", Kind: "PersistentVolumeClaim", Namespaced: true},
				{Name: "namespaces", Kind: "Namespace", Namespaced: false},
			},
		},
		{
			GroupVersion: "apps/v1",
			APIResources: []metav1.APIResource{
				{Name: "deployments", Kind: "Deployment", Namespaced: true},
				{Name: "replicasets", Kind: "ReplicaSet", Namespaced: true},
			},
		},
	}
}

func TestDiscoveryScopeNamespacedKind(t *testing.T) {
	disc := &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{Resources: resourceLists()}}

	scope, err := newDiscoveryScope(disc)
	if err != nil {
		t.Fatalf("newDiscoveryScope: %v", err)
	}

	ns, ok := scope.Namespaced(closure.GVK{Group: "apps", Version: "v1", Kind: "Deployment"})
	if !ok {
		t.Fatalf("Deployment scope should be known")
	}
	if !ns {
		t.Errorf("Deployment should be namespaced")
	}
}

func TestDiscoveryScopeClusterScopedKind(t *testing.T) {
	disc := &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{Resources: resourceLists()}}

	scope, err := newDiscoveryScope(disc)
	if err != nil {
		t.Fatalf("newDiscoveryScope: %v", err)
	}

	ns, ok := scope.Namespaced(closure.GVK{Version: "v1", Kind: "Namespace"})
	if !ok {
		t.Fatalf("Namespace scope should be known")
	}
	if ns {
		t.Errorf("Namespace should be cluster-scoped (not namespaced)")
	}
}

func TestDiscoveryScopeUndiscoveredGVKUnknown(t *testing.T) {
	disc := &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{Resources: resourceLists()}}

	scope, err := newDiscoveryScope(disc)
	if err != nil {
		t.Fatalf("newDiscoveryScope: %v", err)
	}

	_, ok := scope.Namespaced(closure.GVK{Group: "example.com", Version: "v1", Kind: "Widget"})
	if ok {
		t.Errorf("undiscovered GVK must report ok=false so the caller fails closed")
	}
}

// erroringDiscovery returns a FakeDiscovery whose discovery calls fail, modelling an
// API server that cannot answer discovery (a fail-closed input). The fake routes
// ServerGroupsAndResources through its action Invokes, so a reactor error surfaces.
func erroringDiscovery() *discoveryfake.FakeDiscovery {
	f := &clienttesting.Fake{}
	f.AddReactor("*", "*", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("discovery unavailable")
	})
	return &discoveryfake.FakeDiscovery{Fake: f}
}

func TestDiscoveryScopeFailsClosedOnDiscoveryError(t *testing.T) {
	if _, err := newDiscoveryScope(erroringDiscovery()); err == nil {
		t.Fatalf("newDiscoveryScope must return an error on discovery failure, not a partial scope")
	}
}

// --- shared dynamic fake helpers (used by the Reader tests below) ---

func dynamicScheme() *runtime.Scheme {
	return runtime.NewScheme()
}

// listKinds maps the GVRs the Reader lists to their List kinds so the fake dynamic
// client can synthesise empty lists for kinds with no stored objects.
func listKinds() map[schema.GroupVersionResource]string {
	return map[schema.GroupVersionResource]string{
		{Version: "v1", Resource: "pods"}:                       "PodList",
		{Version: "v1", Resource: "services"}:                   "ServiceList",
		{Version: "v1", Resource: "configmaps"}:                 "ConfigMapList",
		{Version: "v1", Resource: "secrets"}:                    "SecretList",
		{Version: "v1", Resource: "persistentvolumeclaims"}:     "PersistentVolumeClaimList",
		{Version: "v1", Resource: "namespaces"}:                 "NamespaceList",
		{Group: "apps", Version: "v1", Resource: "deployments"}: "DeploymentList",
		{Group: "apps", Version: "v1", Resource: "replicasets"}: "ReplicaSetList",
	}
}

func obj(apiVersion, kind, ns, name, uid string, mut ...func(map[string]any)) *unstructured.Unstructured {
	u := u(apiVersion, kind, ns, name, uid, mut...)
	return &u
}

func newTestReader(t *testing.T, objs ...*unstructured.Unstructured) *Reader {
	t.Helper()
	disc := &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{Resources: resourceLists()}}
	runtimeObjs := make([]runtime.Object, 0, len(objs))
	for _, o := range objs {
		runtimeObjs = append(runtimeObjs, o)
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(dynamicScheme(), listKinds(), runtimeObjs...)
	return newReader(disc, dyn)
}

func countRefs(t *testing.T, st closure.State, ns string) int {
	t.Helper()
	return len(st.NamespaceContents(ns))
}

func TestReaderListsObjectsIntoState(t *testing.T) {
	rs := obj("apps/v1", "ReplicaSet", "prod", "web-7f9", "uid-rs")
	pod := obj("v1", "Pod", "prod", "web-1", "uid-p1", withOwner("ReplicaSet", "web-7f9", "uid-rs"))
	r := newTestReader(t, rs, pod)

	st, err := r.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}

	children := st.OwnedChildren(closure.Ref{GVK: closure.GVK{Group: "apps", Version: "v1", Kind: "ReplicaSet"}, Namespace: "prod", Name: "web-7f9", UID: "uid-rs"})
	if len(children) != 1 {
		t.Fatalf("OwnedChildren(rs) = %d, want 1", len(children))
	}
	t.Logf("indexed Refs in prod: %d", countRefs(t, st, "prod"))
	if got := countRefs(t, st, "prod"); got != 2 {
		t.Errorf("NamespaceContents(prod) = %d, want 2", got)
	}
}

func TestReaderMapsClusterScopedKindToEmptyNamespace(t *testing.T) {
	nsObj := obj("v1", "Namespace", "", "prod", "uid-ns")
	cm := obj("v1", "ConfigMap", "prod", "cfg", "uid-cm")
	r := newTestReader(t, nsObj, cm)

	st, err := r.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}

	// The ConfigMap is namespaced → appears in prod; the Namespace is cluster-scoped
	// → resolves to "" and NamespaceContents excludes Kind=="Namespace" anyway.
	if got := countRefs(t, st, "prod"); got != 1 {
		t.Errorf("NamespaceContents(prod) = %d, want 1 (only the ConfigMap)", got)
	}
	if _, ok := st.Get(closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Namespace"}, Namespace: "", Name: "prod", UID: "uid-ns"}); !ok {
		t.Errorf("cluster-scoped Namespace should be tracked with empty namespace")
	}
}

func TestReaderFailsClosedOnDiscoveryError(t *testing.T) {
	disc := erroringDiscovery()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(dynamicScheme(), listKinds())
	r := newReader(disc, dyn)

	if _, err := r.State(context.Background()); err == nil {
		t.Fatalf("State must fail closed on discovery error, not return a partial State")
	}
}

func TestReaderFailsClosedOnListError(t *testing.T) {
	disc := &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{Resources: resourceLists()}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(dynamicScheme(), listKinds())
	dyn.PrependReactor("list", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden: cannot list secrets")
	})
	r := newReader(disc, dyn)

	_, err := r.State(context.Background())
	if err == nil {
		t.Fatalf("State must fail closed when a kind cannot be listed, not return a shrunk closure")
	}
	if strings.Contains(err.Error(), "token") || strings.Contains(err.Error(), "Bearer") {
		t.Errorf("error must not leak credential material: %q", err)
	}
}

func TestReaderUsesOnlyReadVerbs(t *testing.T) {
	pod := obj("v1", "Pod", "prod", "web-1", "uid-p1")
	disc := &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{Resources: resourceLists()}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(dynamicScheme(), listKinds(), pod)
	r := newReader(disc, dyn)

	if _, err := r.State(context.Background()); err != nil {
		t.Fatalf("State: %v", err)
	}

	writeVerbs := map[string]bool{
		"create": true, "update": true, "patch": true,
		"delete": true, "deletecollection": true, "apply": true,
	}
	for _, a := range dyn.Actions() {
		if writeVerbs[a.GetVerb()] {
			t.Errorf("Reader invoked a write verb %q on %s — must be read-only", a.GetVerb(), a.GetResource())
		}
	}
}

func TestNewReaderBuildsClientsFromRESTConfig(t *testing.T) {
	// A minimal config; NewReader must build discovery + dynamic clients without
	// contacting any cluster, and never embed the config (token) anywhere it logs.
	r, err := NewReader(&rest.Config{Host: "https://example.invalid:6443"})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if r == nil || r.disc == nil || r.dyn == nil {
		t.Fatalf("NewReader returned an incompletely built Reader: %+v", r)
	}
}

// TestPackageInvokesNoWriteVerbs is a static, grep-style guard: no mutating dynamic
// verb appears in the package source. The dynamic client mutation methods are
// Create/Update/UpdateStatus/Patch/Delete/DeleteCollection/Apply/ApplyStatus; the
// Reader uses only Resource(...).List. This complements TestReaderUsesOnlyReadVerbs
// (which inspects the fake's action tracker at runtime) by forbidding the call sites
// themselves, so a future edit that adds a write is caught at the source level.
func TestPackageInvokesNoWriteVerbs(t *testing.T) {
	writeCall := regexp.MustCompile(`\.(Create|Update|UpdateStatus|Patch|Delete|DeleteCollection|Apply|ApplyStatus)\s*\(`)
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if loc := writeCall.FindIndex(src); loc != nil {
			t.Errorf("%s invokes a write verb at byte %d — internal/cluster must be read-only", name, loc[0])
		}
	}
}

func TestReaderSkipsUndiscoveredKinds(t *testing.T) {
	// Discovery only reports core v1; the Reader must not attempt apps/v1 lists.
	disc := &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{Resources: []*metav1.APIResourceList{
		{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{
				{Name: "pods", Kind: "Pod", Namespaced: true},
			},
		},
	}}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(dynamicScheme(), listKinds())
	r := newReader(disc, dyn)

	if _, err := r.State(context.Background()); err != nil {
		t.Fatalf("State: %v", err)
	}
	for _, a := range dyn.Actions() {
		if a.GetResource().Group == "apps" {
			t.Errorf("Reader listed an undiscovered group %q", a.GetResource())
		}
	}
}
