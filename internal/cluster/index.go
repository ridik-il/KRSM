// Package cluster builds the four KRSM relations (DESIGN §3) from live cluster
// objects read as unstructured.Unstructured, and exposes them through the existing
// closure.State seam — a second State implementation beside closure.NewScanState,
// not an engine change.
//
// It sits OUTSIDE the stdlib-only closure/ and scope/ trees (enforced by
// internal/archguard): client-go/apimachinery enter the module here, never in the
// embeddable SDK. SLICE 1 (this file plus build.go) is the PURE index builder
// (BuildObjects + the name→uid resolver); the live discovery/dynamic Reader and the
// CLI live path are later slices (docs/plans/v0.4-live-cluster-reads.md, steps 2–5).
package cluster

import "github.com/ridik-il/krsm/closure"

// ScopeInfo reports, per GVK, whether that kind is namespaced or cluster-scoped. It
// is the live replacement for the loader's static clusterScopedKinds map
// (internal/scenario/scenario.go:249): the live reader fills it from discovery/
// RESTMapper (slice 2), while slice-1 tests build it by hand. Keeping it an interface
// lets both the discovery client and a fake satisfy the builder's only scope input.
type ScopeInfo interface {
	// Namespaced reports whether gvk is a namespaced kind. ok is false when the
	// GVK's scope is unknown (not discovered); the caller must fail closed rather
	// than guess (slice 4), since guessing cluster-scoped would let a namespaced
	// object escape namespace containment — the unsafe direction for a safety gate.
	Namespaced(gvk closure.GVK) (namespaced bool, ok bool)
}

// humanKey is the (Kind, namespace, name) identity used to resolve a cross-ref /
// owner that names only kind+name to a listed object's real metadata.uid.
type humanKey struct {
	Kind      string
	Namespace string
	Name      string
}

// nameUIDIndex maps a humanKey to the real metadata.uid of a listed object. A miss
// leaves a cross-ref's/owner's UID empty (the referent was not listed); the engine's
// crossRefMatches/OwnedChildren then fall back to Kind/namespace/name.
type nameUIDIndex map[humanKey]string

// uidFor returns the real uid of a listed object, or "" on a miss.
func (ix nameUIDIndex) uidFor(kind, namespace, name string) string {
	return ix[humanKey{Kind: kind, Namespace: namespace, Name: name}]
}
