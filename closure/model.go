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
	RefVolume          RefKind = iota // pod volume → ConfigMap/Secret/PVC
	RefEnvFrom                        // container envFrom → ConfigMap/Secret
	RefEnv                            // container env[].valueFrom → ConfigMap/Secret
	RefScaleTarget                    // HPA spec.scaleTargetRef → workload
	RefImagePullSecret                // pod imagePullSecrets → Secret
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
	Selector   LabelSelector // Service/PDB/NetworkPolicy/workload selector (matchLabels + matchExpressions)
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

// ScopeDim is the relation dimension a scope clause authorises along (DESIGN §6).
type ScopeDim string

const (
	DimResource  ScopeDim = "resource"  // flat identity: GVK + namespace + name(glob)
	DimSelector  ScopeDim = "selector"  // pods/objects whose labels satisfy Selector
	DimNamespace ScopeDim = "namespace" // every resource in a namespace (GVK optional gate)
	DimOwnership ScopeDim = "ownership" // a root + every resource it transitively owns
)

// ScopeClause is one allow-clause of a task's authorised scope. Exactly one
// dimension's fields are meaningful, chosen by Dim. An empty Dim is read as
// DimResource for backward compatibility with v0.1/v0.2 flat scopes.
//
// DimResource is the v0.1 flat-identity clause: Name may be a glob ("*"), and
// Group/Version are optional (ignored when empty). DimSelector is state-dependent:
// it matches a candidate whose live labels satisfy Selector, gated by the same
// GVK/Namespace fields (see matchScope).
type ScopeClause struct {
	Dim       ScopeDim
	GVK       GVK
	Namespace string
	Name      string        // DimResource only: identity or path.Match glob
	Selector  LabelSelector // DimSelector only: matchLabels + matchExpressions
	Root      Ref           // DimOwnership only: the subtree root (the task target)
}

// ResourceClause builds a DimResource scope clause: a flat-identity allow-clause
// gating on GVK + namespace and matching the resource name (exact or path.Match
// glob). It is the safe constructor for the v0.1 identity dimension — use it (and
// SelectorClause) rather than a struct literal so the Dim/field combination is
// always internally consistent (see ScopeClause.Validate).
func ResourceClause(gvk GVK, namespace, name string) ScopeClause {
	return ScopeClause{Dim: DimResource, GVK: gvk, Namespace: namespace, Name: name}
}

// SelectorClause builds a DimSelector scope clause: a state-dependent allow-clause
// gating on GVK + namespace and matching a candidate whose live labels satisfy
// selector. An empty selector deliberately matches nothing (a fail-safe, not an
// error); see matchScope. It is the safe constructor for the selector dimension.
func SelectorClause(gvk GVK, namespace string, selector LabelSelector) ScopeClause {
	return ScopeClause{Dim: DimSelector, GVK: gvk, Namespace: namespace, Selector: selector}
}

// NamespaceClause builds a DimNamespace scope clause: a pure-Ref allow-clause that
// authorises every resource in namespace (no cluster state needed). The GVK is an
// optional gate (ignored when empty); a non-empty Namespace is required. It carries
// neither a Name nor a Selector — both would be silently ignored — so use it (rather
// than a struct literal) to keep the Dim/field combination consistent (see
// ScopeClause.Validate).
func NamespaceClause(gvk GVK, namespace string) ScopeClause {
	return ScopeClause{Dim: DimNamespace, GVK: gvk, Namespace: namespace}
}

// OwnershipClause builds a DimOwnership scope clause rooted at root: a
// state-dependent allow-clause covering root itself plus every resource transitively
// owned by it (owner→child by uid, the same walk Closure cascades over). Identity
// lives entirely on Root — the clause carries no GVK/Namespace/Name/Selector of its
// own (all would be silently ignored) — so use it (rather than a struct literal) to
// keep the Dim/field combination consistent (see ScopeClause.Validate).
func OwnershipClause(root Ref) ScopeClause {
	return ScopeClause{Dim: DimOwnership, Root: root}
}

