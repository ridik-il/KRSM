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
| `MutateSelector` | `update`/`patch` changing the target's **selector** | pods bound by old∪new selector |
| `MutateLabels` | `update`/`patch` changing the target's **labels** | selector-owners (Service/NP/PDB) binding old∪new labels |
| `MutateConfig` | `update`/`patch`/**`delete`** of ConfigMap/Secret/PVC | referenced-object→consumers (reverse cross-ref) |
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
even on cyclic ownerReferences (DESIGN §4). The target is **included** in the result: the
action affects it directly, so conformance must check the target's own scope membership —
an action whose target is itself out of scope is a violation even with no collateral
(corpus #3, #10). Result is sorted for determinism.

## Verdict — `Safe(s State, a Action, scope []ScopeRef) Decision`

```go
type ScopeRef struct{ GVK GVK; Namespace, Name string }   // v0.1 flat identity; Name may be "*"/glob
type Verdict int                                           // Allow < Warn < Block
type Decision struct {
    Verdict  Verdict
    Reason   string    // distinguishes the two deny kinds: scope escape vs fail-closed
    Closure  []Ref     // includes the action's target
    Escaping []Ref     // closure members not covered by any scope pattern → Block
    External []Ref     // cross-boundary effects (finalizer→external) → Warn, NOT counted as escaping
}
```

- A closure member matches a `ScopeRef` when GVK+namespace match and the name matches the
  (possibly glob) pattern (`fnmatch`-style via `path.Match`).
- **Precedence Block > Warn > Allow.** Any in-cluster escape ⇒ Block. Else any external
  effect ⇒ Warn. Else Allow. (Corpus #9: the orphaned external resource is reported in
  `External`/`Warn`, excluded from `Escaping`.)
- **Fail-closed (DESIGN §5).** If `a.Target` cannot be resolved in the supplied state the
  closure is unknown, so `Safe` denies (Block) with a `Reason` distinct from a scope
  escape — never admits an unbounded blast radius. The closure decision procedure's default
  on uncertainty is *deny*, not allow.

## Test strategy — golden scenarios

`closure/testdata/scenarios/<NN-name>/` each holds `cluster.yaml`, `request.yaml`,
`scope.yaml`, `expected.yaml` (carrying `verdict`, `closure`, `escaping`, `external` in
human `GVK/ns/name` form). A table-driven test walks the directory, loads each scenario
(via `sigs.k8s.io/yaml`, synthesising deterministic UIDs and resolving ownerReferences to
uids on load), runs `Safe`, and asserts closure+escaping+external+verdict (and the deny
`reason` as a substring when a scenario specifies one). Adding a scenario = adding a
directory. Property tests assert `|C| ≤ |R|` and termination on a cyclic-ownerReference
fixture. Scenario 1 is the acceptance tracer bullet.

**Cross-references are read from both the bare-Pod spec and the workload pod template**
(`spec.template.spec.{volumes,containers}`). A Deployment/StatefulSet that mounts a
ConfigMap/Secret/PVC via its template is therefore a `Consumers()` hit, so mutating or
deleting a shared config object reaches the workloads that consume it (corpus #4, #7) —
not only bare Pods.

All ten corpus failure modes (#1–#10) are materialised as golden scenarios, plus a
fail-closed case. A differential **oracle-parity** check lives with the research
apparatus (the Python prototype repo, *outside* this Go repo, per §11): it runs the
reference oracle over these same golden scenarios and asserts the engine never reports
*fewer* escapes than the reference (`oracle_escaping ⊆ go_escaping`) — the intentional
hardenings only *add* detections.

## Deviations from the Python oracle (intentional)

Identity GVK+uid (was kind-only); owner-matching by uid (was kind+name); added namespace
containment, envFrom/env.valueFrom, finalizer→external (WARN), scaleTargetRef/HPA+PDB, and
selector-mutation old∪new — all per the plan. The oracle remains the parity reference for
the relations it does implement.

## Known v0.1 limitations (deliberate, tracked)

These are scoped out of v0.1 on purpose; they are written down so golden scenarios are
authored honestly rather than fudged.

- **Scope is flat identity only (the expressiveness gap).** `scope(T)` is a list of
  `ScopeRef` (GVK + namespace + glob name); there is no ownership/selector/reference
  scope *dimension* until the `TaskContract` compiler (v0.3). So scenarios whose
  intended scope is "a workload **and its own pods**" cannot be stated as one clause:
  e.g. corpus **#6** (scale `web`, where the workload's own pods are the intended effect)
  and **#8** (the API pods the policy is meant to govern) must enumerate those pods with
  a name glob (`api-*`) to express in-scope membership. The closure is exact; only the
  *scope language* is limited. Inexpressible scopes will, in v0.3, emit an explicit gap
  signal rather than silently over-grant.
- **Single-target `Action`.** An action has one `Target`. Multi-target requests —
  corpus **#3**'s "delete the Pod *and* the PVC" or **#5**'s `delete pods --all` — are
  modelled as the single most-dangerous action (e.g. the PVC delete). PV identity and
  `reclaimPolicy` (the disk behind a PVC) are **not** modelled in v0.1; closure stops at
  the in-cluster consumers of the PVC.
- **Identity is `(Kind, namespace, name)` in v0.1.** All state is synthetic fixtures with
  no real `uid`s, so the loader synthesises a deterministic uid from `Kind/namespace/name`
  and `Ref.human()` is the canonical identity/display key. This means two resources that
  share kind+namespace+name across different API **groups** would collide — harmless for
  the corpus, but full GVK-group/real-uid identity is deferred to the live-state slice
  (v0.4), where the API server supplies authoritative `uid`s and `ownerReferences` carry
  `apiVersion`+`uid`.
- **Workload mutation is a coarse over-approximation.** Because the v0.1 `Action` does not
  carry the pod template, any `update`/`patch` of a workload is treated as disruptive —
  the closure pulls in the pods it selects even when the change was a benign annotation
  edit that recreates nothing. This is sound for a safety gate (it never *under*-reports
  the blast radius), at the cost of over-blocking some no-op edits. Field-level template
  diffing (distinguishing an image bump from an annotation change) is deferred; it needs
  the template plumbed into `Action`. Guarded by scenario `12-workload-update-recreates-pods`.
- **A kind-less scope clause matches every kind in the namespace.** `matchScope` skips the
  kind check when `ScopeRef.GVK.Kind == ""`, so a clause like `{namespace: prod, name: "*"}`
  authorises *all* kinds in `prod` — convenient for a deliberate namespace-wide grant, but a
  footgun if the `kind` was omitted by mistake (a too-broad scope silently passes more than
  intended). v0.1 keeps this permissive and documents it here; the `TaskContract` compiler
  (v0.3) is the right place to require an explicit kind (or an explicit `kind: "*"`) so an
  accidental omission is rejected rather than silently widening scope.
