# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/), and the project aims to follow
[Semantic Versioning](https://semver.org/) once it reaches a usable release.

## [Unreleased]

## [0.4.0] - 2026-06-24

### Added
- **Derived default scope — Level 0 (ADR-0011, ROADMAP v0.4).** `krsm check <dir>` now
  returns a verdict with **no `scope.yaml` and no `taskcontract.yaml`**. The loader
  synthesizes a conservative scope from the request target via a new, pure, stdlib-only
  `scope.Derive(target) ScopePredicate` — a single `ownership`-dimension clause rooted
  at the target (the target plus everything it transitively owns), deliberately *not*
  OR'd with a namespace clause (under the union semantics of `C ⊆ scope` a namespace
  clause would re-admit the same-namespace collateral the tree is meant to flag, and it
  is redundant since anything cross-namespace is already outside the tree). This catches
  the flagship intra-namespace cascade with zero declared scope: the broken Service is
  in the closure but is not owned by the Deployment, so it escapes the derived tree. New
  golden `26-derived-default` pins this zero-config verdict.
- **Two new scope dimensions** (DESIGN §6; the ADR-0008/0009 deferral pulled forward by
  ADR-0011):
  - `dim: namespace` (`closure.DimNamespace` / `closure.NamespaceClause(gvk, ns)`) —
    every candidate in a namespace, with an optional GVK gate; a pure-`Ref` match (no
    state). New golden `23-namespace-scope`.
  - `dim: ownership` (`closure.DimOwnership` / `closure.OwnershipClause(root)`) — a root
    plus its transitive owned subtree; **state-dependent**, computed by the *same*
    `State.OwnedChildren` owner→child walk `closure.Closure` uses, so scope-ownership and
    closure-ownership agree by construction (visited-set guarded, `|subtree| ≤ |R|`,
    cycle-safe, memoized per root). New goldens `24-ownership-scope` (the cascade is in
    scope) and `25-ownership-escape` (a non-owned member escapes).
  - `closure.ScopeClause` gains a `Root Ref` field (ownership only) and `Validate`
    extends to the two dimensions (each clause carries only its own fields; a stray
    field is a hard error at load).
- **Scope provenance.** `scope.ScopePredicate` gains `Provenance`
  (`contract` | `derived:ownership-tree`): `Compile` stamps `ProvenanceContract`,
  `Derive` stamps `ProvenanceDerivedOwner`. The `krsm check` SCOPE line now reports the
  scope's source (`scope.yaml`, `taskcontract.yaml`, or `derived (ownership-tree)`) and
  renders the new dimensions readably (`ns:prod/*`, `owns:Deployment.apps/prod/web`).
- **Audit/enforce verdict mode (ADR-0011 audit-first default).** `scope.Mode`
  (`ModeAudit` default / `ModeEnforce`) with `Mode.Apply(closure.Decision) closure.Decision`,
  applied *above* the unchanged `closure.Safe`. In audit a **scope-escape** Block
  (`len(Escaping) > 0`) is downgraded to `Warn` — exit `0`, with the escaping set still
  printed so the operator sees what *would* block — while a **fail-closed** Block (empty
  `Escaping`: closure uncomputable, DESIGN §5) is *not* softened. `krsm check` gains
  `--mode audit|enforce` (default `audit`); the exit status acts on the *applied* verdict.
- **Live-cluster reads (ROADMAP v0.4, the motivating milestone).** `krsm check` can now
  compute the closure against a **real cluster, read-only** — no hand-written
  `cluster.yaml`. New invocation
  `krsm check [--context X] [--kubeconfig P] [--mode audit|enforce] <verb> <Kind/name> [-n ns]`
  resolves a `*rest.Config` through standard client-go rules (flags override
  `$KUBECONFIG` / `~/.kube/config` / current-context), reads the relevant GVKs read-only,
  derives the Level-0 ownership scope when none is declared, and runs the **shared**
  `closure.Safe` / `mode.Apply` / report. A positional directory still selects the offline
  scenario path (unchanged).
