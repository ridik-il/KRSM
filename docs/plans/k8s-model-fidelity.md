# Plan: k8s-model-fidelity
Status: approved · Date: 2026-06-20 · Slug: `k8s-model-fidelity`

## Problem
The PR #2 review surfaced three Kubernetes-modeling fidelity gaps in KRSM's own resource model.
Each is a place where the tool's model diverges from real Kubernetes, so the computed closure is
wrong — in **both** directions: **A3 *under*-reports** (a missed affected resource — a true false
negative / scope escape that can wrongly *Allow*), while **A1 and A2 *over*-report** (binding /
containment the action does not actually have — false positives that wrongly *Block* in-scope actions
and print an inexact blast radius). Both directions break the thesis's *exact-closure* claim, and the
golden corpus cannot even *express* most of these cases. For a doctoral artifact, undisclosed holes
in the tool's own model undermine both soundness (A3) and the precision/fidelity claim (A1/A2).
Fix them now, before the corpus, the webhook (v0.5), and a thesis chapter are built on top.

- **A1 — cluster-scoped resources are mis-namespaced.** `nsOf` (`internal/scenario/scenario.go:160`)
  treats only `Namespace` as cluster-scoped; PersistentVolume, Node, ClusterRole(Binding),
  StorageClass, PriorityClass, CRDs, etc. all get a spurious `"default"` namespace. This pollutes the
  namespace-containment relation (a PV would look "contained in" the default namespace) and makes
  scope matching for cluster-scoped kinds wrongly namespace-sensitive.
- **A2 — selectors model only `matchLabels`; `matchExpressions` is silently dropped.**
  `Object.Selector` is `map[string]string` (`closure/model.go:80`); `selectorBinds`/`subsetOf`
  (`closure/state.go:190`) do equality-subset only; the loader's `matchLabels()` ignores
  `matchExpressions`. A Service is equality-only (fine), but Deployment/StatefulSet/PDB/NetworkPolicy
  selectors support `In/NotIn/Exists/DoesNotExist`. Because requirements are AND-ed (and an
  expression-only selector collapses to the empty selector, which on a non-Service binds **all** pods),
  dropping `matchExpressions` makes the selector *broader* — the closure **over**-reports, wrongly
  Blocking in-scope actions (verified: deleting an in-scope Pod is Blocked because an `In [web]`
  NetworkPolicy that should not select it appears to). This is a precision/fidelity defect, not a
  missed escape.
- **A3 — cross-reference parsing is partial.** `rawPodSpec` reads `volumes[].{configMap,secret,pvc}`
  and container `envFrom`/`env.valueFrom`, but not `initContainers`, `ephemeralContainers`,
  `volumes[].projected.sources[].{configMap,secret}`, or `imagePullSecrets`. A ConfigMap/Secret
  consumed only through those paths is invisible to closure.

## Goals / Non-goals
- **Goal:** the model faithfully represents (A1) resource scope (namespaced vs cluster-scoped),
  (A2) full label selectors (matchLabels + matchExpressions), and (A3) the common ConfigMap/Secret/PVC
  consumption paths — so each corresponding scope escape is *detected*.
- **Goal:** each fix is proven by a new golden scenario that **fails before / passes after** (a
  matchExpressions scope-escape, an initContainer-mounted ConfigMap, a cluster-scoped PV).
- **Goal:** the existing **13 golden scenarios stay green** (behaviour-preserving for what already
  works), and `closure/` keeps its **stdlib-only public API** (DESIGN §7).
- **Non-goal:** the full migration of the loader to `k8s.io/apimachinery` typed decoders (review item
  **B4**) — its own later slice. This plan uses the existing raw-struct parsing, extended.
- **Non-goal:** generative/property-based testing of the fixes (review item **E8**) — its own slice.
- **Non-goal:** live RESTMapper/discovery-driven scope detection (that is v0.4, when there is a real
  cluster). A1 uses a static known-cluster-scoped set for now.
- **Non-goal:** indirect relations — `serviceAccountName` → ServiceAccount → its Secrets, CSI/flex
  inline secret refs. Note them; don't build them here.

## Current state
- `internal/scenario/scenario.go` — the loader. `nsOf` (line 160) hardcodes the only cluster-scoped
  kind; `selectorFrom`/`matchLabels` build `map[string]string` selectors, dropping matchExpressions;
  `podSpecCrossRefs` walks `volumes`/`containers` only (no init/ephemeral/projected).
- `closure/model.go` — `Object.Selector map[string]string`; `Object.CrossRefs []CrossRef` (generic, so
  A3 needs **no** model change — only more refs produced by the loader).
- `closure/state.go` — `scanState` evaluates selectors via `selectorBinds`/`subsetOf`; the `State`
  interface passes selectors and labels as `map[string]string`. The closure engine
  (`closure/closure.go`) never matches selectors itself — it calls `State` methods — so selector
  *evaluation* already lives behind the `State` seam.
- `closure/testdata/scenarios/` — 13 golden scenarios; `internal/scenario` golden tests assert them.

## Approach
Three independent slices, smallest/lowest-risk first; A2 last because it ripples through the public
`closure` model and the `State` interface and needs the architecture decision (below) settled first.

