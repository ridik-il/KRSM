package cluster

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// fakeDiscovery serves ServerGroupsAndResources from `all` (every served version, used for
// the scope/namespaced map) and ServerPreferredResources from `preferred` (one version per
// resource, used to pick LIST targets after S3). discoveryfake.FakeDiscovery's own
// ServerPreferredResources returns nil, so without this the post-S3 State would list
// nothing; `preferred` defaults to `all` so existing single-version tests are unaffected.
type fakeDiscovery struct {
	*discoveryfake.FakeDiscovery
	preferred []*metav1.APIResourceList
}

func (f *fakeDiscovery) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	if f.preferred != nil {
		return f.preferred, nil
	}
	return f.Resources, nil
}

func newFakeDiscovery(all []*metav1.APIResourceList) *fakeDiscovery {
	return &fakeDiscovery{FakeDiscovery: &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{Resources: all}}}
}

func newFakeDiscoveryWithPreferred(all, preferred []*metav1.APIResourceList) *fakeDiscovery {
	f := newFakeDiscovery(all)
	f.preferred = preferred
	return f
}

// TestReaderListsPreferredVersionOnce (S3): a resource served at two versions
// (HorizontalPodAutoscaler at autoscaling/v1 AND /v2) must be listed ONCE, at the
// server-preferred version (/v2) — not once per served version. selectTargets dedups only
// by GVR, so v1 and v2 are distinct targets; listing preferred-only collapses them.
func TestReaderListsPreferredVersionOnce(t *testing.T) {
	hpaV1 := &metav1.APIResourceList{
		GroupVersion: "autoscaling/v1",
		APIResources: []metav1.APIResource{{Name: "horizontalpodautoscalers", Kind: "HorizontalPodAutoscaler", Namespaced: true, Verbs: metav1.Verbs{"list"}}},
	}
	hpaV2 := &metav1.APIResourceList{
		GroupVersion: "autoscaling/v2",
		APIResources: []metav1.APIResource{{Name: "horizontalpodautoscalers", Kind: "HorizontalPodAutoscaler", Namespaced: true, Verbs: metav1.Verbs{"list"}}},
	}
	all := []*metav1.APIResourceList{hpaV1, hpaV2}
	preferred := []*metav1.APIResourceList{hpaV2} // server prefers v2

	disc := newFakeDiscoveryWithPreferred(all, preferred)
	listKinds := map[schema.GroupVersionResource]string{
		{Group: "autoscaling", Version: "v1", Resource: "horizontalpodautoscalers"}: "HorizontalPodAutoscalerList",
		{Group: "autoscaling", Version: "v2", Resource: "horizontalpodautoscalers"}: "HorizontalPodAutoscalerList",
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(dynamicScheme(), listKinds)
	r := newReader(disc, dyn)

	if _, err := r.State(context.Background()); err != nil {
		t.Fatalf("State: %v", err)
	}

	var versions []string
	for _, a := range dyn.Actions() {
		if a.GetVerb() == "list" && a.GetResource().Resource == "horizontalpodautoscalers" {
			versions = append(versions, a.GetResource().Version)
		}
	}
	if len(versions) != 1 {
		t.Fatalf("HPA listed %d times %v, want once (preferred version only)", len(versions), versions)
	}
	if versions[0] != "v2" {
		t.Errorf("HPA listed at %q, want preferred %q", versions[0], "v2")
	}
}

// podsOnlyDiscovery is a single-resource discovery (core Pods) used by the C3 pagination
// tests so State lists exactly one GVR and the list reactor paginates it in isolation.
func podsOnlyDiscovery() *fakeDiscovery {
	return newFakeDiscovery([]*metav1.APIResourceList{
		{GroupVersion: "v1", APIResources: []metav1.APIResource{
			{Name: "pods", Kind: "Pod", Namespaced: true, Verbs: metav1.Verbs{"list"}},
		}},
	})
}

func podsListDynamic() *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(dynamicScheme(),
		map[schema.GroupVersionResource]string{{Version: "v1", Resource: "pods"}: "PodList"})
}

// TestReaderPaginatesList (C3): a kind with more objects than one page must be assembled
// across pages by following the Continue token — not truncated to the first page. The
// reactor returns a Continue token + two pods on page 1 and the final pod (empty Continue)
// on page 2; State must surface all three and issue at least two list calls.
func TestReaderPaginatesList(t *testing.T) {
	dyn := podsListDynamic()
	calls := 0
	dyn.PrependReactor("list", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		calls++
		list := &unstructured.UnstructuredList{}
		if calls == 1 {
			list.SetContinue("page-2-token")
			list.Items = []unstructured.Unstructured{*obj("v1", "Pod", "prod", "p1", "uid-1"), *obj("v1", "Pod", "prod", "p2", "uid-2")}
		} else {
			list.Items = []unstructured.Unstructured{*obj("v1", "Pod", "prod", "p3", "uid-3")}
		}
		return true, list, nil
	})

	r := newReader(podsOnlyDiscovery(), dyn)
	st, err := r.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if got := len(st.NamespaceContents("prod")); got != 3 {
		t.Errorf("paginated list assembled %d objects, want 3 across pages", got)
	}
	if calls < 2 {
		t.Errorf("expected >=2 list calls (pagination via Continue), got %d", calls)
	}
}

// TestReaderPaginatedListPageErrorFailsClosed (C3): an error on ANY page must fail the
// whole read closed — never return the pages gathered so far as a (partial) snapshot.
func TestReaderPaginatedListPageErrorFailsClosed(t *testing.T) {
	dyn := podsListDynamic()
	calls := 0
	dyn.PrependReactor("list", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls == 1 {
			list := &unstructured.UnstructuredList{}
			list.SetContinue("page-2-token")
			list.Items = []unstructured.Unstructured{*obj("v1", "Pod", "prod", "p1", "uid-1")}
			return true, list, nil
		}
		return true, nil, errors.New("etcdserver: request timed out")
	})

	r := newReader(podsOnlyDiscovery(), dyn)
	if _, err := r.State(context.Background()); err == nil {
		t.Fatal("a mid-pagination page error must fail closed, not return a partial snapshot")
	}
	if calls < 2 {
		t.Errorf("expected the failure on page 2 (after a first page with a Continue token); got %d calls", calls)
	}
}
