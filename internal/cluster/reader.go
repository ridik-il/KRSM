package cluster

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/ridik-il/krsm/closure"
)

// Reader reads a live cluster READ-ONLY and assembles a closure.State. It lists the
// relevant GVKs via the dynamic client (verb "list" only — no create/update/patch/
// delete/deletecollection/apply anywhere in this package), records each GVK's
// namespaced/cluster scope from discovery, and feeds both into the pure BuildObjects
// projection (slice 1) → closure.NewScanState.
//
// It is fail-closed (docs/design/v0.4-live-cluster-reads.md §5): a discovery failure
// or a list error returns an ERROR, never a silently shrunk object set — a partial
// read is an unknown closure, which the safety gate must deny. Errors wrap only the
// API error; the *rest.Config (bearer token / client cert) is never logged or echoed.
type Reader struct {
	disc discovery.DiscoveryInterface
	dyn  dynamic.Interface
}

// NewReader builds a read-only Reader from a *rest.Config resolved from kubeconfig/
// context (slice 3 supplies one). It constructs a discovery client and a dynamic
// client; the listing logic stays behind newReader so tests inject fakes. The cfg is
// used only to build clients — never logged.
func NewReader(cfg *rest.Config) (*Reader, error) {
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build discovery client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build dynamic client: %w", err)
	}
	return newReader(disc, dyn), nil
}

// newReader is the injectable seam: it takes already-built clients so tests drive the
// Reader with discovery/fake + dynamic/fake and no real cluster.
func newReader(disc discovery.DiscoveryInterface, dyn dynamic.Interface) *Reader {
	return &Reader{disc: disc, dyn: dyn}
}

// readTargets are the kinds the four relations can traverse — the broad, safe default
// (plan decision 4): under-scoping is unsafe, over-inclusion is merely conservative.
// Selection intersects this set with what discovery actually reports; discovered CRDs
// (custom groups) are added on top so an ownerReference into a CRD is not missed.
var readTargets = map[string]bool{
	"Pod":                     true,
	"Service":                 true,
	"ConfigMap":               true,
	"Secret":                  true,
	"PersistentVolumeClaim":   true,
	"Namespace":               true,
	"Deployment":              true,
	"ReplicaSet":              true,
	"StatefulSet":             true,
	"DaemonSet":               true,
	"Job":                     true,
	"CronJob":                 true,
	"PodDisruptionBudget":     true,
	"NetworkPolicy":           true,
	"HorizontalPodAutoscaler": true,
}

// builtInGroups are the core Kubernetes API groups. A resource in any OTHER group is
// treated as a CRD and listed too, so the live read can follow ownerReferences into a
// custom resource (plan decision 4). The empty string is the core group ("v1").
var builtInGroups = map[string]bool{
	"":                          true,
	"apps":                      true,
	"batch":                     true,
	"policy":                    true,
	"networking.k8s.io":         true,
	"autoscaling":               true,
	"rbac.authorization.k8s.io": true,
	"storage.k8s.io":            true,
	"apiextensions.k8s.io":      true,
}

// listTargets are the GVRs the Reader lists. Scope (namespaced vs cluster) is carried
// separately by the ScopeInfo from scopeFromLists, so a target needs only its GVR.
type listTargets = []schema.GroupVersionResource

// State lists the relevant GVKs read-only, projects them through BuildObjects, and
// returns a closure.State. Fail-closed on any discovery or list error.
func (r *Reader) State(ctx context.Context) (closure.State, error) {
	_, lists, err := r.disc.ServerGroupsAndResources()
	if err != nil {
		return nil, fmt.Errorf("discover server resources: %w", err)
	}

	scope := scopeFromLists(lists)
	targets, err := selectTargets(lists)
	if err != nil {
		return nil, err
	}

	var objs []unstructured.Unstructured
	for _, t := range targets {
		// Resource(...).List is the ONLY dynamic verb this package invokes — read-only.
		ul, err := r.dyn.Resource(t).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", t.Resource, err)
		}
		objs = append(objs, ul.Items...)
	}

	built, err := BuildObjects(objs, scope)
	if err != nil {
		return nil, fmt.Errorf("build objects: %w", err)
	}
	return closure.NewScanState(built), nil
}

// selectTargets picks the GVRs to list: every discovered resource whose kind is a
// read target OR whose group is a custom (CRD) group. Subresources (names with a "/")
// are skipped. The discovery answer is the source of truth, so a kind the cluster does
// not report is never listed (no spurious "not found").
func selectTargets(lists []*metav1.APIResourceList) (listTargets, error) {
	var out listTargets
	seen := map[schema.GroupVersionResource]bool{}
	for _, list := range lists {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			return nil, fmt.Errorf("parse groupVersion %q: %w", list.GroupVersion, err)
		}
		for _, res := range list.APIResources {
			if isSubresource(res.Name) {
				continue
			}
			if !readTargets[res.Kind] && builtInGroups[gv.Group] {
				continue
			}
			r := schema.GroupVersionResource{Group: gv.Group, Version: gv.Version, Resource: res.Name}
			if seen[r] {
				continue
			}
			seen[r] = true
			out = append(out, r)
		}
	}
	return out, nil
}

func isSubresource(name string) bool {
	for i := 0; i < len(name); i++ {
		if name[i] == '/' {
			return true
		}
	}
	return false
}

// scopeFromLists builds a ScopeInfo from the discovery answer: each (group, version,
// kind) → its Namespaced flag. A GVK absent from discovery is unknown (ok=false), so
// the caller fails closed rather than guess.
func scopeFromLists(lists []*metav1.APIResourceList) ScopeInfo {
	m := make(discoveryScope)
	for _, list := range lists {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			continue // a malformed groupVersion contributes no scope; List parsing fails elsewhere
		}
		for _, res := range list.APIResources {
			if isSubresource(res.Name) {
				continue
			}
			m[closure.GVK{Group: gv.Group, Version: gv.Version, Kind: res.Kind}] = res.Namespaced
		}
	}
	return m
}

// newDiscoveryScope queries discovery once and returns a ScopeInfo over the preferred
// resources. It FAILS CLOSED on a discovery error — never returning a partial/empty
// scope that would silently treat every GVK as unknown.
func newDiscoveryScope(disc discovery.DiscoveryInterface) (ScopeInfo, error) {
	_, lists, err := disc.ServerGroupsAndResources()
	if err != nil {
		return nil, fmt.Errorf("discover server resources: %w", err)
	}
	return scopeFromLists(lists), nil
}

// discoveryScope is a ScopeInfo backed by a discovery-built GVK→namespaced map. A GVK
// not present is unknown (ok=false): the live replacement for the loader's static
// clusterScopedKinds map (internal/scenario/scenario.go:249).
type discoveryScope map[closure.GVK]bool

func (d discoveryScope) Namespaced(gvk closure.GVK) (bool, bool) {
	namespaced, ok := d[gvk]
	return namespaced, ok
}
