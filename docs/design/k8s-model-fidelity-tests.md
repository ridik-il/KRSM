# Test list: k8s-model-fidelity  (source: docs/design/k8s-model-fidelity.md)
Ordered as vertical slices — implement one before writing the next (red → green → refactor).
Slice order A1 → A3 → A2 (smallest/safest first). The 13 existing golden scenarios must stay green
throughout (behaviour parity).

## Slice A1 — cluster-scoped resource scope
1. [x] Deleting a Namespace does **not** pull in a cluster-scoped `PersistentVolume` (which lives in
       no namespace). New scenario: cluster has Namespace `default`, a Pod in it, and a PV; the task
       is scoped to the namespace + its contents (not the PV). RED = Block (PV wrongly counted as
       namespace contents because it is mis-namespaced to `default`); GREEN = Allow (PV resolves to
       namespace `""`, excluded from containment).

## Slice A3 — cross-reference coverage (true false-negative fixes)
2. [x] Deleting a ConfigMap consumed **only via an initContainer** pulls the consuming workload into
       the closure. New scenario: a Deployment references the ConfigMap under
       `spec.template.spec.initContainers[].envFrom`; action deletes the ConfigMap; scope authorises
       only the ConfigMap. RED = Allow (consumer invisible — the missed escape); GREEN = Block
       (consumer followed).
3. [x] Same coverage for a **projected** volume `configMap`/`secret` source and for a pod-level
       **imagePullSecret**. New scenario(s): a Secret consumed only via `volumes[].projected.sources[]`
       and a Secret named in `imagePullSecrets`; deleting it pulls the consuming pod/workload in.
       RED = Allow; GREEN = Block.

## Slice A2 — faithful label selectors
4. [x] `closure.LabelSelector.Matches` evaluates `In`, `NotIn`, `Exists`, `DoesNotExist` correctly,
       **including absence** (a `NotIn`/`DoesNotExist` requirement matches a pod whose key is missing).
       Public-method unit test — this is the absence-sensitivity trap.
5. [x] Parity: all 13 existing golden scenarios produce identical verdicts after `Object.Selector`
       retypes to `LabelSelector` and `subsetOf`→`Matches` (matchLabels-only reduces to the old test;
       nil-vs-present-empty preserved — scenario 08 is the guard).
6. [x] A `matchExpressions` selector binds **precisely**: deleting an in-scope Pod that an `In [web]`
       NetworkPolicy does **not** select is correctly **Allowed**. New scenario. RED = Block (selector
       dropped → empty → binds all → NP spuriously pulled in); GREEN = Allow (selector parsed → binds
       only `app=web`). (This is the exact case verified during design-check.)
7. [x] `closure/` imports nothing under `k8s.io/` — guard via `go list -deps ./closure/...`.

## Notes for the build
- A1 proof deletes Namespace `default` (where the PV is currently mis-namespaced), not an arbitrary ns.
- A3 adds one additive const `RefImagePullSecret` after `RefScaleTarget`; `Consumers()` already follows
  every non-`RefScaleTarget` cross-ref, so no engine change.
- A2: keep the empty-selector rule in `selectorBinds` keyed by `ownerKind`; encode `isNil()` (absent →
  binds nothing) vs `isEmpty()` (present `{}` → kind decides). Loader maps operator strings verbatim.
- Gate every slice: gofmt / vet / staticcheck / golangci-lint / `go test -race ./...`. Git guardrail:
  stage at most; the user commits.
