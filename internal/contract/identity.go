package contract

import "fmt"

// clusterScopedKinds are the standard Kubernetes kinds that exist outside any
// namespace. A cluster-scoped object resolves to namespace "" regardless of input, so
// it is never counted as the contents of a namespace and matches scope clauses on "".
// Custom cluster-scoped CRDs need live discovery (internal/cluster reads it live);
// YAML cannot distinguish an absent namespace from an explicit empty one.
//
// SAFETY INVARIANT: only add a kind here if it is *definitely* cluster-scoped. A
// namespaced kind listed here would resolve to namespace "" and so escape its
// namespace's containment — a Namespace delete would silently miss it (a false
// negative, the one error class a safety gate must not have). Over-inclusion of a
// genuinely cluster-scoped kind is merely conservative; under-scoping is unsafe.
// Guarded by TestClusterScopedKindsExcludesNamespaced.
var clusterScopedKinds = map[string]bool{
	"Namespace":                      true,
	"Node":                           true,
	"PersistentVolume":               true,
	"ClusterRole":                    true,
	"ClusterRoleBinding":             true,
	"StorageClass":                   true,
	"PriorityClass":                  true,
	"CustomResourceDefinition":       true,
	"IngressClass":                   true,
	"APIService":                     true,
	"ValidatingWebhookConfiguration": true,
	"MutatingWebhookConfiguration":   true,
	"RuntimeClass":                   true,
}

// NamespaceFor resolves the effective namespace of a kind/namespace pair: "" for a
// cluster-scoped kind (whatever was written), "default" for a namespaced kind with no
// namespace, and the given namespace otherwise. It is the one namespace-defaulting
// rule shared by the contract parser and the scenario loader, so a clause and the
// object it is meant to authorise resolve their namespaces identically.
func NamespaceFor(kind, namespace string) string {
	if clusterScopedKinds[kind] {
		return ""
	}
	if namespace == "" {
		return "default"
	}
	return namespace
}

// SyntheticUID is the OFFLINE identity convention: a deterministic stand-in uid
// derived from a kind/namespace/name tuple. The offline corpus has no API server to
// mint uids, so every object, action target and ownership root is keyed by this
// synthetic form and the engine's uid-based matching (closure.Ref.key, ownedSubtree)
// works unchanged.
//
// It is emphatically NOT a cluster identity: an object read from a live cluster
// carries the API server's real metadata.uid, and a synthetic uid would match nothing
// there. A contract resolved from a live TaskContract CR must therefore have its
// ownership roots resolved against live state rather than relying on this.
func SyntheticUID(kind, namespace, name string) string {
	return fmt.Sprintf("uid:%s/%s/%s", kind, namespace, name)
}
