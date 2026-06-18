# Design — v0.1 Closure Engine (`closure/`)

Status: **stable draft** · Plan: [plans/v0.1-closure-engine.md](../plans/v0.1-closure-engine.md) · Architecture: [DESIGN.md §3–4, §7–8, §11](../DESIGN.md)

Implements the heart of KRSM: compute an action's affected-resource closure `C(S,A)`
over the four relations and decide `Safe(T,S,A) ≡ C(S,A) ⊆ scope(T)`. This document
fixes the concrete Go API; the plan fixes scope and build order.

## Principles

- **Public, embeddable, dependency-free.** Package `closure/` imports only the stdlib —
  no client-go, no YAML, nothing leaked through its API. An agent-builder can `import`
  it directly. (The test loader may use `sigs.k8s.io/yaml`; that never reaches the API.)
- **Faithful to the formal model.** Identity by `uid`; the four relations of DESIGN §3;
  termination/decidability of DESIGN §4 (`|C| ≤ |R|`). Where code and the formal
  definitions could drift, the definitions win (DESIGN §11).
- **A `G(S)` seam.** State access is an interface so the v0.1 scanning impl can be
  swapped for the indexed `O(c·d)` engine (v0.4+) without touching callers.

## Types

```go
type GVK struct{ Group, Version, Kind string }       // apps/v1/Deployment; core = ""

type Ref struct {                                     // resource identity
    GVK       GVK
    Namespace string                                  // "" for cluster-scoped
    Name      string
    UID       string                                  // authoritative match key
}

type OwnerRef struct{ Kind, Name, UID string }        // from metadata.ownerReferences

type RefKind int                                      // cross-resource reference flavour
const ( RefVolume RefKind = iota; RefEnvFrom; RefEnv; RefFinalizerExternal; RefScaleTarget )

type CrossRef struct {                                // consumer → referenced object
    Kind RefKind
    Ref  Ref                                          // the referenced object
}

type Object struct {                                  // per-relation projection of a resource
    Ref          Ref
    Labels       map[string]string
    Selector     map[string]string                    // Service/PDB/NetworkPolicy/workload selector
    Owners       []OwnerRef
    CrossRefs    []CrossRef                            // volumes, envFrom, env.valueFrom, scaleTargetRef
    Finalizers   []string
}

type Verb string                                      // delete | update | patch | scale | restart
type Action struct {
    Verb    Verb
    Target  Ref
    Cascade bool                                       // delete propagation (default true)
    Old, New *Object                                   // payload for mutations (selector/config/finalizer)
}
```

## Effect classes — `classify(a Action) effectSet` (internal)

Classification is an implementation detail (unexported). It returns a **set** (one
action can be several classes — deleting a Namespace is both *cascade* and
*containment*). Each class licenses which relations the BFS follows:

| EffectClass | Triggered by | Relations followed |
|---|---|---|
| `CascadeDelete` | `delete` + `Cascade` | ownerReferences (owner→child, by uid) |
| `Containment` | `delete` of a Namespace | namespace→all contained objects |
| `Disruptive` | `delete`/`scale`/`restart` on a workload | workload→selected Pods |
| `MutateSelector` | `update`/`patch` changing labels/selector | old∪new selector match-set |
| `MutateConfig` | `update`/`patch` of ConfigMap/Secret/PVC | referenced-object→consumers (reverse cross-ref) |
| `Scale` | `scale` on a workload | scaleTargetRef/HPA + PDB controllers |
| `FinalizerRemoval` | `update`/`patch` removing finalizers | finalizer→external (WARN, cross-boundary) |

A Pod reached by any path also pulls in the selector-owners binding it (Service/PDB/NP).

## State — the `G(S)` interface

```go
type State interface {
    Get(Ref) (*Object, bool)
    OwnedChildren(Ref) []Ref          // objects whose ownerReferences match Ref by uid
    NamespaceContents(ns string) []Ref
    PodsSelectedBy(Ref) []Ref         // pods matching the selector of Ref
    SelectorsTargeting(pod Ref) []Ref // Service/PDB/NetworkPolicy whose selector matches the pod
    Consumers(target Ref) []Ref       // pods/workloads referencing target via volume/env/envFrom
    ControllersTargeting(Ref) []Ref   // HPA(scaleTargetRef) + PDB referencing Ref
}
```

`NewScanState(objs []Object) State` — v0.1 linear-scan implementation. Correct, not yet
fast; the indexed implementation (later slice) satisfies the same interface. Identity is
by `UID`; same-name objects in different namespaces/tenants never collide (corpus #1, #10).

## Closure — `Closure(s State, a Action) []Ref`

BFS worklist from `a.Target`, following only relations the effect-class set licenses;
visited-set guard ⇒ each resource expanded once ⇒ finite (`|C| ≤ |R|`) and terminating
even on cyclic ownerReferences (DESIGN §4). The target itself is discarded from the
result (it is the action's subject, not collateral). Result is sorted for determinism.

## Verdict — `Safe(s State, a Action, scope []ScopeRef) Decision`

```go
type ScopeRef struct{ GVK GVK; Namespace, Name string }   // v0.1 flat identity; Name may be "*"/glob
type Verdict int                                           // Allow < Warn < Block
type Decision struct {
    Verdict  Verdict
    Closure  []Ref
    Escaping []Ref     // in-cluster closure members not covered by any scope pattern → Block
    External []Ref     // cross-boundary effects (finalizer→external) → Warn, NOT counted as escaping
}
```

- A closure member matches a `ScopeRef` when GVK+namespace match and the name matches the
  (possibly glob) pattern (`fnmatch`-style via `path.Match`).
- **Precedence Block > Warn > Allow.** Any in-cluster escape ⇒ Block. Else any external
  effect ⇒ Warn. Else Allow. (Corpus #9: the orphaned external resource is reported in
  `External`/`Warn`, excluded from `Escaping`.)

## Test strategy — golden scenarios

`closure/testdata/scenarios/<NN-name>/` each holds `cluster.yaml`, `action.yaml`,
`scope.yaml`, `expected.yaml` (carrying `verdict`, `closure`, `escaping`, `external` in
human `GVK/ns/name` form). A table-driven test walks the directory, loads each scenario
(via `sigs.k8s.io/yaml`, synthesising deterministic UIDs and resolving ownerReferences to
uids on load), runs `Safe`, and asserts closure+escaping+external+verdict. Adding a
scenario = adding a directory. Property tests assert `|C| ≤ |R|` and termination on a
cyclic-ownerReference fixture. Scenario 1 is the acceptance tracer bullet.

## Deviations from the Python oracle (intentional)

Identity GVK+uid (was kind-only); owner-matching by uid (was kind+name); added namespace
containment, envFrom/env.valueFrom, finalizer→external (WARN), scaleTargetRef/HPA+PDB, and
selector-mutation old∪new — all per the plan. The oracle remains the parity reference for
the relations it does implement.
