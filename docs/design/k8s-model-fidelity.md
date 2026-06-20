# Design: k8s-model-fidelity
Status: approved · Date: 2026-06-20 · Slug: `k8s-model-fidelity` · Plan: docs/plans/k8s-model-fidelity.md

## Summary
Close three fidelity gaps in KRSM's own Kubernetes model so the computed closure matches the real
blast radius — A3 currently *under*-reports (a missed affected resource: false negative), while A1/A2
*over*-report (false positives that wrongly Block in-scope actions). (A1) resolve cluster-scoped
resources to namespace `""` via a known-kind set;
(A3) extend the loader's cross-reference walk to init/ephemeral containers, projected configMap/secret
volume sources, and `imagePullSecrets`; (A2) replace the `map[string]string` selector with a
pure-stdlib typed `LabelSelector` (matchLabels + matchExpressions) evaluated in-core behind the
existing `State` seam. The public `closure` package stays stdlib-only; the 13 golden scenarios stay
behaviour-identical; each fix ships with a new scenario proving the previously-missed escape. A2's
representation decision is recorded in **ADR-0005**.

## Interfaces / contracts

### A2 — selector types (new, in `closure`, stdlib only)
```go
type LabelSelector struct {
    MatchLabels      map[string]string
    MatchExpressions []SelectorRequirement
}
type SelectorRequirement struct {
    Key      string
    Operator SelectorOperator
    Values   []string // In/NotIn: candidates; Exists/DoesNotExist: must be empty
}
type SelectorOperator string
const (
    OpIn           SelectorOperator = "In"
    OpNotIn        SelectorOperator = "NotIn"
    OpExists       SelectorOperator = "Exists"
    OpDoesNotExist SelectorOperator = "DoesNotExist"
)

// Matches reports whether labels satisfy every matchLabels pair AND every
// requirement. The zero-value selector Matches everything; the kind-aware
// "empty binds all vs nothing" rule is NOT decided here (see Behavior).
func (s LabelSelector) Matches(labels map[string]string) bool
```
- `Object.Selector` changes type `map[string]string` → `LabelSelector` (`closure/model.go`). Breaking
  change to the SDK, taken now because there are no external importers (ADR-0005).

### A2 — `State` interface change (`closure/state.go`)
```go
// CHANGED — selector parameter becomes the typed selector:
PodsMatching(ns string, selector LabelSelector, ownerKind string) []Ref
// UNCHANGED — carries a candidate LABEL SET, not a selector:
SelectorsMatchingLabels(ns string, labels map[string]string) []Ref
// UNCHANGED signatures (read Object.Selector internally; its type changed, params did not):
PodsSelectedBy(r Ref) []Ref
SelectorsTargeting(pod Ref) []Ref
```
Only `PodsMatching` changes. The engine (`closure.go`) still never matches a selector itself — it
forwards typed selectors into `PodsMatching`; the G(S) seam (DESIGN §3–4) is preserved.

### A3 — cross-ref kind (one additive const, `closure/model.go`)
```go
const RefImagePullSecret RefKind = iota + … // additive; Consumers() treats it like any non-scaleTarget ref
```
Projected volume sources reuse `RefVolume` (they *are* volumes). No other model/engine change — A3 is
otherwise loader-only (more `CrossRef`s emitted), and `scanState.Consumers` already follows every
non-`RefScaleTarget` cross-ref.

### A1 — loader scope resolution (`internal/scenario`, no public API)
`nsOf(kind, ns)` consults a package-level `clusterScopedKinds` set; a cluster-scoped kind resolves to
namespace `""` regardless of input. No `closure` API change.

## Data model
- **A2:** `LabelSelector`/`SelectorRequirement`/`SelectorOperator` as above. `isNil()` (both fields
  nil → absent selector, binds nothing) and `isEmpty()` (non-nil, zero requirements → present-empty
  `{}`, kind decides) are unexported helpers preserving today's nil-vs-`len==0` distinction.
- **A1:** `clusterScopedKinds = {Namespace, Node, PersistentVolume, ClusterRole, ClusterRoleBinding,
  StorageClass, PriorityClass, CustomResourceDefinition, IngressClass, APIService,
  ValidatingWebhookConfiguration, MutatingWebhookConfiguration, RuntimeClass}` (loader-internal,
  extensible).
- **A3:** loader `rawPodSpec` gains `InitContainers`, `EphemeralContainers` (same shape as
  `Containers`), `volumes[].projected.sources[].{configMap,secret}`, and pod-level `imagePullSecrets`.

## Behavior

### A2 — selector evaluation (semantics must equal apimachinery; the model wins, DESIGN §11)
`Matches` AND-s all requirements:

| Clause | Matches when |
|---|---|
| matchLabels `k=v` | `labels[k] == v` |
| `In` | key present and `labels[key] ∈ Values` |
| `NotIn` | key **absent** or `labels[key] ∉ Values` |
| `Exists` | key present (Values empty) |
| `DoesNotExist` | key **absent** (Values empty) |

