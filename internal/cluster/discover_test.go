package cluster

import (
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ridik-il/krsm/closure"
)

// discoverLists is a discovery answer exercising every DiscoverTargets predicate:
// read-target kinds (incl. Secret/ConfigMap), a subresource, a create-only virtual
// resource, a non-read-target built-in kind, and a CRD group.
func discoverLists() []*metav1.APIResourceList {
	return []*metav1.APIResourceList{
		{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{
				{Name: "pods", Kind: "Pod", Namespaced: true, Verbs: metav1.Verbs{"get", "list", "watch"}},
				{Name: "pods/status", Kind: "Pod", Namespaced: true, Verbs: metav1.Verbs{"get"}},
				{Name: "secrets", Kind: "Secret", Namespaced: true, Verbs: metav1.Verbs{"get", "list", "watch"}},
				{Name: "configmaps", Kind: "ConfigMap", Namespaced: true, Verbs: metav1.Verbs{"get", "list", "watch"}},
				{Name: "bindings", Kind: "Binding", Namespaced: true, Verbs: metav1.Verbs{"create"}},
			},
		},
		{
			GroupVersion: "apps/v1",
			APIResources: []metav1.APIResource{
				{Name: "deployments", Kind: "Deployment", Namespaced: true, Verbs: metav1.Verbs{"get", "list", "watch"}},
				{Name: "controllerrevisions", Kind: "ControllerRevision", Namespaced: true, Verbs: metav1.Verbs{"get", "list", "watch"}},
			},
		},
		{
			GroupVersion: "example.com/v1",
			APIResources: []metav1.APIResource{
				{Name: "widgets", Kind: "Widget", Namespaced: true, Verbs: metav1.Verbs{"get", "list", "watch"}},
			},
		},
	}
}

// TestDiscoverTargets (PR #32 review debt, design test 23): the production wiring for
// the informer state path — one Target per listable preferred GVR, carrying GVK +
// Namespaced; Secrets/ConfigMaps enumerate as ORDINARY targets (the metadata-only
// split happens later, in state.New); subresources, create-only virtual resources,
// and non-read-target built-in kinds are excluded; CRD groups are included.
// (newPartialDiscovery with an empty failed set serves the lists verbatim — the plain
// discovery fake's ServerPreferredResources returns nothing.)
func TestDiscoverTargets(t *testing.T) {
	disc := newPartialDiscovery(discoverLists(), nil)
	targets, scope, err := DiscoverTargets(disc)
	if err != nil {
		t.Fatalf("DiscoverTargets: %v", err)
	}

	byGVR := map[schema.GroupVersionResource]Target{}
	for _, tgt := range targets {
		if _, dup := byGVR[tgt.GVR]; dup {
			t.Errorf("duplicate target for %v — one informer per GVR", tgt.GVR)
		}
		byGVR[tgt.GVR] = tgt
	}

	want := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	if tgt, ok := byGVR[want]; !ok {
		t.Errorf("missing target %v", want)
	} else if tgt.GVK != (closure.GVK{Group: "apps", Version: "v1", Kind: "Deployment"}) || !tgt.Namespaced {
		t.Errorf("deployments target = %+v, want Deployment GVK + namespaced", tgt)
	}
	for _, res := range []string{"secrets", "configmaps"} {
		if _, ok := byGVR[schema.GroupVersionResource{Version: "v1", Resource: res}]; !ok {
			t.Errorf("%s must enumerate as an ordinary target (the meta/full split is state.New's job)", res)
		}
	}
	if _, ok := byGVR[schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "widgets"}]; !ok {
		t.Error("CRD-group resources must be included (ownerReferences into CRDs)")
	}
	for gvr := range byGVR {
		if gvr.Resource == "pods/status" || gvr.Resource == "bindings" || gvr.Resource == "controllerrevisions" {
			t.Errorf("target %v must be excluded (subresource / create-only / non-read-target)", gvr)
		}
	}
	if ns, ok := scope.Namespaced(closure.GVK{Group: "apps", Version: "v1", Kind: "Deployment"}); !ok || !ns {
		t.Errorf("returned ScopeInfo must know Deployment is namespaced, got (%v, %v)", ns, ok)
	}
}

// TestDiscoverTargetsC1Tolerance (review debt): an unrelated aggregated-API failure
// keeps the resolved targets (C1); a closure-relevant built-in group failing is
// fail-closed — identical rule to the one-shot reader.
func TestDiscoverTargetsC1Tolerance(t *testing.T) {
	disc := newPartialDiscovery(discoverLists(), map[schema.GroupVersion]error{
		{Group: "metrics.k8s.io", Version: "v1beta1"}: errors.New("apiservice unavailable"),
	})
	targets, _, err := DiscoverTargets(disc)
	if err != nil {
		t.Fatalf("DiscoverTargets must tolerate an unrelated aggregated failure, got: %v", err)
	}
	if len(targets) == 0 {
		t.Fatal("resolved groups must still be targeted")
	}

	disc = newPartialDiscovery(discoverLists(), map[schema.GroupVersion]error{
		{Group: "apps", Version: "v1"}: errors.New("apps down"),
	})
	if _, _, err := DiscoverTargets(disc); err == nil {
		t.Fatal("a closure-relevant built-in group failing discovery must fail closed")
	}
}
