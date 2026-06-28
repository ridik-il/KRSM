package cluster

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ridik-il/krsm/closure"
)

// BuildObjects projects live unstructured objects into the per-relation
// closure.Object set the engine consumes. It is PURE: no client, no I/O, no cluster.
//
// It mirrors internal/scenario.parseCluster's relation extraction (the parity oracle,
// docs/design/v0.4-live-cluster-reads.md), reading the SAME field paths from
// unstructured maps instead of typed YAML structs. Two things differ from the loader:
// uids are REAL (metadata.uid / ownerReferences[].uid, no uidOf synthesis), and a
// cross-ref's uid is resolved through a name→uid index over the listed objects.
//
// It fails closed (returns an error) on a malformed object — a missing kind or name,
// or a selector with an unrecognised matchExpressions operator — mirroring the
// loader's parse-boundary rejections. A GVK whose scope ScopeInfo cannot resolve is a
// caller-level fail-closed concern (slice 4), not a builder error.
func BuildObjects(objs []unstructured.Unstructured, scope ScopeInfo) ([]closure.Object, error) {
	out := make([]closure.Object, 0, len(objs))
	for i := range objs {
		o, err := project(objs[i], scope)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	// One-shot optimization: with the whole listed set in hand, fill each cross-ref's
	// referent uid from the objects' real uids. The informer index (slice 3) skips this
	// — closure.crossRefMatches falls back to Kind/ns/name — so the closure result is the
	// same either way (see docs/design/v0.5-c2-consistent-snapshot.md).
	resolveCrossRefUIDs(out)
	return out, nil
}

// project extracts the four relations from ONE object into a closure.Object, using the
// SAME unstructured field paths as the loader's parity oracle. It is INDEX-FREE: a
// cross-ref carries its referent's Kind/namespace/name with an EMPTY uid (the engine's
// crossRefMatches falls back to Kind/ns/name). It is shared verbatim by the one-shot
// reader (via BuildObjects) and the informer index (slice 3), which is the parity
// guarantee that keeps goldens 01–27 authoritative for the live paths.
//
// It fails closed (returns an error) on a malformed object — a missing kind or name, or a
// selector with an unrecognised matchExpressions operator — mirroring the loader's
// parse-boundary rejections.
func project(o unstructured.Unstructured, scope ScopeInfo) (closure.Object, error) {
	kind, name, err := kindName(o)
	if err != nil {
		return closure.Object{}, err
	}
	gvk := gvkOf(o)
	ns := resolveNamespace(o, scope)
	ref := closure.Ref{
		GVK:       gvk,
		Namespace: ns,
		Name:      name,
		UID:       nestedString(o.Object, "metadata", "uid"),
	}
	sel, err := selectorFromUnstructured(kind, o)
	if err != nil {
		return closure.Object{}, fmt.Errorf("%s/%s: %w", kind, name, err)
	}
	return closure.Object{
		Ref:        ref,
		Labels:     labelsOf(o),
		Selector:   sel,
		Owners:     ownersOf(o),
		CrossRefs:  crossRefsFromUnstructured(o, ns),
		Finalizers: finalizersOf(o),
	}, nil
}

// resolveCrossRefUIDs fills each cross-ref's referent uid from a name→uid index built
// over the projected objects themselves — the one-shot equivalent of the old per-build
// nameUIDIndex. A referent not present in the set keeps an empty uid (the engine then
// falls back to Kind/ns/name). It MUTATES objs in place. Index-free callers (the informer
// index) simply do not invoke it.
func resolveCrossRefUIDs(objs []closure.Object) {
	ix := make(nameUIDIndex, len(objs))
	for _, o := range objs {
		if o.Ref.UID != "" {
			ix[humanKey{Kind: o.Ref.GVK.Kind, Namespace: o.Ref.Namespace, Name: o.Ref.Name}] = o.Ref.UID
		}
	}
	for i := range objs {
		for j := range objs[i].CrossRefs {
			cr := &objs[i].CrossRefs[j]
			if cr.Ref.UID == "" {
				cr.Ref.UID = ix.uidFor(cr.Ref.GVK.Kind, cr.Ref.Namespace, cr.Ref.Name)
			}
		}
	}
}

// finalizersOf reads metadata.finalizers verbatim (nil when absent), the input to the
// finalizer-removal cross-boundary relation (closure.ExternalEffects).
func finalizersOf(o unstructured.Unstructured) []string {
	fs, ok := nestedStringSlice(o.Object, "metadata", "finalizers")
	if !ok {
		return nil
	}
	return fs
}

// labelsOf reads metadata.labels (nil when absent), the pod-side input to the
// label-selector relation.
func labelsOf(o unstructured.Unstructured) map[string]string {
	m, ok := nestedStringMap(o.Object, "metadata", "labels")
	if !ok {
		return nil
	}
	return m
}

// ownersOf reads metadata.ownerReferences, carrying each entry's REAL uid. Unlike the
// loader (which synthesises owner uids via uidOf), the live owner edge is already a
// real uid on the child — exactly what closure.OwnedChildren matches on — so no
// name-based resolution is needed.
func ownersOf(o unstructured.Unstructured) []closure.OwnerRef {
	refs, ok := nestedSlice(o.Object, "metadata", "ownerReferences")
	if !ok {
		return nil
	}
	out := make([]closure.OwnerRef, 0, len(refs))
	for _, r := range refs {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, closure.OwnerRef{
			Kind: nestedString(m, "kind"),
			Name: nestedString(m, "name"),
			UID:  nestedString(m, "uid"),
		})
	}
	return out
}

