<div align="center">

# KRSM

**A pre-execution safety gate for autonomous Kubernetes agents.**

*Kubernetes Remediation Scope Model — stop an AI agent's action before it runs if its real blast radius escapes the task it was given.*

[![CI](https://github.com/ridik-il/KRSM/actions/workflows/ci.yml/badge.svg)](https://github.com/ridik-il/KRSM/actions/workflows/ci.yml)
[![CodeQL](https://github.com/ridik-il/KRSM/actions/workflows/codeql.yml/badge.svg)](https://github.com/ridik-il/KRSM/actions/workflows/codeql.yml)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/ridik-il/KRSM/badge)](https://securityscorecards.dev/viewer/?uri=github.com/ridik-il/KRSM)
[![Go Reference](https://pkg.go.dev/badge/github.com/ridik-il/krsm.svg)](https://pkg.go.dev/github.com/ridik-il/krsm)
[![License](https://img.shields.io/badge/license-Apache--2.0-green)](LICENSE)
[![Status](https://img.shields.io/badge/status-early%20design-orange)](docs/ROADMAP.md)

</div>

---

> **Status: early, building in the open.** The closure engine and its failure-mode corpus are
> implemented and runnable today via `krsm check` (see the demo below); the live-cluster reads and
> the admission webhook are next (see [ROADMAP](docs/ROADMAP.md)). The design and roadmap are stable.
> Stars and feedback welcome — issues especially.

## The problem in 15 seconds

Companies increasingly let AI agents *operate* their clusters: an alert fires, the agent issues a Kubernetes API command to fix it — restart, patch, scale, delete. The danger is that **a single, perfectly legal command can do far more than intended**, because what an action *actually affects* is decided by the **live shape of the cluster**, not by the text of the request.

```
Task given to the agent:   "relieve memory pressure on one Pod: web-1"
Agent's action:            kubectl delete deployment web      # syntactically fine
What actually happens:     ownerReference cascade → deletes web-1, web-2, web-3
                           + the Service selecting app=web silently loses all endpoints
Result:                    a full outage, from a task scoped to one Pod
```

**RBAC, OPA/Gatekeeper, and Kyverno cannot catch this.** They inspect the *syntax of a single request* — they never look at live cluster relations, so they cannot see the cascade. KRSM does.

## What KRSM does

KRSM computes an action's **affected-resource closure** — the full set of resources the action will actually touch — by walking the live cluster's relations from the action's target:

1. **ownerReferences** (cascade deletion: Deployment → ReplicaSet → Pods)
2. **namespace containment**
3. **label-selector bindings** (a Service "owns" whichever Pods its selector currently matches)
4. **explicit cross-resource references** (volume mounts, env/`envFrom`, finalizers, `scaleTargetRef`)

It then checks one thing, **before the action reaches the API server**:

> **`Safe(T, S, A)` ⟺ closure(A) ⊆ scope(T)**
> *Is everything this action affects inside the slice the task was authorised to touch?*

If yes, allow. If no, **deny — and return exactly which resources escaped scope**. The verdict is a decision over live state, never an LLM's opinion: *the agent proposes, an inspectable closure disposes.*

```
$ krsm check closure/testdata/scenarios/01-memory-pressure-cascade
ACTION   delete Deployment/prod/web
SCOPE    Pod/prod/web-1
CLOSURE  Deployment/prod/web, Pod/prod/web-1, Pod/prod/web-2, Pod/prod/web-3, ReplicaSet/prod/web-7f9, Service/prod/web-svc
VERDICT  ❌ BLOCK — affected-resource closure escapes task scope:
           → Deployment/prod/web
           → Pod/prod/web-2
           → Pod/prod/web-3
           → ReplicaSet/prod/web-7f9
           → Service/prod/web-svc
$ echo $?
2
```

Every failure-mode scenario under [`closure/testdata/scenarios/`](closure/testdata/scenarios)
is a runnable demo: `krsm check <dir>` loads the scenario's `cluster.yaml` / `request.yaml` /
`scope.yaml`, computes the closure, and prints the verdict. Exit code is **2** on a block,
**0** on allow/warn (a `WARN` and its cross-boundary detail go to stderr), **1** on a usage or
load error — so it scripts cleanly.

## How it works

```
                 ┌──────────────────────────────────────────────┐
   agent issues  │  Kubernetes API server                       │
   an action ───▶│   └─ ValidatingWebhook ──▶  KRSM             │
                 │                              │  closure(A) over│
                 │        allow / deny ◀────────┤  live G(S),     │
                 │                              │  check ⊆ scope(T)│
                 └──────────────────────────────┴────────────────┘
                                                 ▲
                          informer-backed, indexed mirror of live state
```

KRSM runs as a **validating admission webhook**: the API server calls it *after auth, before persistence*, so enforcement is pre-execution by construction. State comes from an in-process, informer-backed index, so a verdict needs no synchronous API calls and stays within a tight latency budget. The task's authorised scope is declared via a `TaskContract` resource the agent references.

See **[docs/DESIGN.md](docs/DESIGN.md)** for the full architecture and **[docs/ROADMAP.md](docs/ROADMAP.md)** for the build plan.

## Why this is hard to copy

- **Independence.** KRSM is not the agent and not the generator — it's the third-party check. The verdict is a decision procedure over live state, not another model's guess.
- **Live-state closure** is precisely what syntactic policy engines *structurally cannot* compute — a property with a formal lower bound (see the research below), not just an engineering gap.
- **It works with any agent** (Copilot/Claude/Cursor/hand-written) — KRSM is generator-agnostic.

## The research behind it

KRSM is also the artifact of an ongoing **Doctor of Engineering Sciences** thesis — *Formal Task-Scoped Safety for Autonomous Cloud Infrastructure Agents: A Theory of Affected-Resource Closure*. The theory proves that purely syntactic admission control has an irreducible false-negative floor on this class of actions, and that closure over live relations is computable and decidable within a stated boundary. The code is the engineering contribution; the proofs and benchmark are the scientific one. (Research planning lives outside this repo; this repo is the tool.)

## Project status & roadmap

Building in vertical slices, each shippable on its own — see **[ROADMAP](docs/ROADMAP.md)**:

1. Faithful closure engine (the four relations) · 2. The failure-mode test corpus (runnable via `krsm check`) · 3. `TaskContract` scope language · 4. Live-cluster reads · 5. The admission webhook · 6. OSS polish.

## Contributing & security

- [CONTRIBUTING.md](CONTRIBUTING.md) — how to build, test, and propose changes.
- [SECURITY.md](SECURITY.md) — responsible disclosure (KRSM is a security tool; please read).

## License

[Apache-2.0](LICENSE) — permissive, with a patent grant, matching the Kubernetes ecosystem and allowing embedding in commercial agents.