- **New internal `cluster` package** — a pure
  `BuildObjects([]unstructured.Unstructured, ScopeInfo) ([]closure.Object, error)`
  projection that extracts the four relations from live `unstructured` objects using the
  **same field paths** as the YAML loader, but with **real** `metadata.uid` /
  `ownerReferences` uids (no synthesis) and a name→uid index resolving cross-references;
  plus a read-only `Reader` (discovery + dynamic client) and a discovery-backed `ScopeInfo`
  that **replaces the loader's static cluster-scoped-kinds map** with live discovery. A
  parity oracle drives the golden corpus through `BuildObjects`, so the live verdict
  matches the offline goldens by construction.
- **Read-only by construction.** The live reader issues only `list` (never
  create/update/patch/delete/apply) — enforced by both a runtime action-tracker assertion
  and a static source-guard test, so a future write call site fails the build. New
  `deploy/rbac/krsm-reader-clusterrole.yaml` ships a least-privilege `get`/`list`/`watch`-only
  `ClusterRole` (no wildcard verb, no write verb) + binding.
- **Distinct fail-closed deny reasons on the live path** — an unresolvable target, an
  unreadable/forbidden kind, and a discovery failure each deny the *whole* check with an
  operator-legible reason; a partial read is an unknown closure (DESIGN §5) and is denied,
  never silently shrunk into a smaller verdict.
- **`kind` acceptance test + `make e2e`.** A `//go:build cluster` end-to-end test (excluded
  from the default `make check` / CI gate) stands up a real `kind` cluster, applies a
  scenario-`01`-equivalent fixture with **no `cluster.yaml` and no contract**, and proves
  the ROADMAP v0.4 "Done when": the derived-default escape is **flagged** and the in-scope
  action **allowed**.
- **First Kubernetes client dependency.** `k8s.io/client-go` + `k8s.io/apimachinery`
  (v0.34.x) enter `go.mod`, confined to `internal/cluster` + `cmd/krsm` — the embeddable
  `closure` / `scope` packages stay **stdlib-only** (guarded by `internal/archguard`).
  Resolving a client-go-reachable `golang.org/x/net` advisory by **bumping** (never
  suppressing) raised `x/net` → v0.56.0, which requires **go 1.25.0** — so the go directive
  moved 1.24 → 1.25.0. `govulncheck` reports no vulnerabilities.

### Changed
- **`krsm check` default verdict mode is now `audit`.** A scope-escape that previously
  exited `2` (Block) now exits `0` (a Warn, with the escaping detail shown) unless
  `--mode enforce` is passed — the Falco/Kyverno adoption lesson (ADR-0011), so a day-0
  false positive does not get KRSM uninstalled. Pass `--mode enforce` for the prior
  block-and-exit-`2` behaviour.
- **Internal (`closure`, non-breaking):** `matchScope`'s label-lookup seam widened from
  a bare `labelsOf` function to a narrow unexported `scopeResolver` interface (`labels` +
  memoized `ownedSubtree`), built by `Safe` from the `State` it already holds — room for
  the future `reference` dimension without accumulating positional functions.
  `closure.Safe`'s public signature and the closure walk are unchanged; `archguard`
  confirms `closure` and `scope` stay stdlib-only; all existing goldens are unchanged
  (corpus now 27 scenarios, 22 prior + 23–27).

