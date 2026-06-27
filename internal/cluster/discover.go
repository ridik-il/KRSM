package cluster

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"

	"github.com/ridik-il/krsm/closure"
)

// Target is a tracked resource type for the informer-backed state path: the GVR to
// inform/list, the GVK (the Kind to stamp metadata-only objects whose TypeMeta the
// metadata API may omit), and whether it is namespaced.
type Target struct {
	GVR        schema.GroupVersionResource
	GVK        closure.GVK
	Namespaced bool
}

// DiscoverTargets resolves the resource types the informer state path watches, reusing
// the slice-1 hardened discovery selection: preferred-version only (S3), C1 partial-
// discovery tolerant, list-verb filtered, CRD-inclusive (the same predicates
// selectTargets uses). It also returns a ScopeInfo over EVERY served version (for
// namespace resolution), exactly as Reader.State does. Fail-closed on a
// closure-relevant discovery failure.
//
// It mirrors selectTargets but additionally carries each target's GVK + Namespaced
// flag, which the informer factory needs (selectTargets returns bare GVRs for the
// one-shot list path and is left unchanged).
func DiscoverTargets(disc discovery.DiscoveryInterface) ([]Target, ScopeInfo, error) {
	all, err := serverResources(disc)
	if err != nil {
		return nil, nil, err
	}
	scope := scopeFromLists(all)

	preferred, err := serverPreferredResources(disc)
	if err != nil {
		return nil, nil, err
	}

	var out []Target
	seen := map[schema.GroupVersionResource]bool{}
	for _, list := range preferred {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			return nil, nil, fmt.Errorf("parse groupVersion %q: %w", list.GroupVersion, err)
		}
		for _, res := range list.APIResources {
			if isSubresource(res.Name) {
				continue
			}
			if !readTargets[res.Kind] && builtInGroups[gv.Group] {
				continue
			}
			if !supportsList(res.Verbs) {
				continue
			}
			gvr := schema.GroupVersionResource{Group: gv.Group, Version: gv.Version, Resource: res.Name}
			if seen[gvr] {
				continue
			}
			seen[gvr] = true
			out = append(out, Target{
				GVR:        gvr,
				GVK:        closure.GVK{Group: gv.Group, Version: gv.Version, Kind: res.Kind},
				Namespaced: res.Namespaced,
			})
		}
	}
	return out, scope, nil
}