Empty-selector rule stays in `selectorBinds(ownerKind, sel, labels)`:
- `sel.isNil()` → binds nothing (any kind).
- `sel.isEmpty()` → binds all pods in ns iff `ownerKind != "Service"` (Service with no selector has no
  endpoints; `podSelector: {}` on a NetworkPolicy/PDB/workload selects all — corpus #8).
- otherwise → `sel.Matches(labels)`.

**Absence-sensitivity:** `NotIn`/`DoesNotExist` match on a *missing* key, which today's key-iterating
`subsetOf` cannot see; `Matches` evaluates each requirement against the full label set. (The v0.4
inverted index must special-case these — a "labels → selectors naming them" forward index cannot serve
absence; noted for that slice, not built here.)

### A1 — scope resolution
A cluster-scoped object resolves to namespace `""`, so `NamespaceContents(ns)` (which returns objects
whose `Namespace == ns`) never returns it for any real namespace — fixing the containment over-count —
and scope clauses for cluster-scoped kinds (loader applies the same `nsOf`) match consistently on `""`.

### A3 — cross-ref following
A ConfigMap/Secret consumed only via an init/ephemeral container, a projected volume source, or
`imagePullSecrets` now produces a `CrossRef`, so deleting/mutating it pulls the consuming pod into the
closure exactly as a normal volume mount does today.

## Components & responsibilities
- `closure/model.go` — `LabelSelector` family; `Object.Selector` retype; `RefImagePullSecret` const.
- `closure/state.go` — `selectorBinds` reshaped to typed selector + `isNil`/`isEmpty`; `subsetOf`
  replaced by `LabelSelector.Matches`; `PodsMatching` signature; `scanState` owns the four-operator
  semantics (one audited place, per §11).
- `closure/closure.go` — `selectorMutationAffected` passes `LabelSelector` (from `Action.Old/New`)
  into `PodsMatching`; otherwise untouched.
- `internal/scenario/scenario.go` — `selectorFrom`/`matchLabels` capture `matchExpressions` into
  `closure.SelectorRequirement` (stdlib `encoding/json`, operator strings mapped verbatim); `nsOf`
  consults `clusterScopedKinds`; `podSpecCrossRefs` walks init/ephemeral/projected/imagePullSecrets.
- `closure/testdata/scenarios/` — three new golden scenarios (A1 PV, A3 initContainer-ConfigMap, A2
  matchExpressions escape); the 13 existing stay byte-identical.
- `docs/adr/0005-*.md` — records the A2 selector-modeling decision.

## Alternatives considered
- **A2 (c) `k8s.io/apimachinery/pkg/labels` in `closure`** — rejected: leaks a k8s dependency through
  the embeddable SDK's public API (DESIGN §7, ADR-0002); non-starter.
- **A2 (b) push all selector evaluation behind `State`** — rejected: relocates the research-critical
  four-operator semantics out of the audited core into each provider, inviting drift between the
  scan and future informer providers — exactly what §11 forbids.
- **Empty-selector rule on the `LabelSelector` type** — rejected: the rule is a property of the
  binding *kind*, which the value doesn't know; keep it in `selectorBinds` keyed by `ownerKind`.
- **A1 via live RESTMapper/discovery now** — deferred to v0.4 (no live cluster yet); static set is the
  honest interim. Custom cluster-scoped CRDs are deferred with it (YAML can't distinguish absent from
  empty namespace, so a fixture can't force scope without discovery).
- **A3 via apimachinery typed decoders now** — deferred (review item B4); stdlib raw-struct extension
  is sufficient and keeps the loader dep-light.

## Trade-offs & risks
- **Breaking SDK change** (`Object.Selector`, `State.PodsMatching`). Acceptable only pre-importers —
  the reason to do it now. Recorded in ADR-0005.
- **Absence-sensitive operator trap** — a naive key-iterating port silently mis-handles
  `NotIn`/`DoesNotExist`, reintroducing a false negative. Mitigated by operator-specific scenarios.
- **nil-vs-present-empty preservation** — corpus #8 depends on it; the loader must keep emitting
  `isNil` for absent and `isEmpty` for `{}`. Guarded by the unchanged scenario 08.
- **A1 static set incompleteness** for arbitrary CRDs — documented interim; v0.4 discovery is the fix.

## Testability
- **Parity (A2):** the 13 golden scenarios produce identical verdicts before/after the type change
  (TDD red/green with zero diff) — matchLabels-only `Matches` reduces to today's `subsetOf`.
- **A2 behavior:** new golden scenario where a `matchExpressions` selector (e.g. an `In [web]`
  NetworkPolicy) is currently treated as empty→binds-all, so deleting an in-scope Pod is **wrongly
  Blocked**; after the fix the selector binds correctly and the verdict is the right **Allow**
  (red = Block, green = Allow). Plus a `LabelSelector.Matches` unit test covering the absence-sensitive
  `NotIn`/`DoesNotExist` operators directly (public method).
- **A1 behavior:** new golden scenario with a cluster-scoped `PersistentVolume`; a namespace-delete
  must not falsely pull it in, and a PV-targeted action scopes correctly on namespace `""`.
- **A3 behavior:** new golden scenario where a ConfigMap/Secret is mounted only via an initContainer
  (and a projected source) → appears in the closure → Block when out of scope.
- **Dependency:** a check that `go list -deps ./closure/...` contains no `k8s.io/` path.
- **Gate:** gofmt/vet/staticcheck/golangci-lint/`go test -race` green throughout.

## Open questions
None blocking. Noted follow-ons (separate slices, not this design): DESIGN §6's scope-contract clause
gaining `matchExpressions` for selector/scope symmetry; the v0.4 inverted-index special-casing of the
absence-sensitive operators; review items B4 (apimachinery migration) and E8 (generative property
tests).