### Known limitations
- **v0.4 is the `kind` milestone, not yet production-grade.** The live reader is a
  one-shot, whole-cluster list. A critical real-world review found gaps that surface on a
  production cluster and are resolved in v0.5 (the informer-backed admission webhook):
  partial discovery treated as fatal (#12), a non-atomic snapshot that can miss collateral
  (#13), unbounded reads that pull every Secret's data into memory (#14), no request
  timeout (#15), group-blind CLI target resolution (#16), multi-version double-listing
  (#17), and a hardcoded delete cascade that ignores `propagationPolicy` (#18). Use
  `krsm check` against a real cluster with this understanding; do not gate a production
  cluster on it yet. See `docs/plans/v0.5-admission-webhook.md`.

## [0.3.0] - 2026-06-22

### Added
- Negative golden scenarios (ADR-0010): a scenario's `expected.yaml` may declare a
  `loadError` substring, marking a **fail-closed proof** — `Load(dir)` must fail with a
  matching error instead of yielding a verdict. New scenario
  `22-taskcontract-fail-closed` carries a well-formed `taskcontract.yaml` that uses the
  not-yet-built `dim: ownership`, proving the loader→`scope.Compile` path rejects an
  uncompilable contract end-to-end ("unsupported scope dimension") rather than silently
  narrowing the scope. The directory-driven golden runner (`TestScenarios`,
  `TestClosureBoundedByInventory`) treats `loadError` scenarios as first-class; the 21
  existing scenarios are unaffected.
- Public `scope` package — `TaskContract`→`ScopePredicate` compiler (ADR-0009,
  realising ADR-0003 / DESIGN §6–§7). A new stdlib-only, embeddable package defines a
  Go `TaskContract` (the declarative, agent-referenced authorised scope) and
  `Compile(TaskContract) (ScopePredicate, error)`, which lowers each declared
  allow-clause to a dimension-typed `closure.ScopeClause` (the `resource` and
  `selector` dims) and carries the (unenforced) `maxSeverity`. `Compile` takes the
  struct, not YAML — parsing the wire form lives in the loader, so embedders pull in
  no YAML dependency. It **fails closed**: a wrong `apiVersion`/`kind`, an unsupported
  scope dimension (e.g. a not-yet-built `ownership`), a structurally invalid clause
  (`closure.ScopeClause.Validate`), or an unknown `maxSeverity` is a hard error rather
  than a silently narrowed (allow-nothing) or partial scope. The loader prefers a
  scenario's `taskcontract.yaml` (compiling it) over the legacy `scope.yaml` path;
  new golden scenario `21-taskcontract-selector-scope` proves the compiler path
  end-to-end (Allow). `closure.Safe`'s signature is unchanged; `archguard`'s new
  `TestScopeIsStdlibOnly` guards the package's stdlib-only boundary.
- Selector scope dimension (ADR-0008, ADR-0005 follow-on): a task's authorised scope
  can now express *the pods matching a label selector*, not just flat identities.
  `Safe`'s scope clauses are dimension-typed — `dim: resource` (the v0.1/v0.2 flat
  identity clause, unchanged) and the new `dim: selector`, which matches a closure
  member against its **live** labels via the existing `closure.LabelSelector`
  (`matchLabels` + `matchExpressions`, absence-sensitive operators). This closes the
  `C ⊆ scope(T)` precision asymmetry: a precise closure (ADR-0005) tested against an
  imprecise scope no longer yields avoidable false Blocks. Selector scope matching is
  state-dependent and fails safe — an empty/nil selector clause matches nothing, and a
  candidate whose labels cannot be resolved escapes (fail-closed, DESIGN §5). New
  golden scenario `20-scope-selector-precision` proves a Block→Allow flip that is only
  expressible with the selector dimension; the `krsm check` SCOPE line renders a
  selector clause readably (e.g. `Pod/prod/{app In [web]}`).
- `closure.ResourceClause(gvk, namespace, name)` and
  `closure.SelectorClause(gvk, namespace, selector)` safe constructors, plus
  `ScopeClause.Validate()` — structural-consistency checks (Dim must be `""`,
  `resource`, or `selector`; a resource clause must carry no selector; a selector
  clause must carry no name). `parseScope` runs `Validate` per clause, so a malformed
  scope fails to load loudly instead of misbehaving.
- **Fail-closed unknown scope dimensions:** a scope clause whose `dim` is not one of
  `{"", resource, selector}` (a typo, or a not-yet-implemented dimension like
  `ownership`) is now **rejected at load** (`parseScope` → error) and, as
  defence-in-depth, **matches nothing in the engine** (`matchScope`'s dimension switch
  is explicit — an unknown dim is skipped, never coerced into a resource grant). An
  empty `dim` still reads as `resource` for v0.1/v0.2 back-compat.
- Release automation (ADR-0006): `.goreleaser.yaml` + `.github/workflows/release.yml` —
  a tag (`v*`) builds multi-OS/arch `krsm` binaries with SHA-256 checksums, release notes
  extracted from this changelog, a syft SBOM per archive, and keyless cosign signing.
- CI / supply-chain hardening: `staticcheck` now runs in CI (previously local-only),
  plus `govulncheck`, CodeQL (Go), PR dependency-review, and a weekly OpenSSF Scorecard
  scan (with README badges) and coverage reporting. All actions are pinned to commit SHAs,
  `go-version-file: go.mod` is the single Go-version source, and jobs use least-privilege
  permissions + concurrency cancellation.
- Repo hygiene: Dependabot (gomod + github-actions), `CODEOWNERS`, and a pull-request template.

### Changed
- **Breaking (`closure` SDK, pre-1.0):** `ScopeRef{GVK, Namespace, Name}` is replaced by
  `ScopeClause{Dim, GVK, Namespace, Name, Selector}` with a `ScopeDim` tag
  (`DimResource` / `DimSelector`); `Safe`'s `scope` parameter retypes
  `[]ScopeRef` → `[]ScopeClause`. An empty `Dim` is read as `DimResource`, so existing
  flat scopes behave identically. `ScopeRef` is removed (no deprecated alias) — the same
  no-external-importers rationale as ADR-0005's `Object.Selector` retype (ADR-0008).
- `make check` now mirrors CI (adds `lint` + `staticcheck`); CONTRIBUTING updated to match.

## [0.2.0] - 2026-06-20

### Added
- `krsm check [--plain] <dir>` — runs the closure check for a scenario directory
  (`cluster.yaml` / `request.yaml` / `scope.yaml`) and prints the
  `ACTION` / `SCOPE` / `CLOSURE` / `VERDICT` report. Exit codes: `0` allow/warn,
  `2` block, `1` usage/load error. A `WARN`'s cross-boundary detail goes to stderr
  (with a self-contained verdict stub on stdout); `--plain` emits ASCII without
  emoji for CI logs / non-UTF8 terminals. Every corpus scenario is now a runnable
  demo (completes ROADMAP v0.2).
- `internal/scenario` package: shared loader turning scenario YAML into the
  `closure` types, used by both the CLI and the golden tests (keeps the public
  `closure` API stdlib-only).
- Kubernetes model fidelity (see `docs/design/k8s-model-fidelity.md`, ADR-0005):
  cluster-scoped resources resolve to namespace `""` so a Namespace delete no
  longer mis-counts a `PersistentVolume` as its contents; cross-reference coverage
  extended to `initContainers`, projected volume sources, pod `imagePullSecrets`,
  and `ephemeralContainers`; faithful label selectors — a pure-stdlib
  `LabelSelector` with `matchLabels` + `matchExpressions` (`In`/`NotIn`/`Exists`/
  `DoesNotExist`, absence-sensitive) so set-based selectors bind precisely instead
  of collapsing to the empty selector. Adds golden scenarios 14–19.
- The scenario loader rejects an unrecognised selector operator instead of
  silently binding nothing (fail-closed at the parse boundary), and a safety-
  invariant test asserts no namespaced kind is mislabelled cluster-scoped.
- Generative property tests (`closure/property_test.go`, stdlib `math/rand`) that
  exercise the termination and `|C| ≤ |R|` bounds on thousands of randomized
  adversarial graphs (cyclic/self/deep ownership, selector and cross-ref edges).
- `internal/archguard` test enforcing that `closure/` imports nothing under
  `k8s.io/` (the embeddable-SDK stdlib-only constraint).

### Changed
- **Breaking (pre-1.0 SDK):** `closure.Object.Selector` and `State.PodsMatching`
  retype from `map[string]string` to `closure.LabelSelector` (ADR-0005). A
  `matchLabels`-only selector reduces to the previous subset test, so the golden
  scenarios are unchanged; external embedders (none yet) would need to adapt.

## [0.1.0] - 2026-06-19

### Added
- Project scaffolding: README, design doc (`docs/DESIGN.md`), roadmap, ADRs
  0001–0004, Apache-2.0 license, and CI (`gofmt`, `golangci-lint`, `staticcheck`,
  `go test -race`).
- `krsm` CLI skeleton (`version`, `help`).
- `closure` — the embeddable, stdlib-only closure engine: model types
  (`GVK`/`Ref`/`Object`/`Action`/`Decision`), the four relations (ownerReference
  cascade, namespace containment, label-selector binding, cross-resource
  reference), effect-class gating per verb, a breadth-first closure walk with a
  visited-set guard (`|C| ≤ |R|`, terminating on cyclic ownerReferences), and the
  `Safe` verdict (scope-subset test, fail-closed on an unresolvable target).
- The failure-mode scenario corpus as golden tests under
  `closure/testdata/scenarios/`.

[Unreleased]: https://github.com/ridik-il/krsm/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/ridik-il/krsm/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/ridik-il/krsm/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/ridik-il/krsm/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/ridik-il/krsm/releases/tag/v0.1.0
