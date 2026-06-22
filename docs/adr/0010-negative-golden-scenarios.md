# 10. Negative golden scenarios (`loadError`) for fail-closed proofs

Date: 2026-06-22

## Status

Accepted

## Context

KRSM is a safety control, so its *fail-closed* behaviour is as important as its
verdicts — DESIGN §5 and ADR-0009 require that an authored-but-unbuildable scope (a
wrong `apiVersion`/`kind`, an unsupported scope dimension, a structurally invalid
clause, an unknown `maxSeverity`) is a hard error rather than a silently narrowed
(allow-nothing) or partial scope. `scope.Compile`'s rejection paths are covered by
unit tests (`scope/scope_test.go`), but the *end-to-end* loader path — a real
`taskcontract.yaml` on disk that the loader parses, compiles, and refuses — had no
golden-corpus artifact. The corpus is the language-agnostic spec and the thesis
evidence base (DESIGN §8); a fail-closed proof belongs in it, not only in a Go unit
test.

The obstacle is the directory-driven golden runner (`internal/scenario/golden_test.go`):
`TestScenarios` and `TestClosureBoundedByInventory` iterate **every** directory under
`closure/testdata/scenarios/` and require `Load(dir)` to *succeed* (then assert a
verdict / the `|C| ≤ |R|` bound). A scenario whose `Load` must *fail* by design would
break both loops. So a negative scenario needs to be a first-class, declared kind —
not an ad-hoc dir the runner chokes on.

## Decision

Introduce a **negative golden scenario** marked by an optional `loadError` field in
`expected.yaml`, and teach the runner to treat it as first-class.

- **`expected.yaml` gains `loadError: <substring>`.** When non-empty, the scenario
  asserts that `Load(dir)` returns a non-nil error whose message **contains** the
  substring; `verdict`/`closure`/`escaping`/`external` are then absent (there is no
  computable closure for a contract that never compiles). The substring is matched
  against the wrapped error chain (e.g. `loadScope`'s `compile taskcontract: %w`
  around `scope.Compile`'s `unsupported scope dimension %q`).
- **`TestScenarios` loads `expected.yaml` first** and branches: a non-empty
  `loadError` asserts the failing `Load` and returns; otherwise the path is unchanged.
- **`TestClosureBoundedByInventory` skips** any `loadError` scenario before requiring
  `parseCluster`/`Load` to succeed — the inventory bound is undefined when `Load`
  fails.
- **The negative scenario is a complete, valid-prefix artifact.** `cluster.yaml` and
  `request.yaml` must parse cleanly so execution *reaches* the failure under test (the
  contract compile), making the failure unambiguous. The first such scenario,
  `22-taskcontract-fail-closed`, carries a well-formed `taskcontract.yaml` that uses
  `dim: ownership` — valid *future* syntax (DESIGN §6), not yet built — so
  `scope.Compile` rejects it with "unsupported scope dimension". It deliberately omits
  `scope.yaml` so the TaskContract path (not the legacy fallback) is exercised.

## Consequences

- **Fail-closed is now corpus-proven end-to-end.** The loader→compiler rejection is a
  golden artifact, not only a unit test, strengthening the corpus as thesis evidence
  (DESIGN §8, §11) and as a regression guard against a future silent over-grant.
- **The convention is additive and back-compatible.** The 21 existing scenarios have
  no `loadError`, so they take the unchanged path byte-for-byte; the only reordering is
  that `TestScenarios` reads `expected.yaml` before `Load` (both still run for a normal
  scenario).
- **Adding a negative scenario is a directory, like any other.** New fail-closed proofs
  (a bad envelope, an invalid clause, an unknown `maxSeverity`, a malformed YAML) are a
  dir + a `loadError` line — no runner change.
- **`fuzz_test.go` is unaffected:** it seeds only from `cluster.yaml`/`request.yaml`/
  `scope.yaml` with missing-file guards, so a negative scenario's absent `scope.yaml`
  and unread `taskcontract.yaml` contribute no seed.
- **Scope of the marker.** `loadError` asserts a *load/compile* failure, not a runtime
  verdict. A scenario that should *load* but yield a fail-closed **Block** (e.g. an
  unresolvable target) remains a normal scenario asserting `verdict: Block` with a
  `reason` substring — the two fail-closed kinds stay distinct (DESIGN §5).