- **A1** is local: replace the single-kind check with a **known cluster-scoped kind set** (and let a
  fixture force cluster scope by writing `namespace: ""`/omitting it for a known cluster-scoped kind).
  Cluster-scoped objects then carry namespace `""`, which *automatically* excludes them from the
  namespace-containment over-count (they match no real namespace) — so no engine change, just correct
  data. Forward path: v0.4 replaces the static set with discovery/RESTMapper.
- **A3** is loader-only: extend the cross-ref walk to init/ephemeral containers, projected volume
  sources, and `imagePullSecrets`. The engine consumes `CrossRef`s generically, so there is **no
  engine or model change** — only more refs emitted. Prove with scenarios.
- **A2** is the deep one and the reason this goes through `design-doc` + an **ADR**: faithful
  matchExpressions needs set-based evaluation, which tensions with `closure/` staying stdlib-only.
  The leading option (to be confirmed in design, with a `go-pro` design consult) is a **pure-stdlib
  typed selector inside `closure`** — a small `LabelSelector{matchLabels, matchExpressions}` value
  with its own stdlib `Matches(labels)` (the In/NotIn/Exists/DoesNotExist semantics are ~30 lines and
  fully specified), so the SDK stays dependency-free *and* faithful, and evaluation stays behind the
  existing `State` seam. The alternatives (push all evaluation behind `State` so a provider may use
  apimachinery; or take the `k8s.io/apimachinery/pkg/labels` dependency in `closure`) are weighed in
  the design/ADR. This changes the public `Object.Selector` type and the `State` selector signatures
  — a deliberate breaking change made **now**, while there are no external SDK importers.

## Steps (vertical slices)
1. [ ] **A1 — correct resource scope.** Cluster-scoped kind set in the loader; cluster-scoped objects
   get namespace `""`. New scenario: a cluster-scoped resource (e.g. PV) that a namespace-delete must
   *not* falsely pull in, and/or a cluster-scoped target scoped correctly. Existing 13 stay green.
2. [ ] **A3 — complete the cross-ref walk.** Parse init/ephemeral containers, projected
   configMap/secret sources, and `imagePullSecrets`. New scenario: a ConfigMap/Secret mounted only in
   an initContainer (and one via a projected volume) appears in the closure → Block when out of scope.
3. [ ] **A2 — faithful label selectors.** Per the approved design/ADR: typed `LabelSelector`
   (matchLabels + matchExpressions) in `closure` with stdlib `Matches`; update `scanState` evaluation,
   the `State` selector signatures, and the loader parser; keep matchLabels-only inputs behaving
   exactly as today. New scenario: a workload/NetworkPolicy binding pods via `matchExpressions` whose
   closure escapes scope → Block (undetectable before). Verify `closure/` imports no `k8s.io/...`.

## Risks & unknowns
- **A2 ripples through the public `closure` API** (model + `State` interface) and every caller. Mit:
  the 13 golden scenarios are the behaviour guard; a `LabelSelector` carrying only matchLabels must
  reproduce today's verdicts exactly.
- **Breaking SDK change.** Changing `Object.Selector`/`State` signatures breaks importers — acceptable
  *only* because there are none yet; this is precisely why we do it early. Record in the ADR.
- **A1 static set is incomplete for arbitrary CRDs.** Fixtures can force scope via explicit namespace;
  authoritative scope is a v0.4 discovery concern. Document the interim.
- **A3 over-reach.** `serviceAccountName`/CSI inline secrets are deliberately out — make sure the
  scenarios don't imply they're covered.

## Definition of done
- A1: cluster-scoped kinds resolve to namespace `""`; a cluster-scoped scenario proves containment no
  longer mis-counts them.
- A2: `closure` represents and evaluates matchLabels + matchExpressions faithfully; a matchExpressions
  scope-escape scenario Blocks; `grep` proves `closure/` imports nothing under `k8s.io/`.
- A3: init/ephemeral/projected/imagePullSecrets refs are followed; scenarios prove an
  initContainer-mounted ConfigMap is in the closure.
- All 13 prior + 3 new scenarios green; full gate green (gofmt/vet/staticcheck/golangci-lint/
  `go test -race`); an ADR records the selector-modeling decision.

## Open questions — RESOLVED (approved 2026-06-20)
1. **(Central)** Selector locus → **(a) pure-stdlib typed `LabelSelector` with `Matches` inside
   `closure`** — no `k8s.io/...` in the public API. To be validated by a `go-pro` design consult and
   recorded in an ADR during `design-doc`.
2. A1 cluster-scoped set → seed with the standard kinds (Namespace, Node, PersistentVolume,
   ClusterRole, ClusterRoleBinding, StorageClass, PriorityClass, CustomResourceDefinition, …).
   CORRECTED in design: a fixture *cannot* mark a custom cluster-scoped CRD via "explicit empty
   namespace" (YAML→struct can't distinguish absent from empty), so custom cluster-scoped CRDs are
   deferred to v0.4 discovery; A1 is proven with a standard kind (`PersistentVolume`).
3. A3 `serviceAccountName` → ServiceAccount → **deferred** with the other indirect relations.
