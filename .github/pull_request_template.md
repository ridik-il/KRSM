## What & why
<!-- One paragraph: what this changes and the motivation. Link the issue (#NN) —
     non-trivial work starts from an issue per CONTRIBUTING. -->

Closes #

## Design
- [ ] Trivial change, OR a design decision is captured/updated in an ADR (`docs/adr/`) in this PR.
- [ ] Docs (DESIGN / ROADMAP / CHANGELOG) updated if behaviour changed.

## Tests
- [ ] New behaviour has tests. Closure-engine changes add/adjust a golden scenario under
      `closure/testdata/scenarios/`.
- [ ] `make check` passes locally (fmt + vet + lint + staticcheck + `go test -race`).

## SDK purity (if `closure/` or `scope/` touched)
- [ ] No new dependency added to `closure` — it stays standard-library-only (`internal/archguard` passes).

## Verdict-path integrity
- [ ] No model/LLM introduced into the allow/deny decision path (DESIGN §1 non-goal).
