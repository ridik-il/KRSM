// Package closure computes a Kubernetes action's affected-resource closure over
// live cluster relations and decides whether that closure stays within a task's
// authorised scope.
//
// It is the public, embeddable heart of KRSM: an agent-builder can import it
// directly. The package depends only on the standard library — no client-go and
// no YAML types leak through its API. State is supplied through the State
// interface, so the v0.1 linear-scan provider can later be replaced by an
// informer-backed indexed provider without changing callers.
//
// The model and the four relations follow docs/DESIGN.md §3–4; the closure walk
// is finite (|C| ≤ |R|) and terminating even on cyclic ownerReferences.
package closure

import "fmt"

// GVK identifies a Kubernetes kind. Group is "" for the core API group (e.g. Pod).
type GVK struct {
	Group   string
	Version string
	Kind    string
}

// Ref is the identity of a single resource. UID is the authoritative match key
// (Kubernetes ownerReferences carry a uid); GVK+Namespace+Name is the
// human-readable identity used for scope matching and golden comparison.
type Ref struct {
	GVK       GVK
	Namespace string // "" for cluster-scoped resources
	Name      string
	UID       string
}

// key returns a stable dedup/identity key: the uid if present, else the
// human-readable GVK/namespace/name tuple.
func (r Ref) key() string {
	if r.UID != "" {
		return "uid:" + r.UID
	}
	return r.human()
}

// human renders the Kind/namespace/name form used in scope and golden files.
func (r Ref) human() string {
	return fmt.Sprintf("%s/%s/%s", r.GVK.Kind, r.Namespace, r.Name)
}

// String implements fmt.Stringer.
func (r Ref) String() string { return r.human() }

// OwnerRef is a resolved metadata.ownerReferences entry. UID is filled in on
// load by resolving (Kind, Name) against the live state.
type OwnerRef struct {
	Kind string
	Name string
	UID  string
}

// RefKind is the flavour of a cross-resource reference.
type RefKind int

const (
	RefVolume      RefKind = iota // pod volume → ConfigMap/Secret/PVC
	RefEnvFrom                    // container envFrom → ConfigMap/Secret
	RefEnv                        // container env[].valueFrom → ConfigMap/Secret
	RefScaleTarget                // HPA spec.scaleTargetRef → workload
)

// CrossRef is a consumer's explicit reference to another object.
type CrossRef struct {
	Kind RefKind
	Ref  Ref
}

// Object is the per-relation projection of a live resource: only the fields the
// four relations read.
type Object struct {
	Ref        Ref
	Labels     map[string]string
	Selector   map[string]string // Service/PDB/NetworkPolicy/workload selector, flattened
	Owners     []OwnerRef
	CrossRefs  []CrossRef
	Finalizers []string
}

// Verb is the kind of action being proposed.
type Verb string

const (
	Delete  Verb = "delete"
	Update  Verb = "update"
	Patch   Verb = "patch"
	Scale   Verb = "scale"
	Restart Verb = "restart"
)

// Action is the proposed API action. Old/New carry the request payload for
// mutations (used to detect selector/finalizer/config changes); they are nil for
// non-mutating verbs.
type Action struct {
	Verb    Verb
	Target  Ref
	Cascade bool
	Old     *Object
	New     *Object
}

// ScopeRef is one clause of a task's authorised scope (v0.1: flat identity).
// Name may be a glob ("*"); Group/Version are optional and ignored when empty.
type ScopeRef struct {
	GVK       GVK
	Namespace string
	Name      string
}

// Verdict is the allow/deny decision, ordered Allow < Warn < Block.
type Verdict int

const (
	Allow Verdict = iota
	Warn
	Block
)

// String implements fmt.Stringer.
func (v Verdict) String() string {
	switch v {
	case Allow:
		return "Allow"
	case Warn:
		return "Warn"
	case Block:
		return "Block"
	default:
		return fmt.Sprintf("Verdict(%d)", int(v))
	}
}

// Decision is the result of Safe: the full closure (including the action's
// target), the in-cluster members that escaped scope (→ Block), and any
// cross-boundary effects (→ Warn). Reason distinguishes the two kinds of deny
// (DESIGN §5): a scope escape versus a fail-closed deny when the closure cannot
// be computed.
type Decision struct {
	Verdict  Verdict
	Reason   string
	Closure  []Ref
	Escaping []Ref
	External []Ref
}
