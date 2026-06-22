# 9. A `scope` package and a TaskContract → ScopePredicate compiler

Date: 2026-06-21

## Status

Accepted

## Context

ADR-0003 decided that a KRSM task's authorised scope is supplied by a **`TaskContract`**
the agent references, and DESIGN §6 specifies its CRD-shaped YAML (`apiVersion: krsm.io/v1`,
`kind: TaskContract`, `metadata`, `spec.allow: [{dim …}]`, `maxSeverity`). DESIGN §7's package
layout puts a **`scope/` package** — "TaskContract → ScopePredicate compiler", marked *public,
embeddable* — alongside the `closure` engine. ADR-0008 then gave the *engine* the
dimension-typed `closure.ScopeClause` and the `resource` and `selector` dimensions, and named
the compiler and the `scope/` package as the next step on that trajectory.

Until now there was no user-facing scope contract: scope was assembled ad hoc by
`internal/scenario.parseScope` into a `[]closure.ScopeClause`. The `TaskContract` (the *authored*
contract — envelope, metadata, `maxSeverity`, room for un-built dimensions) and the
`closure.ScopeClause` (the *compiled* engine input) are different things, and there was no seam
between them. ADR-0002/ADR-0005 impose a hard constraint, enforced by `internal/archguard`: the
public, embeddable SDK must stay **stdlib-only** — no `k8s.io/...`, no `sigs.k8s.io/yaml` — so an
agent-builder can import it without taking the Kubernetes dependency.

Open sub-decisions: where YAML parsing lives (in the embeddable package or the loader); the
`ScopePredicate` shape (named struct vs a bare `[]closure.ScopeClause` alias); whether `Compile`
takes bytes or a struct; how strict the envelope check is; and what an unknown/un-built dimension
does at the contract boundary.

## Decision

Add a thin **compiler in front of the existing engine** in a new public `scope/` package; do not
re-model scope.

- **A public, embeddable, stdlib-only `scope/` package.** It defines `TaskContract`
  (`APIVersion`, `Kind`, `Metadata`, `Spec{Allow []AllowClause, MaxSeverity Severity}`),
  `AllowClause` (dim-typed: `resource` and `selector` now, fields for `ownership`/`namespace`/
  `reference` added by later slices), a `Severity` enum, and `ScopePredicate`. It imports **only
  `closure` + the standard library**; a new `archguard` invariant (`TestScopeIsStdlibOnly`)
  guards this, a direct parallel of `TestClosureIsStdlibOnly`.
- **`Compile` takes the struct, not YAML bytes.** `func Compile(TaskContract) (ScopePredicate,
  error)`. Parsing the §6 wire form lives in `internal/scenario` (`parseTaskContract`), the one
  place that already depends on `sigs.k8s.io/yaml`, so the embeddable SDK pulls in no YAML
  dependency — the same boundary ADR-0005/§7 guard for `closure`.
- **`ScopePredicate` is a named struct** `{Clauses []closure.ScopeClause; MaxSeverity Severity}`,
  not a bare alias — so `maxSeverity` and future per-predicate metadata have a home. `maxSeverity`
  is **parsed and carried but not enforced** this slice (severity `B` is a later concern); it is
  validated against the known set so a typo is caught now.
- **`closure.Safe` is unchanged.** It keeps taking `[]closure.ScopeClause`; callers pass
  `predicate.Clauses` to it. The compiler sits *above* the engine, it does not reshape its API —
  no second back-to-back public break.
- **The compiler owns the dim→clause mapping and fails closed.** `resource` →
  `closure.ResourceClause(gvk, ns, name)`; `selector` → `closure.SelectorClause(gvk, ns,
  selector)`; an empty `Dim` reads as `resource` (parity with `closure`'s back-compat default).
  Each clause is built preserving the authored fields, then run through
  `closure.ScopeClause.Validate`. `Compile` returns an error (all wrapped) on: a wrong
  `apiVersion`/`kind`; an `AllowClause.Dim` outside `{resource, selector}` (incl. the not-yet-built
  `ownership`/`namespace`/`reference` → explicit "unsupported scope dimension", **not** a silent
  skip); a clause that fails `Validate`; or an unknown `maxSeverity`. An empty `spec.allow`
  compiles to an empty predicate (a contract authorising nothing) with **no** error — legal and
  fail-safe (everything escapes → Block).
- **The loader prefers the contract.** `Load(dir)` parses and compiles `dir/taskcontract.yaml`
  when present (using `predicate.Clauses` as the scenario's scope), else falls back to the legacy
  `scope.yaml` → `parseScope` path; `os.IsNotExist` distinguishes "no contract" from a real read
  error. `Scenario.Scope` stays `[]closure.ScopeClause`.

## Consequences

- **The DESIGN §6–§7 contract boundary exists, end-to-end.** A real `TaskContract` YAML loads,
  compiles, and classifies the corpus; new golden scenario `21-taskcontract-selector-scope`
  proves the compiler path returns Allow with the same three-ref closure scenario 20 pins via a
  hand-written `scope.yaml`. Scenario 20 keeps golden coverage of the `scope.yaml` selector path,
  so the new scenario is purely additive.
- **The SDK stays stdlib-only and embeddable.** `scope` depends only on `closure`; YAML parsing
  stays in the loader. `internal/archguard` now guards two packages (`closure` and `scope`); both
  invariants pass.
- **Fail-closed at the contract boundary.** An authored-but-unbuildable scope (a wrong envelope,
  an un-built dimension, a malformed or invalid clause, a typo'd severity) is a hard compile error
  — stricter than the engine's defensive "match nothing", because at the *contract* layer the
  author should learn immediately rather than receive a silently narrowed scope.
- **Adding a dimension later is local.** A new dimension is a new `case` in the compiler's
  dim→clause mapping plus new `AllowClause` fields — no caller changes, no `Safe` change.
- **A faithful contract must fully qualify the GVK.** The engine seeds the closure from the
  action target's own ref (`closure.Closure`) and gates a resource clause on Group/Version when
  specified, so scenario 21's request fully qualifies its target (`group: apps, version: v1`) to
  match the DESIGN §6-faithful `gvk` in the contract — otherwise the bare-GVK target would escape
  the fully-qualified clause. This is the realistic shape and is noted in the scenario.
- **Back-compatible.** All 20 existing golden scenarios stay byte-for-byte green; the loader
  change is additive (the legacy `scope.yaml` path is untouched) and `closure.Safe`'s signature is
  unchanged.
- **Naming note.** `scope.ScopePredicate` is the authoritative public name (DESIGN §7); revive's
  exported-stutter heuristic is narrowly silenced for it rather than renamed, since "predicate" is
  the domain noun.
- **Follow-on (later v0.3 slices, explicit non-goals here):** the `ownership`, `namespace`, and
  `reference` dimensions; enforcing `maxSeverity` (severity `B`); and inexpressible-scope gap
  signalling (DESIGN §6). This slice introduces the package boundary, the compile/validate seam,
  and the two dimensions that exist today, on the design's committed trajectory.
