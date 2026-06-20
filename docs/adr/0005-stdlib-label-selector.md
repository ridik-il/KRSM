# 5. Model label selectors as a stdlib type in the closure SDK

Date: 2026-06-20

## Status

Accepted

## Context

KRSM's closure must follow Kubernetes label-selector bindings faithfully. Real
Deployment/StatefulSet/PodDisruptionBudget/NetworkPolicy selectors support both `matchLabels` and
`matchExpressions` (operators `In`, `NotIn`, `Exists`, `DoesNotExist`). The v0.1 model represents a
selector as a flattened `map[string]string` and evaluates it with an equality-subset test, silently
dropping `matchExpressions`. Because requirements are AND-ed — and an expression-only selector
collapses to the empty selector, which on a non-Service binds **all** pods — the equality-only model
**over**-approximates the binding: KRSM wrongly Blocks in-scope actions and reports an inexact blast
radius (verified: deleting an in-scope Pod is Blocked because an `In [web]` NetworkPolicy that does
not select it appears to). This violates the exact-closure claim and formal-model fidelity (§11). It
is a precision/fidelity defect, not a missed escape.

Fixing this collides with a hard constraint: the `closure` package is the public, embeddable SDK and
must stay **stdlib-only** (DESIGN §7, ADR-0002) — no `k8s.io/...` may leak through its public API. The
canonical selector implementation, `k8s.io/apimachinery/pkg/labels`, would do exactly that.

Three options were weighed: (a) a pure-stdlib typed selector inside `closure`; (b) push all selector
evaluation behind the `State` interface so a provider may import apimachinery; (c) depend on
apimachinery directly in `closure`.

## Decision

Adopt **(a)**: introduce a pure-stdlib `LabelSelector{ MatchLabels, MatchExpressions }` value type in
`closure`, with `SelectorRequirement` and a string-typed `SelectorOperator` whose constants equal the
apimachinery wire values, and a stdlib `Matches(labels) bool` method implementing the four operators.
Selector semantics stay **in the closure core** (evaluated by the `State` implementation behind the
existing `PodsMatching`/`SelectorsMatchingLabels` seam), not pushed out to providers. The kind-aware
empty-selector rule (empty Service selector binds nothing; empty NetworkPolicy/PDB/workload selector
binds all) stays in `selectorBinds`, keyed by `ownerKind`, because it is a property of the binding
kind, not of the selector value. `Object.Selector` retypes from `map[string]string` to
`LabelSelector` and `State.PodsMatching`'s selector parameter changes accordingly.

## Consequences

- **SDK stays stdlib-only.** No `k8s.io/...` enters the public `closure` API; embeddability (ADR-0002)
  and the §7 boundary hold. The loader may use richer parsing, but in practice captures
  `matchExpressions` with stdlib `encoding/json` too.
- **One audited place for selector semantics.** The four-operator logic lives in the formally-reconciled
  core, so it cannot drift between the linear-scan provider and the future informer-indexed provider —
  the divergence DESIGN §11 forbids. Rejecting (b) is precisely about this.
- **A deliberate breaking change**, made now while there are no external SDK importers: `Object.Selector`
  and `State.PodsMatching` change shape. Done later, it would break embedders.
- **Absence-sensitivity is now a first-class concern.** `NotIn`/`DoesNotExist` match on missing keys,
  which the old equality-subset test could not see; `Matches` evaluates against the full label set, and
  the v0.4 inverted index must special-case these operators (a forward "labels → selectors" index cannot
  serve absence).
- **Backward-compatible for existing data.** A matchLabels-only selector reduces `Matches` to the old
  subset test; the nil-vs-present-empty distinction is preserved, so the 13 golden scenarios are
  unchanged.
- **Follow-on (now the next correctness-relevant step, v0.3 — see ROADMAP):** this ADR makes the
  *closure* side bind `matchExpressions` precisely, but the *scope* side (`TaskContract`, DESIGN §6)
  still cannot, so a precise closure is tested against an imprecise scope and can be wrongly **Block**ed.
  The `selector` scope clause must gain `matchExpressions` **and** become state-dependent (evaluating a
  selector clause needs the candidate resource's labels — `State` access — whereas `matchScope` today
  sees only a `Ref`). Selector precision on only one side of `C ⊆ scope(T)` is half a fix; closing the
  asymmetry leads v0.3 rather than being deferred indefinitely.
