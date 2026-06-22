# 8. Add a selector dimension to the scope clause

Date: 2026-06-21

## Status

Accepted

## Context

KRSM's verdict is `C(S,A) ⊆ scope(T)`: the action's affected-resource **closure** must
stay within the task's authorised **scope**. ADR-0005 made the *closure* side bind label
selectors faithfully — `matchLabels` **and** `matchExpressions`, with the absence-sensitive
operators (`In`, `NotIn`, `Exists`, `DoesNotExist`) — via a pure-stdlib `LabelSelector`. But
the *scope* side stayed flat identity: `ScopeRef{GVK, Namespace, Name-glob}`, matched by an
unexported `matchScope` that saw only a bare `Ref` and compared the name with `path.Match`.

This is a half-fix. A precise closure tested against an imprecise scope produces **avoidable
false Blocks**. Concretely: a Disruptive verb (e.g. `restart`) on a workload binds *every*
pod the workload selects — ownership-independent, by label — including pods of *other*
workloads that happen to carry the same label. A flat-identity scope cannot cover two such
pods that share no name stem without a `*` glob, and a `*` glob also grants unrelated pods
(e.g. an `app=db` decoy), defeating the precision the closure already has. So the only honest
scope is one that says "the `app=web` pods" — which the flat clause cannot express.

ADR-0005 explicitly named closing this asymmetry as "the next correctness-relevant step
(v0.3)": the `selector` scope clause must gain `matchExpressions` **and** become
state-dependent, because evaluating a selector clause needs the *candidate's* labels — `State`
access — whereas `matchScope` saw only a `Ref`. Three sub-decisions were open: how the clause
is encoded (flat optional `Selector` field vs a dimension-typed clause), how `matchScope`
reaches labels (pass `State`, a narrow lookup, or pre-resolve in `Safe`), and the
empty-selector semantics (match-nothing vs reject-at-load vs the binding side's kind-aware
"empty binds all"). The hard constraint from ADR-0002/0005 still holds: the public `closure`
SDK must stay **stdlib-only** (`internal/archguard`), so the one selector implementation
(`closure.LabelSelector`) must be reused — no second selector engine, no `k8s.io/...`.

## Decision

Replace `ScopeRef` with a **dimension-typed `ScopeClause`** carrying a `ScopeDim` tag, and
make `matchScope` state-dependent through a narrow label lookup:

- **Dimension-typed clause.** `ScopeClause{Dim ScopeDim; GVK; Namespace; Name; Selector
  LabelSelector}` with `DimResource = "resource"` (the v0.1 flat-identity clause, behaviour
  unchanged) and `DimSelector = "selector"` (new). An **empty `Dim` is read as `DimResource`**,
  so every dim-less v0.1/v0.2 scope loads and matches exactly as before. This follows DESIGN §6
  (scope is dimension-typed allow-clauses, one per relation dimension), so the later
  `TaskContract`→`ScopePredicate` compiler *extends* this with `ownership`/`namespace`/
  `reference` rather than reworking a flat field. `ScopeRef` is **removed** — a clean break, no
  deprecated alias.
- **State-dependent matching via a narrow lookup.** `matchScope(r, scope, labels labelsOf)`
  takes `type labelsOf func(Ref) (map[string]string, bool)`, **not** the whole `State`. `Safe`
  supplies it by adapting the `State` it already holds (`o, ok := s.Get(r)`), so the lookup is
  backed by the same snapshot the closure was walked over. A `DimSelector` clause gates on
  `GVK`/`Namespace` exactly like a resource clause (a `Pod` selector clause never matches a
  `Service`), then matches `clause.Selector.Matches(labels(r))` — the existing four-operator,
  absence-sensitive evaluation.
- **Fail-safe semantics, twice.** An **empty or nil** selector clause matches **nothing** (an
  empty *authorisation* selector that matched the whole namespace would be a silent over-grant —
  the opposite call from the binding side's kind-aware "empty NetworkPolicy selector binds all",
  which is faithful for a binding but unsafe for an authorisation). A candidate whose labels are
  **unresolvable** in `State` matches nothing (fail-closed, DESIGN §5). The loader stays lenient
  (no load-time rejection); the engine is the safety boundary.
- **One selector parse path.** The loader (`internal/scenario.parseScope`) builds the clause's
  `LabelSelector` with the **same** `matchLabels` conversion the cluster loader already uses for
  `Object.Selector`. The CLI renders a selector clause readably (`Pod/prod/{app In [web]}`).

## Consequences

- **The `C ⊆ scope(T)` precision asymmetry is closed.** Both sides now bind `matchExpressions`,
  so a precise closure is tested against a precise scope; the class of avoidable false Blocks
  ADR-0005 left open is fixed. Golden scenario `20-scope-selector-precision` pins a Block→Allow
  flip that is *only* expressible with the selector dimension.
- **A deliberate pre-1.0 breaking change.** `ScopeRef`→`ScopeClause` and `Safe`'s `scope`
  parameter change shape, rippling through `internal/scenario` and the CLI formatters. Made now
  while there are no external SDK importers — identical rationale to ADR-0005's `Object.Selector`
  retype; recorded in CHANGELOG. Done post-1.0 it would require a `/v2` module path.
- **SDK stays stdlib-only.** Scope matching reuses `closure.LabelSelector`; no second selector
  engine and no `k8s.io/...` enter the public API. `internal/archguard` still passes. Passing a
  narrow `labelsOf` rather than `State` keeps `matchScope` independent of the full `State`
  surface and trivially testable.
- **Back-compatible for existing data.** The 19 pre-existing golden scenarios stay byte-for-byte
  green: the resource-dim path is the old `matchScope` verbatim, and dim-less `scope.yaml` loads
  as `DimResource`.
- **Follow-on (later v0.3 slices, explicit non-goals here):** the `TaskContract`→`ScopePredicate`
  compiler and a `scope/` package; the other state-dependent dimensions (`ownership`,
  `namespace`, `reference`); and inexpressible-scope gap signalling (e.g. set-difference "all
  `app=web` except canaries"). This slice introduces the discriminated clause with the two
  dimensions it needs now, on the design's committed trajectory.
- **Unknown dimensions fail closed (refinement).** An earlier cut let `matchScope`'s
  switch fall through `default` into the resource branch, so an unrecognised `Dim` (a typo
  like `Selector`, or a not-yet-implemented `ownership`) was silently matched as a resource
  clause — for a safety control, a silent over-grant. The switch is now explicit
  (`case DimResource` / `case DimSelector` / `default → skip`): an unknown dimension covers
  **nothing** in the engine, and the loader (`parseScope`) **rejects** any clause whose `dim`
  is outside `{"", resource, selector}` so a typo'd scenario fails to load loudly. An empty
  `Dim` still reads as `DimResource`. This is defence-in-depth across the two layers (load
  and match), in keeping with the fail-closed posture above.
- **Safe constructors + `Validate` (refinement).** `closure.ResourceClause` and
  `closure.SelectorClause` build the two dimensions with their fields guaranteed consistent,
  and `ScopeClause.Validate()` checks structural consistency (known `Dim`; a resource clause
  carries no selector; a selector clause carries no name; an *empty* selector stays valid as a
  deliberate match-nothing fail-safe). `parseScope` calls `Validate` per clause, which is also
  the load-time guard for the unknown-dimension rejection above. Stdlib-only; `internal/archguard`
  still passes.
