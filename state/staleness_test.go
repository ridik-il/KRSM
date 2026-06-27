package state

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ridik-il/krsm/closure"
	"github.com/ridik-il/krsm/internal/cluster"
)

var (
	depGVK = closure.GVK{Group: "apps", Version: "v1", Kind: "Deployment"}
	cmGVK  = closure.GVK{Version: "v1", Kind: "ConfigMap"}
)

// seededDyn builds a fake dynamic client holding objs (for FreshGet to GET).
func seededDyn(objs ...*unstructured.Unstructured) *dynamicfake.FakeDynamicClient {
	runObjs := make([]runtime.Object, len(objs))
	for i, o := range objs {
		runObjs[i] = o
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{corpusGVR["Deployment"]: "DeploymentList"}, runObjs...)
}

func depTargets() []cluster.Target {
	return []cluster.Target{
		{GVR: corpusGVR["Deployment"], GVK: depGVK, Namespaced: true},
		{GVR: corpusGVR["ConfigMap"], GVK: cmGVK, Namespaced: true},
	}
}

// guardProvider builds a Provider with a manually-controlled getter + targets and an
// empty index, for hermetic unit-testing of FreshGet and the staleness guard without
// the informer machinery (the informer→index path is covered by the slice-3 tests).
// The cache is seeded directly via p.idx.upsertWithRV so a test can set cache vs fresh
// resourceVersions deterministically (no watch race).
func guardProvider(dyn *dynamicfake.FakeDynamicClient, meta *metadatafake.FakeMetadataClient, targets []cluster.Target) *Provider {
	m := map[closure.GVK]cluster.Target{}
	for _, t := range targets {
		m[t.GVK] = t
	}
	return &Provider{idx: newIndex(), scope: fakeScope{}, getter: objectGetter{dyn: dyn, meta: meta}, targets: m}
}

func hasVerb(actions []k8stesting.Action, verb string) bool {
	for _, a := range actions {
		if a.GetVerb() == verb {
			return true
		}
	}
	return false
}

func newMetaClient(t *testing.T, objs ...runtime.Object) *metadatafake.FakeMetadataClient {
	t.Helper()
	scheme := metadatafake.NewTestScheme()
	if err := metav1.AddMetaToScheme(scheme); err != nil {
		t.Fatalf("AddMetaToScheme: %v", err)
	}
	return metadatafake.NewSimpleMetadataClient(scheme, objs...)
}

// TestFreshGetMetadataKindNoData (test 17): FreshGet for a Secret/ConfigMap uses the
// METADATA client (PartialObjectMetadata), never the dynamic client, so the live GET can
// never pull the Secret's data — and the projected object carries no data (closure.Object
// has no data field).
func TestFreshGetMetadataKindNoData(t *testing.T) {
	gvk := closure.GVK{Version: "v1", Kind: "Secret"}
	secret := uobj("v1", "Secret", "prod", "db-creds", "uid:Secret/prod/db-creds",
		withLabels(map[string]string{"app": "db"}),
		func(m map[string]any) { m["data"] = map[string]any{"password": "c3VwZXItc2VjcmV0"} }, // must never be fetched
	)
	secret.SetResourceVersion("7")
	pom := toPOM(t, secret, gvk)

	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	meta := newMetaClient(t, pom)
	p := guardProvider(dyn, meta, []cluster.Target{{GVR: corpusGVR["Secret"], GVK: gvk, Namespaced: true}})

	obj, ok, err := p.FreshGet(context.Background(), closure.Ref{GVK: gvk, Namespace: "prod", Name: "db-creds"})
	if err != nil || !ok {
		t.Fatalf("FreshGet(secret) = ok %v, err %v; want a hit", ok, err)
	}
	if obj.Labels["app"] != "db" {
		t.Errorf("FreshGet secret labels = %v, want app=db (metadata carries labels)", obj.Labels)
	}
	if !hasVerb(meta.Actions(), "get") {
		t.Error("FreshGet of a metadata kind must use the metadata client (no get recorded)")
	}
	if hasVerb(dyn.Actions(), "get") {
		t.Error("FreshGet of a metadata kind must NOT use the dynamic client (would fetch data)")
	}
	// closure.Object has no Data field — secret data is structurally uncacheable.
}

// TestInSyncNoFreshGet (test 15): the informer populates the cache with each object's
// resourceVersion; when a request carries that same rv, the guard trusts the index and
// issues NO live GET (the read-free hot path). Exercised through the real informer path.
func TestInSyncNoFreshGet(t *testing.T) {
	dep := uobj("apps/v1", "Deployment", "prod", "web", "uid:Deployment/prod/web")
	dep.SetResourceVersion("9")
	p, dyn, meta := buildProviderC(t, []*unstructured.Unstructured{dep}, nil)
	dyn.ClearActions()
	meta.ClearActions()

	ref := closure.Ref{GVK: closure.GVK{Group: "apps", Version: "v1", Kind: "Deployment"}, Namespace: "prod", Name: "web", UID: "uid:Deployment/prod/web"}
	cachedRV, ok := p.idx.rvFor(ref)
	if !ok {
		t.Fatal("informer did not populate the resourceVersion in the index")
	}
	if err := p.CheckFreshness(context.Background(), ref, cachedRV, nil); err != nil {
		t.Fatalf("CheckFreshness(in-sync) = %v, want nil", err)
	}
	if hasVerb(dyn.Actions(), "get") || hasVerb(meta.Actions(), "get") {
		t.Errorf("in-sync request must not trigger any live GET (dyn=%v meta=%v)", dyn.Actions(), meta.Actions())
	}
}