// hasSelector reports whether the clause carries any selector requirement.
func (c ScopeClause) hasSelector() bool {
	return len(c.Selector.MatchLabels) > 0 || len(c.Selector.MatchExpressions) > 0
}

// Validate checks a clause's structural consistency — that its Dim and fields form
// a coherent combination — independent of any cluster state. It is the load-time
// guard (parseScope calls it per clause) that turns a malformed or unknown-dimension
// clause into a loud failure instead of silent misbehaviour:
//
//   - Dim must be one of "", DimResource, DimSelector, DimNamespace or DimOwnership.
//     An empty Dim is allowed (read as DimResource for v0.1/v0.2 back-compat); any
//     other value (a typo, or a not-yet-implemented dimension) is rejected —
//     fail-closed, not coerced.
//   - A resource clause must NOT carry a Selector (the selector would be silently
//     ignored, masking a clause that meant to be a selector clause).
//   - A selector clause must NOT carry a Name (the Name would be silently ignored).
//   - A namespace clause must carry a non-empty Namespace (a "namespace" clause that
//     names no namespace is malformed) and must NOT carry a Name or a Selector (both
//     would be silently ignored).
//   - An ownership clause's identity lives entirely on Root: Root must carry a
//     GVK.Kind and a Name, and the clause must NOT carry a clause-level Name,
//     Selector, GVK or Namespace (all would be silently ignored — the subtree is
//     rooted at Root, not at the clause's own gate).
//
// An empty selector on a selector clause is NOT an error: an empty authorisation
// selector is a deliberate match-nothing fail-safe (DESIGN §6), kept loadable.
func (c ScopeClause) Validate() error {
	switch c.Dim {
	case "", DimResource:
		if c.hasSelector() {
			return fmt.Errorf("scope clause %s/%s/%s: resource dimension must not carry a selector", c.GVK.Kind, c.Namespace, c.Name)
		}
	case DimSelector:
		if c.Name != "" {
			return fmt.Errorf("scope clause %s/%s: selector dimension must not carry a name (got %q)", c.GVK.Kind, c.Namespace, c.Name)
		}
	case DimNamespace:
		if c.Namespace == "" {
			return fmt.Errorf("scope clause %s: namespace dimension must carry a non-empty namespace", c.GVK.Kind)
		}
		if c.Name != "" {
			return fmt.Errorf("scope clause %s/%s: namespace dimension must not carry a name (got %q)", c.GVK.Kind, c.Namespace, c.Name)
		}
		if c.hasSelector() {
			return fmt.Errorf("scope clause %s/%s: namespace dimension must not carry a selector", c.GVK.Kind, c.Namespace)
		}
	case DimOwnership:
		if c.Root.GVK.Kind == "" || c.Root.Name == "" {
			return fmt.Errorf("scope clause %s: ownership dimension requires a Root with a Kind and a Name", c.Root)
		}
		if c.Name != "" {
			return fmt.Errorf("scope clause root %s: ownership dimension must not carry a clause-level name (got %q); identity lives on Root", c.Root, c.Name)
		}
		if c.hasSelector() {
			return fmt.Errorf("scope clause root %s: ownership dimension must not carry a selector; identity lives on Root", c.Root)
		}
		if c.GVK != (GVK{}) {
			return fmt.Errorf("scope clause root %s: ownership dimension must not carry a clause-level GVK; identity lives on Root", c.Root)
		}
		if c.Namespace != "" {
			return fmt.Errorf("scope clause root %s: ownership dimension must not carry a clause-level namespace (got %q); identity lives on Root", c.Root, c.Namespace)
		}
	default:
		return fmt.Errorf("scope clause %s/%s: unknown scope dimension %q (want %q, %q, %q, %q, or empty)", c.GVK.Kind, c.Namespace, c.Dim, DimResource, DimSelector, DimNamespace, DimOwnership)
	}
	return nil
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
