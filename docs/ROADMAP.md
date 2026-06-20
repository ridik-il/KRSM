# KRSM — Roadmap

Build-first, in **vertical slices**: each milestone ships something that works and can be shown, and is useful on its own. Versions are markers, not dates. The companion design is [DESIGN.md](DESIGN.md).

> **Principle:** correctness is defined once (golden-file scenarios + the reference oracle), met fast (indexed Go engine), and proven on a real cluster. Every milestone ends with an *observable* check.

---

## v0.1 — Faithful closure engine

The heart of the tool: compute `C(S, A)` correctly over all four relations.

- `Ref` keyed on **GVK + namespace + name**, matched by `uid`.
- All four relations implemented (§3 of DESIGN), including the ones early prototypes skip: namespace containment, `envFrom`/`env.valueFrom` cross-refs, finalizer→external, `scaleTargetRef`/HPA, and selector-mutation (old∪new match-set).
- Effect-class gating per verb (delete/cascade, disruptive, mutate-selector, mutate-config, finalizer-removal, scale).

**Done when:** the engine reproduces the expected verdict for every scenario in `testdata/`, under `go test -race`.

## v0.2 — The scenario corpus

Turn the failure-mode corpus into the project's golden test suite and demo material.

- Each scenario = `(state, action, scope, expected verdict + escaping set)` YAML under `closure/testdata/scenarios/`.
- A table-driven test asserts each one.

**Done when:** every scenario in `closure/testdata/scenarios/` passes (19 today — the failure-mode corpus, a fail-closed case, a workload-update case, an in-scope *allow* case, and the k8s-model-fidelity scenarios: cluster-scoped scope, initContainer/projected/imagePullSecret/ephemeralContainer cross-refs, and `matchExpressions` selector binding); each doubles as a runnable `krsm check <dir>` "static admission misses this, KRSM catches it" demo.

## v0.3 — The scope contract

Make scope declarable beyond flat lists.

- **First (correctness-relevant) step — close the selector/scope asymmetry (ADR-0005 follow-on).** v0.2 made the *closure* side bind `matchExpressions` precisely, but the *scope* side is still flat identity + name-globs, so it cannot express "pods matching `app=web`." A precise closure tested against an imprecise scope still yields avoidable false **Block**s — selector precision on only one side of `C ⊆ scope(T)` is half a fix. The `selector` scope dimension must gain `matchExpressions` and, like `ownership`/`reference`, become **state-dependent** (matching a clause needs the resource's labels, i.e. `State` access — `matchScope` currently sees only a `Ref`). This is the next thing that changes a verdict, so it leads v0.3.
- `TaskContract` → `ScopePredicate` compiler (the five dimensions: resource, ownership, namespace, selector, reference).
- State-dependent dimensions (`ownership`/`selector`/`reference`) evaluate against the closure snapshot.
- Inexpressible scopes emit an explicit gap signal rather than over-granting.

**Done when:** a `TaskContract` YAML compiles and correctly classifies the corpus scenarios, **including** a scenario that is wrongly Blocked today purely because scope cannot express the selector the closure already binds.

## v0.4 — Live-cluster reads

Run closure against a real cluster (read-only) — the motivating milestone.

- An informer-backed state provider (client-go) builds the four indexes incrementally.
- The CLI can snapshot a live `kind` cluster and compute closure for a given action.

**Done when:** on a `kind` cluster, `krsm check` reports the correct closure for a real action, with no manual snapshot.

## v0.5 — The admission webhook

The actual product: deny escaping actions before they persist.

- A `ValidatingWebhook` server; `TaskContract` scope channel; fail-closed on unknown closure.
- `namespaceSelector`/`objectSelector` scoping to agent-originated requests.

**Done when:** on `kind`, an agent-stamped escaping `delete` is **denied pre-persistence** and an in-scope one is **admitted**, end-to-end.

## v1.0 — Embeddable + polished

Make it adoptable and credible.

- `closure` and `scope` are a clean, documented **public SDK** an agent-builder can import.
- README quickstart that works; install path; example `TaskContract`s; a demo gif.
- CI green (gofmt, golangci-lint, staticcheck, `go test -race`); tagged release; `SECURITY.md` disclosure path live.

**Done when:** a stranger can install KRSM, protect a `kind` cluster, and embed the SDK in their own agent from the docs alone.

---

## Deferred (research apparatus — not part of the tool)

Built only if/when they serve the thesis, kept out of this repo to keep the tool clean:

- Synthetic state generator at 10²–10⁵ resources and the latency-vs-size study.
- The multi-condition comparison harness (vs RBAC, OPA/Gatekeeper, runtime-enforcement baselines).
- The Claude-API action generator for benchmark scenarios.
- The dual-label benchmark and the practitioner severity study.

## How we build each slice

Through the project's engineering discipline: **design → design-check → test-first (TDD)**, idiomatic Go (`gofmt`, `golangci-lint`, `staticcheck`, `go test -race`), tight commits, behaviour verified on `kind`. Contributions welcome per [CONTRIBUTING.md](../CONTRIBUTING.md).