// TestDriftTriggersBoundedFreshGet (test 14): a request whose resourceVersion is newer
// than the cache triggers a bounded FreshGet over {target} ∪ neighbourhood — a GET on the
// dynamic/metadata client, never a re-list.
func TestDriftTriggersBoundedFreshGet(t *testing.T) {
	depU := uobj("apps/v1", "Deployment", "prod", "web", "uid:dep")
	depU.SetResourceVersion("2")
	cmU := uobj("v1", "ConfigMap", "prod", "cfg", "uid:cfg")
	cmU.SetResourceVersion("2")

	dyn := seededDyn(depU)
	meta := newMetaClient(t, toPOM(t, cmU, cmGVK))
	p := guardProvider(dyn, meta, depTargets())

	depRef := closure.Ref{GVK: depGVK, Namespace: "prod", Name: "web", UID: "uid:dep"}
	cmRef := closure.Ref{GVK: cmGVK, Namespace: "prod", Name: "cfg", UID: "uid:cfg"}
	// Cache is STALE at rv=1; the live cluster (the fakes) is at rv=2.
	p.idx.upsertWithRV(closure.Object{Ref: depRef}, "1")
	p.idx.upsertWithRV(closure.Object{Ref: cmRef}, "1")

	if err := p.CheckFreshness(context.Background(), depRef, "2", []closure.Ref{cmRef}); err != nil {
		t.Fatalf("CheckFreshness(reconcilable drift) = %v, want nil", err)
	}
	if !hasVerb(dyn.Actions(), "get") {
		t.Error("drift must trigger a bounded FreshGet on the dynamic client")
	}
	if hasVerb(dyn.Actions(), "list") || hasVerb(meta.Actions(), "list") {
		t.Errorf("staleness fallback must be a bounded GET, never a re-list (dyn=%v meta=%v)", dyn.Actions(), meta.Actions())
	}
}

// TestUnresolvedDriftFailsClosed (test 16): when even a fresh GET cannot reconcile the
// cache to the request's resourceVersion, the guard fails closed with the distinct reason.
func TestUnresolvedDriftFailsClosed(t *testing.T) {
	depU := uobj("apps/v1", "Deployment", "prod", "web", "uid:dep")
	depU.SetResourceVersion("2") // live only reaches rv=2 …
	dyn := seededDyn(depU)
	p := guardProvider(dyn, newMetaClient(t), depTargets())

	depRef := closure.Ref{GVK: depGVK, Namespace: "prod", Name: "web", UID: "uid:dep"}
	p.idx.upsertWithRV(closure.Object{Ref: depRef}, "1")

	err := p.CheckFreshness(context.Background(), depRef, "5", nil) // … but the request is at rv=5
	var se *StalenessError
	if !errors.As(err, &se) {
		t.Fatalf("CheckFreshness(unresolved drift) = %v, want *StalenessError", err)
	}
	if err.Error() != "could not confirm current state" {
		t.Errorf("staleness reason = %q, want %q", err.Error(), "could not confirm current state")
	}
}

// TestStalenessReasonCredentialFree (test 18): the fail-closed reason never echoes object
// data, tokens, or config — even with a data-bearing Secret in the neighbourhood.
func TestStalenessReasonCredentialFree(t *testing.T) {
	depU := uobj("apps/v1", "Deployment", "prod", "web", "uid:dep")
	depU.SetResourceVersion("2")
	secretU := uobj("v1", "Secret", "prod", "db", "uid:sec",
		func(m map[string]any) { m["data"] = map[string]any{"password": "TOPSECRETVALUE"} })
	secretU.SetResourceVersion("2")

	dyn := seededDyn(depU)
	meta := newMetaClient(t, toPOM(t, secretU, closure.GVK{Version: "v1", Kind: "Secret"}))
	p := guardProvider(dyn, meta, append(depTargets(),
		cluster.Target{GVR: corpusGVR["Secret"], GVK: closure.GVK{Version: "v1", Kind: "Secret"}, Namespaced: true}))

	depRef := closure.Ref{GVK: depGVK, Namespace: "prod", Name: "web", UID: "uid:dep"}
	secRef := closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Secret"}, Namespace: "prod", Name: "db", UID: "uid:sec"}
	p.idx.upsertWithRV(closure.Object{Ref: depRef}, "1")

	err := p.CheckFreshness(context.Background(), depRef, "9", []closure.Ref{secRef}) // unreachable rv → fail closed
	if err == nil {
		t.Fatal("expected a fail-closed StalenessError")
	}
	if msg := err.Error(); msg != stalenessReason || strings.Contains(msg, "TOPSECRET") {
		t.Errorf("staleness reason = %q; must equal %q and never leak data", msg, stalenessReason)
	}
}
