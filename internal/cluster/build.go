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
	ix, err := newNameUIDIndex(objs, scope)
	if err != nil {
		return nil, err
	}
	out := make([]closure.Object, 0, len(objs))
	for i := range objs {
		o, err := buildOne(objs[i], scope, ix)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

// newNameUIDIndex maps every listed object's (Kind, resolved-namespace, name) to its
// real metadata.uid. It resolves namespace the same way buildOne does so an
// owner/cross-ref lookup keyed on the resolved namespace finds the referent.
func newNameUIDIndex(objs []unstructured.Unstructured, scope ScopeInfo) (nameUIDIndex, error) {
	ix := make(nameUIDIndex, len(objs))
	for i := range objs {
		kind, name, err := kindName(objs[i])
		if err != nil {
			return nil, err
		}
		ns := resolveNamespace(objs[i], scope)
		uid := nestedString(objs[i].Object, "metadata", "uid")
		if uid != "" {
			ix[humanKey{Kind: kind, Namespace: ns, Name: name}] = uid
		}
	}
	return ix, nil
}

// buildOne projects a single object. The relation extractors are filled in slice-1
// test by test (selector, owners, cross-refs, finalizers).
func buildOne(o unstructured.Unstructured, scope ScopeInfo, ix nameUIDIndex) (closure.Object, error) {
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
		CrossRefs:  crossRefsFromUnstructured(o, ns, ix),
		Finalizers: finalizersOf(o),
	}, nil
}

// finalizersOf reads metadata.finalizers verbatim (nil when absent), the input to the
// finalizer-removal cross-boundary relation (closure.ExternalEffects).
func finalizersOf(o unstructured.Unstructured) []string {
	fs, found, err := unstructured.NestedStringSlice(o.Object, "metadata", "finalizers")
	if !found || err != nil {
		return nil
	}
	return fs
}

// labelsOf reads metadata.labels (nil when absent), the pod-side input to the
// label-selector relation.
func labelsOf(o unstructured.Unstructured) map[string]string {
	m, found, err := unstructured.NestedStringMap(o.Object, "metadata", "labels")
	if !found || err != nil {
		return nil
	}
	return m
}

// ownersOf reads metadata.ownerReferences, carrying each entry's REAL uid. Unlike the
// loader (which synthesises owner uids via uidOf), the live owner edge is already a
// real uid on the child — exactly what closure.OwnedChildren matches on — so no
// name-based resolution is needed.
func ownersOf(o unstructured.Unstructured) []closure.OwnerRef {
	refs, found, err := unstructured.NestedSlice(o.Object, "metadata", "ownerReferences")
	if !found || err != nil {
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
