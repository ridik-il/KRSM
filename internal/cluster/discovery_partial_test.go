package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	discoveryfake "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/ridik-il/krsm/closure"
)

// partialDiscovery models an aggregated-API outage: ServerGroupsAndResources returns the
// groups it COULD resolve PLUS a non-nil *discovery.ErrGroupDiscoveryFailed naming the
// failed group(s) — exactly what the real client does when an aggregated APIService is
// momentarily unavailable (metrics-server rolling, a flaky adapter/webhook). The C1 fix
// must keep the resolved lists and fail closed only when a closure-relevant (built-in)
// group is among the failed set. Embeds FakeDiscovery so the rest of the
// discovery.DiscoveryInterface is satisfied.
type partialDiscovery struct {
	*discoveryfake.FakeDiscovery
	resolved []*metav1.APIResourceList
	failed   map[schema.GroupVersion]error
}

func (p *partialDiscovery) ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
	return nil, p.resolved, &discovery.ErrGroupDiscoveryFailed{Groups: p.failed}
}

// ServerPreferredResources mirrors the partial failure for the LIST-target path (S3): the
// resolved groups plus the same ErrGroupDiscoveryFailed, so serverPreferredResources
// applies the identical tolerate/fail-closed rule as serverResources.
func (p *partialDiscovery) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	return p.resolved, &discovery.ErrGroupDiscoveryFailed{Groups: p.failed}
}

func newPartialDiscovery(resolved []*metav1.APIResourceList, failed map[schema.GroupVersion]error) *partialDiscovery {
	return &partialDiscovery{
		FakeDiscovery: &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{Resources: resolved}},
		resolved:      resolved,
		failed:        failed,
	}
}

func emptyDynamic() dynamic.Interface {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(dynamicScheme(), listKinds())
}

// TestReaderToleratesUnrelatedAggregatedDiscoveryFailure: a momentary failure of an
// aggregated API (metrics.k8s.io) — while core+apps resolve fine — must NOT fail-close
// the whole read. The Reader keeps the resolved groups and still produces a usable
// closure for an unrelated target (a Deployment delete), reproducing the real-world
// "metrics-server rolling must not deny a delete deployment" case (C1).
func TestReaderToleratesUnrelatedAggregatedDiscoveryFailure(t *testing.T) {
	rs := obj("apps/v1", "ReplicaSet", "prod", "web-7f9", "uid-rs")
	pod := obj("v1", "Pod", "prod", "web-1", "uid-p1", withOwner("ReplicaSet", "web-7f9", "uid-rs"))

	disc := newPartialDiscovery(resourceLists(), map[schema.GroupVersion]error{
		{Group: "metrics.k8s.io", Version: "v1beta1"}: errors.New("the server is currently unable to handle the request"),
	})
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(dynamicScheme(), listKinds(), rs, pod)
	r := newReader(disc, dyn)

	st, err := r.State(context.Background())
	if err != nil {
		t.Fatalf("State must tolerate an unrelated aggregated-API discovery failure, got: %v", err)
	}
	children := st.OwnedChildren(closure.Ref{GVK: closure.GVK{Group: "apps", Version: "v1", Kind: "ReplicaSet"}, Namespace: "prod", Name: "web-7f9", UID: "uid-rs"})
	if len(children) != 1 {
		t.Errorf("OwnedChildren(rs) = %d, want 1 — the resolved groups must still produce a closure", len(children))
	}
}

// TestReaderFailsClosedWhenClosureRelevantGroupFails: if a closure-relevant (built-in)
// group — here apps/v1, which hosts Deployment/ReplicaSet/StatefulSet/DaemonSet — is
// among the failed set, the read is genuinely incomplete for the closure walk, so the
// Reader must fail closed rather than return a shrunk object set.
func TestReaderFailsClosedWhenClosureRelevantGroupFails(t *testing.T) {
	coreOnly := []*metav1.APIResourceList{resourceLists()[0]} // drop the apps list (it "failed")
	disc := newPartialDiscovery(coreOnly, map[schema.GroupVersion]error{
		{Group: "apps", Version: "v1"}: errors.New("the server is currently unable to handle the request"),
	})
	r := newReader(disc, emptyDynamic())

	_, err := r.State(context.Background())
	if err == nil {
		t.Fatalf("State must fail closed when a closure-relevant built-in group (apps) fails discovery")
	}
	if strings.Contains(err.Error(), "token") || strings.Contains(err.Error(), "Bearer") {
		t.Errorf("error must not leak credential material: %q", err)
	}
}

// TestDiscoveryScopeToleratesUnrelatedFailure: newDiscoveryScope must likewise keep the
// resolved scope when only an aggregated group fails.
func TestDiscoveryScopeToleratesUnrelatedFailure(t *testing.T) {
	disc := newPartialDiscovery(resourceLists(), map[schema.GroupVersion]error{
		{Group: "custom.metrics.k8s.io", Version: "v1beta1"}: errors.New("unavailable"),
	})
	scope, err := newDiscoveryScope(disc)
	if err != nil {
		t.Fatalf("newDiscoveryScope must tolerate an unrelated aggregated-API failure, got: %v", err)
	}
	ns, ok := scope.Namespaced(closure.GVK{Group: "apps", Version: "v1", Kind: "Deployment"})
	if !ok || !ns {
		t.Errorf("resolved Deployment scope should survive a tolerated partial discovery (ok=%v, namespaced=%v)", ok, ns)
	}
}

// TestDiscoveryScopeFailsClosedWhenClosureRelevantGroupFails: a built-in group failing
// must fail closed, never a partial scope that silently treats real kinds as unknown.
func TestDiscoveryScopeFailsClosedWhenClosureRelevantGroupFails(t *testing.T) {
	coreOnly := []*metav1.APIResourceList{resourceLists()[0]}
	disc := newPartialDiscovery(coreOnly, map[schema.GroupVersion]error{
		{Group: "apps", Version: "v1"}: errors.New("unavailable"),
	})
	if _, err := newDiscoveryScope(disc); err == nil {
		t.Fatalf("newDiscoveryScope must fail closed when a closure-relevant built-in group fails")
	}
}

// TestResolveKindToleratesUnrelatedFailure: ResolveKind canonicalises from the resolved
// groups even when an aggregated API is down.
func TestResolveKindToleratesUnrelatedFailure(t *testing.T) {
	disc := newPartialDiscovery(resourceLists(), map[schema.GroupVersion]error{
		{Group: "metrics.k8s.io", Version: "v1beta1"}: errors.New("unavailable"),
	})
	r := newReader(disc, emptyDynamic())

	got, err := r.ResolveKind(context.Background(), "deployment")
	if err != nil {
		t.Fatalf("ResolveKind must tolerate an unrelated aggregated-API failure, got: %v", err)
	}
	if got != "Deployment" {
		t.Errorf("ResolveKind = %q, want canonical %q", got, "Deployment")
	}
}

// TestResolveKindFailsClosedWhenClosureRelevantGroupFails: a built-in group failing while
// resolving a kind it would have hosted must fail closed, not guess.
func TestResolveKindFailsClosedWhenClosureRelevantGroupFails(t *testing.T) {
	coreOnly := []*metav1.APIResourceList{resourceLists()[0]}
	disc := newPartialDiscovery(coreOnly, map[schema.GroupVersion]error{
		{Group: "apps", Version: "v1"}: errors.New("unavailable"),
	})
	r := newReader(disc, emptyDynamic())

	if _, err := r.ResolveKind(context.Background(), "deployment"); err == nil {
		t.Fatalf("ResolveKind must fail closed when a closure-relevant built-in group fails discovery")
	}
}