// kindName extracts kind and name, failing closed when either is absent (a malformed
// object). The loader silently skips a kind-less manifest (a YAML comment block); a
// live read should never produce one, so here it is an error.
func kindName(o unstructured.Unstructured) (kind, name string, err error) {
	kind = nestedString(o.Object, "kind")
	name = nestedString(o.Object, "metadata", "name")
	if kind == "" {
		return "", "", fmt.Errorf("object missing kind")
	}
	if name == "" {
		return "", "", fmt.Errorf("%s object missing metadata.name", kind)
	}
	return kind, name, nil
}

// gvkOf parses apiVersion+kind into a closure.GVK, exactly as internal/scenario.gvkOf
// does: "group/version" splits; a bare "v1" is the core group's version.
func gvkOf(o unstructured.Unstructured) closure.GVK {
	apiVersion := nestedString(o.Object, "apiVersion")
	kind := nestedString(o.Object, "kind")
	g := closure.GVK{Kind: kind}
	if parts := strings.SplitN(apiVersion, "/", 2); len(parts) == 2 {
		g.Group, g.Version = parts[0], parts[1]
	} else {
		g.Version = apiVersion
	}
	return g
}

// resolveNamespace returns "" for a cluster-scoped kind (per ScopeInfo) and the
// metadata.namespace (defaulting "" → "default") for a namespaced kind. Defaulting to
// "default" matches the loader's nsOf so the parity oracle holds; an unknown-scope GVK
// is treated as namespaced here (slice-1 builder is pure — the caller fails closed on
// unknown scope in slice 4).
func resolveNamespace(o unstructured.Unstructured, scope ScopeInfo) string {
	gvk := gvkOf(o)
	if namespaced, ok := scope.Namespaced(gvk); ok && !namespaced {
		return ""
	}
	ns := nestedString(o.Object, "metadata", "namespace")
	if ns == "" {
		return "default"
	}
	return ns
}

// nestedString reads a string at the given path, returning "" if absent or not a
// string (a thin wrapper over unstructured.NestedString that drops the found/err
// return for the common read-or-empty case).
func nestedString(obj map[string]any, fields ...string) string {
	s, _, _ := unstructured.NestedString(obj, fields...)
	return s
}

// nestedSlice reads a []any at the given path WITHOUT deep-copying. The apimachinery
// unstructured.NestedSlice deep-copies its result and PANICS ("cannot deep copy ...")
// when a nested element is not a JSON-compatible value (e.g. a bare Go int, which a
// real API read never produces but an adversarial/hand-built map can). Since the
// builder only READS these slices, NestedFieldNoCopy is both panic-safe and cheaper.
// Returns (nil, false) when absent or not a slice — callers then skip the relation.
func nestedSlice(obj map[string]any, fields ...string) ([]any, bool) {
	v, found, err := unstructured.NestedFieldNoCopy(obj, fields...)
	if !found || err != nil {
		return nil, false
	}
	s, ok := v.([]any)
	if !ok {
		return nil, false
	}
	return s, true
}

// nestedMap reads a map[string]any at the given path WITHOUT deep-copying, for the same
// panic-safety reason as nestedSlice. Returns (nil, false) when absent or not a map.
func nestedMap(obj map[string]any, fields ...string) (map[string]any, bool) {
	v, found, err := unstructured.NestedFieldNoCopy(obj, fields...)
	if !found || err != nil {
		return nil, false
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	return m, true
}

// nestedStringMap reads a map[string]string at the given path without deep-copying. It
// mirrors unstructured.NestedStringMap's semantics (every value must be a string) but is
// panic-safe on non-copyable contents. found is true only when the path holds a map; an
// entry whose value is not a string makes the whole read fail (ok=false), matching
// NestedStringMap, so a malformed labels/selector map is skipped rather than partial.
func nestedStringMap(obj map[string]any, fields ...string) (map[string]string, bool) {
	m, ok := nestedMap(obj, fields...)
	if !ok {
		return nil, false
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		s, isStr := v.(string)
		if !isStr {
			return nil, false
		}
		out[k] = s
	}
	return out, true
}

// nestedStringSlice reads a []string at the given path without deep-copying, mirroring
// unstructured.NestedStringSlice (every element must be a string) but panic-safe.
func nestedStringSlice(obj map[string]any, fields ...string) ([]string, bool) {
	s, ok := nestedSlice(obj, fields...)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(s))
	for _, e := range s {
		str, isStr := e.(string)
		if !isStr {
			return nil, false
		}
		out = append(out, str)
	}
	return out, true
}
