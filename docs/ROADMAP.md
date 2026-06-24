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

## v0.4 — Live-cluster reads + derived default scope

Run closure against a real cluster (read-only) — the motivating milestone — and make
it **useful with zero contracts** (ADR-0011: progressive, derived scope).

- An informer-backed state provider (client-go) builds the four indexes incrementally.
- The CLI can snapshot a live `kind` cluster and compute closure for a given action.
- **Derived default scope (Level 0, ADR-0011).** With no annotation and no contract,
  KRSM synthesizes a conservative scope from the action's own target — the *ownership
  tree* of the target, **alone**. Not a bare namespace boundary (which would Allow the
  flagship intra-namespace cascade `01`), and not OR'd with a namespace clause (under
  union semantics that would re-admit the same collateral and defeat the default; it is
  redundant anyway, since anything cross-namespace is already outside the tree). This is
  the "install and immediately useful" cut. Requires the `ownership` scope dimension and
  a scope *synthesizer* above `scope.Compile`; the `namespace` dimension is built
  alongside (deferred from v0.3, ADR-0008/0009) for explicit/`template` use.

**Done when:** on a `kind` cluster, `krsm check` reports the correct closure for a real
action with no manual snapshot, **and** flags an escaping action under the derived
default scope with no contract written.

**Status: shipped (kind milestone).** Both halves done — derived default scope
(`docs/plans/v0.4-derived-scope.md`) and live-cluster reads (all 5 slices,
`docs/plans/v0.4-live-cluster-reads.md`), proven end-to-end by `internal/e2e` on a real
`kind` cluster. A critical real-world review (PR #11) found the **one-shot live reader is
the kind milestone, not yet production-grade**: partial-discovery-fatal (C1),
non-atomic-snapshot (C2), unbounded/secret-data reads (C3), no timeout (S1), group-blind
target resolution (S2), multi-version double-list (S3), hardcoded cascade (S4). These are
not milestone blockers (v0.4's scope was "kind, one-shot, fail-closed") but are **v0.5
entry criteria** — resolved by the informer-backed indexed state path the webhook needs
anyway. Tracked in `docs/plans/v0.5-admission-webhook.md` (§"v0.4 real-world gaps").

## v0.5 — The admission webhook (audit-first)

The actual product: flag/deny escaping actions before they persist — adoption-friendly
by default (ADR-0011).

- A `ValidatingWebhook` server; `namespaceSelector`/`objectSelector` scoping to
  agent-originated requests; fail-closed on unknown closure.
- **Audit-first verdict mode (ADR-0011).** Install default is **audit** (a derived-scope
  escape is a `Warn`, never a deny); **enforce** (`Block`) is opt-in — the Falco/Kyverno
  lesson, so a day-0 false positive does not get KRSM uninstalled.
- **Progressive scope channel.** Level 1 (`krsm.io/target` annotation → ownership-tree
  from a declared target) and the full `TaskContract` (Level 3, ADR-0009) both supply
  scope; provenance (`derived` | `annotation` | `template` | `contract`) is reported in
  the verdict. A cross-boundary allowlist exempts legitimate shared-resource references
  from *derived* modes.

**Done when:** on `kind`, an agent-stamped escaping `delete` is **flagged in audit mode
and denied pre-persistence in enforce mode**, and an in-scope one is **admitted** — both
with a *derived* scope (no contract) and with a full contract, end-to-end.

## v0.6 — Scope templates (Level 2)

Lower the friction between "derived default" and "full custom" (ADR-0011).

- Built-in `spec.template`s on a `TaskContract` — `ownership-tree`, `namespace-contained`,
  `single-resource`, `selector-bound` — each a one-liner the synthesizer expands to
  clauses. No engine change (sugar over the compiler).

**Done when:** a one-line templated `TaskContract` classifies the corpus scenarios
identically to the equivalent hand-written multi-dimension contract.

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
