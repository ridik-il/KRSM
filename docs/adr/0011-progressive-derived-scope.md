# 11. Progressive, derived scope (provenance levels + audit-first default)

Date: 2026-06-22

## Status

Accepted

## Context

ADR-0003 / DESIGN §6 / ADR-0009 made a `TaskContract` the **only** source of
`scope(T)`: no contract → no compiled predicate → no verdict → no value. That is an
adoption dead-end. The user journey is "install KRSM → *write a multi-dimension YAML
contract for every task your agent might do* → get value", and almost everyone leaves
at the middle step. It is also the soundness objection a reviewer already raised: if a
human must author the scope for every agent action, KRSM has replaced "autonomous
agent" with "human writes the safety spec." A credible answer to "who writes the
scope?" is needed for both adoption and the thesis.

The key realisation: `scope(T)` need not be **declared**. It can be **derived** from
the action's target plus live cluster topology, because the same five dimensions a
contract declares (`resource`/`ownership`/`namespace`/`selector`/`reference`,
DESIGN §6) can be *synthesized* from what the cluster already knows. The formal
property is untouched — `Safe(T,S,A) ≡ C(S,A) ⊆ scope(T)` holds whatever the
provenance of `scope(T)`; only where `scope(T)` comes from changes. ADR-0008/0009
already named `ownership` and `namespace` as scope dimensions (deferred); this ADR is
what pulls them forward and gives them a zero-config provenance.

One subtlety drives the default choice. KRSM's flagship "aha" scenario
(`01-memory-pressure-cascade`) is **intra-namespace**: `delete deployment web` in
`prod` cascades to sibling Pods + a ReplicaSet + a Service, all in `prod`. So a bare
**namespace-boundary** default (block only when the closure escapes the namespace)
would *Allow* the very example the README leads with. The conservative zero-config
default must therefore be the **ownership tree of the target**, not the namespace.

## Decision

Introduce **progressive scope** — four provenance levels with a derived default and an
audit-first verdict mode — implemented as a thin **scope synthesizer above
`scope.Compile`**, leaving the engine and `closure.Safe` unchanged.

- **Level 0 — derived, zero config.** With no annotation and no contract, KRSM
  synthesizes a conservative scope from the admission request's own target: a single
  `ownership`-dimension clause rooted at the target (the target + everything it owns).
  This catches the intra-namespace cascade (scenario 01 — the Service the deletion
  breaks is *not* owned by the Deployment, so it escapes the tree) *and* cross-namespace
  blast radius (a member in another namespace is likewise outside the tree) with one
  `helm install` and no YAML. **The default is the ownership tree alone — deliberately
  not OR'd with a namespace-containment clause.** Scope is a *union* of allow-clauses
  (`C ⊆ scope` ⇔ every member matches *some* clause), so adding a "everything in the
  target's namespace" clause would re-admit the very same-namespace collateral the tree
  is meant to flag (it would Allow scenario 01), and it is redundant besides — anything
  outside the namespace is already outside the tree. The `namespace` dimension is still
  built (DESIGN §6 commits to it; templates need it), but as an *explicit* scope option,
  not the derived default.
- **Level 1 — target annotation.** When the *task* target differs from the API request
  target, the agent stamps `krsm.io/target: "<Kind>/<ns>/<name>"`; KRSM roots the
  ownership-tree derivation there instead.
- **Level 2 — templates.** A `TaskContract` may set `spec.template`
  (`ownership-tree` | `namespace-contained` | `single-resource` | `selector-bound`)
  plus a `target`/`selector`; the synthesizer expands the template to clauses. One-line
  contracts for common patterns.
- **Level 3 — full contract.** Today's multi-dimension `TaskContract` → `Compile`
  (ADR-0009), unchanged. Power users only.
- **Audit-first verdict mode.** The install default is **audit** (a derived-scope
  escape is a `Warn`, never a deny); **enforce** (`Block`) is opt-in. A day-0 hard
  block on a *derived* scope would fire false positives on legitimate cross-namespace
  references (shared-config namespaces, cluster-scoped objects) and get KRSM
  uninstalled within the hour — the Falco/Kyverno lesson (defaults audit, enforcement
  opt-in). The `Warn` verdict already exists.
- **Cross-boundary escape hatch.** A configurable allowlist of namespaces and
  cluster-scoped kinds that do not count as escapes for *derived* (Level 0/1) modes,
  so legitimate shared-resource traffic is not flagged. A full contract (Level 3) does
  not need it — it states its own scope.
- **Provenance is reported.** Every verdict records how `scope(T)` was obtained
  (`derived:ownership-tree` | `annotation` | `template:<name>` | `contract`) in the
  `Decision.Reason`, so an operator can see *why* a scope was what it was.

## Consequences

- **Install-and-useful.** The first deployable cut (v0.4 live reads + v0.5 webhook in
  audit mode) is valuable with zero contracts — the adoption story becomes "`helm
  install`, then KRSM flags an agent action that would have cascaded beyond its
  ownership tree", not "first write a 20-line YAML." This is the GitHub-stars cut.
- **The formal contribution is unchanged and strengthened.** `C(S,A) ⊆ scope(T)` holds
  at every level; derivation only changes the provenance of `scope(T)`. Derived scope
  is the credible answer to the "who writes the contract?" examiner attack, and *where
  a derived default diverges from human intent* is precisely the SRQ4 underdetermination
  question — a research knob, not just a product feature.
- **It is a reprioritisation, not new theory or new engine.** Every level still needs
  v0.4 (live reads) and v0.5 (webhook) to be the `helm install` story; nothing works
  offline. The only new code is the synthesizer (target + mode + level →
  `[]closure.ScopeClause`); it reuses the `ownership`/`namespace` dimensions and the
  existing compiler. `closure.Safe` and the closure walk do not change.
- **`ownership` and `namespace` dimensions move from deferred to required.** ADR-0008/
  0009 deferred them; Level 0/1 need at least `ownership` now (`namespace` for the outer
  guard). They are built as scope dimensions first (reusing the compiler seam), then the
  synthesizer derives them.
- **A derived scope is deliberately conservative** and may over-approximate (e.g. an
  ownership tree that legitimately spans namespaces). The audit default makes
  over-blocking visible rather than disruptive; enforce mode is an explicit operator
  choice. Templates (Level 2) are pure sugar over the compiler — no engine risk.
- **Supersedes nothing.** ADR-0003/0009 (contract as *a* scope source) still stand;
  this ADR adds three lower-friction provenances beneath the full contract. DESIGN §1's
  "not a generator / not an LLM judge / task-scope conformance" non-goals are unchanged:
  derivation is a topology computation, no model in the path.
