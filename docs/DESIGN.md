# KRSM — Design

Status: **stable draft** · Audience: contributors and reviewers · Companion: [ROADMAP.md](ROADMAP.md), [adr/](adr/)

This document explains *what KRSM is, how it is built, and why it is built that way*, in that order. It is meant to be read top-to-bottom by someone new to the project. Precise-but-plain throughout; the formal treatment lives in the research, and is referenced where relevant.

---

## 1. Purpose and non-goals

**Purpose.** Decide, **before** a Kubernetes action reaches the API server, whether the action's *affected-resource closure* over live cluster state stays within the slice of infrastructure its task was authorised to touch — and deny it (with an explanation) if not.

**In scope.**
- Pre-execution verdicts for actions issued by autonomous remediation agents.
- The four relation types through which effects propagate (§3).
- A declarative scope contract per task.
- An explanation: *which* resources escaped scope.

**Non-goals (explicitly out of scope, by design).**
- **Not a generator.** KRSM does not write or fix manifests; it judges actions. (The agent proposes; KRSM disposes.)
- **Not an LLM judge.** The verdict is a decision procedure over live state. No model is in the verdict path.
- **Not post-hoc.** Audit-after-execution is a different (already-too-late) problem.
- **Not a general policy engine.** KRSM checks *task-scope conformance*, not arbitrary org rules (OPA/Kyverno do that, and KRSM composes with them).
- **Not (yet) the research apparatus.** The benchmark harness, multi-tool comparison, and scale study belong to the research, not this tool — see [ROADMAP §Deferred](ROADMAP.md).

## 2. The core idea

Whether an action is in-scope is **not a property of its syntax** — it is a property of the **live relational state** of the cluster when the action fires. Two identical `delete deployment web` requests have different blast radii depending on what currently exists and what currently references it.

KRSM makes that blast radius explicit:

> **affected-resource closure `C(S, A)`** — start at the resources the action `A` names, then walk the relations of the live state `S`, collecting every resource the action will actually affect.
>
> **conformance `Safe(T, S, A) ≡ C(S, A) ⊆ scope(T)`** — the action is safe for task `T` iff its closure stays inside the task's authorised scope.

Because syntactic mechanisms never read `S`, there is a class of violations they *cannot* detect. KRSM reads `S` and can.

## 3. The model

The vocabulary (kept deliberately small):

| Symbol | Meaning | Concretely |
|---|---|---|
| `R` | the live resource inventory | objects KRSM tracks, keyed by **GVK + namespace + name** (and matched by `uid`) |
| `A` | the proposed action | verb + target + payload (old/new object) + delete-propagation |
| `S` | live state | `R` plus the per-object fields the relations are read from |
| `G(S)` | the relation graph over `S` | four typed edge kinds (below) |
| `T` | the task contract | a declared, per-task authorised scope |
| `scope(T)` | the authorised set | what `T` compiles to: the resources the task may affect |
| `C(S, A)` | the closure | resources `A` actually affects, by walking `G(S)` |
| `Safe(T, S, A)` | the verdict | `C(S, A) ⊆ scope(T)` — the allow/deny decision |
| `B(S, A)` | severity (reported, not enforced) | how dangerous the closure looks (irreversibility, tenant span, …) |

### The four relations (`G(S)`)

Effects propagate along these edges; closure follows them in the direction effects flow (which is sometimes the *reverse* of the reference):

| # | Relation | Edge | Read from |
|---|---|---|---|
| 1 | **ownerReferences / cascade** | owner → child | `metadata.ownerReferences` on the child (matched by `uid`) |
| 2 | **namespace containment** | namespace → contained object | `metadata.namespace`; deleting a Namespace affects all it contains |
| 3 | **label-selector binding** | selector-owner → matched Pods | `Service.spec.selector`, `NetworkPolicy.spec.podSelector`, workload/PDB `spec.selector`; Pod `metadata.labels` |
| 4 | **cross-resource reference** | consumer → referenced object | Pod `volumes[]`/`envFrom[]`/`env[].valueFrom`; `metadata.finalizers`; HPA `spec.scaleTargetRef` |

## 4. The closure algorithm

A worklist (BFS) from the action's target, following only the relations the action's **effect class** licenses:

- a **cascading delete** follows owner→child (and namespace→contents for a Namespace target);
- a **disruptive verb on a workload** follows workload→selected Pods;
- a **selector/label mutation** unions the old and new match-sets (this needs the request payload — old vs new);
- a **mutation of a ConfigMap/Secret/PVC** follows referenced-object→consumers (reverse of rel 4);
- a **finalizer removal** flags the guarded external resource (closure can only *warn* across the cluster boundary).

**Termination / decidability.** Each resource is expanded at most once (visited-set guard), so the walk is finite (`|C| ≤ |R|`); therefore `C` is computable and `Safe` (a finite subset test) is decidable. *(Proven formally in the research; the implementation must preserve this property.)*

**Performance.** Naïve adjacency = a full scan per node = `O(c·n)` in cluster size `n` — fine for an offline oracle, **too slow** for an admission webhook at 10⁵ resources. The production engine uses **incremental, informer-fed indexes** (owner, namespace, selector inverted-index, reverse cross-ref) so closure costs `O(c·d)` — proportional to *blast radius*, not cluster size. This is the property that keeps verdicts fast.

## 5. Architecture

```
            ┌─────────────────────── krsm (Go) ───────────────────────┐
 API server │  ValidatingWebhook handler                              │
   calls ──▶│     │                                                    │
            │     ├─▶ scope resolver  ── reads TaskContract (informer) │
            │     ├─▶ state index     ◀─ informers/watch over R        │
            │     ├─▶ closure engine  ── C(S,A) over the indexes       │
            │     └─▶ verdict         ── C ⊆ scope(T) ? allow : deny   │
            │                              fail-closed on unknown closure
            └──────────────────────────────────────────────────────────┘
```

- **Interception:** a `ValidatingWebhookConfiguration` — the API server calls KRSM after auth/mutation, before persistence. Pre-execution by construction. Validating (not mutating): KRSM *decides*, it does not rewrite actions.
- **State:** in-process, informer-backed, indexed mirror of the resource types KRSM tracks. A verdict needs **zero synchronous API reads** in the common case; on-demand `GET` is a cold-start fallback only.
- **Scope channel:** the agent stamps the request with a reference to a `TaskContract` (a CRD); KRSM resolves it from an informer. (Inline signed-token and out-of-band session map are fallbacks — see [adr/0003](adr/0003-scope-channel.md).)
- **Failure mode:** **fail-closed** when KRSM cannot compute the closure (cache cold, scope unresolvable, internal error) — an unknown blast radius must never be silently admitted. Distinguish *“closure says escape”* (deny, expected) from *“cannot compute closure”* (deny, error) in the response reason.
- **Enforcement target:** a `namespaceSelector`/`objectSelector` limits KRSM to agent-originated requests, so it doesn't gate humans or cluster controllers.

## 6. The scope-contract language (`TaskContract`)

A CRD-shaped declaration that compiles to `scope(T)` as a disjunction of dimension-typed allow-clauses, one per relation dimension plus a flat identity dimension:

```yaml
apiVersion: krsm.io/v1
kind: TaskContract
metadata: { name: relieve-web-1-memory, namespace: prod }
spec:
  allow:
    - dim: resource                 # flat identity (glob allowed)
      gvk: { group: "", version: v1, kind: Pod }
      namespace: prod
      name: web-1
    - dim: ownership                # a root and everything it owns
      root: { gvk: { group: apps, version: v1, kind: Deployment }, namespace: prod, name: web }
      includeDescendants: true
    - dim: selector                 # pods a selector covers
      gvk: { group: "", version: v1, kind: Pod }
      namespace: prod
      matchLabels: { app: web }
  maxSeverity: high                 # reported via B; does not change the verdict
```

`ownership`/`selector`/`reference` clauses are **state-dependent set definitions** evaluated against the *same snapshot* as the closure, so "the web Deployment and everything it owns" expands consistently. Scopes that cannot be expressed (e.g. set-difference "all `app=web` Pods *except* canaries") surface an explicit **expressiveness gap** rather than forcing a silent over-grant — informing both the user and the research.

## 7. Repository layout

```
cmd/krsm/          CLI entrypoint (offline checker, dev tool)
closure/           the closure engine: model types, G(S), C(S,A), Safe   [public, embeddable]
scope/             TaskContract → ScopePredicate compiler                 [public, embeddable]
state/             informer-backed indexed state provider (client-go)
webhook/           validating admission webhook server
internal/          unexported helpers
testdata/          golden-file scenarios (the KRSM failure-mode corpus)
docs/              this design, the roadmap, ADRs
```

Public packages (`closure`, `scope`) are the **embeddable SDK** an agent-builder can import directly; the webhook is the turnkey deployment.

## 8. Testing strategy

- **Golden-file scenarios.** Each failure-mode scenario is a `(state, action, scope, expected verdict + escaping set)` tuple under `testdata/`. The corpus is language-agnostic YAML, reused verbatim as the spec. A reference Python oracle (from the research repo) seeds these and serves as a parity check during the Go port.
- **Unit tests** per relation type and per effect class (table-driven, `go test -race`).
- **Property:** `|C| ≤ |R|` and termination on adversarial/cyclic inputs.
- **End-to-end on `kind`:** an agent-stamped escaping action is denied pre-persistence; an in-scope one is admitted.
- CI runs `gofmt`, `golangci-lint`, `staticcheck`, `go test -race ./...` on every change.

## 9. Security considerations

KRSM is a security control, so its own posture matters:
- **Scope-channel trust.** If `scope(T)` is agent-supplied, a compromised agent could declare an over-broad contract. Who may create/sign a `TaskContract`, and whether KRSM verifies it against an issuer policy, is a threat-model decision tracked in [SECURITY.md](../SECURITY.md) and [adr/0003](adr/0003-scope-channel.md).
- **Webhook as attack surface.** Strict schema validation on every `AdmissionReview`; the webhook holds itself to the standard it enforces.
- **Fail-closed default** (§5) so an exploit that crashes closure computation cannot become an open door.
- **Staleness vs the watch race.** Between informer snapshot and persistence, neighbours can change; mitigations (resync bounds, `resourceVersion` checks, fail-closed on detected drift) are tracked as an open question in [adr/0004](adr/0004-state-freshness.md).

## 10. Key decisions (see [adr/](adr/))

- **[0001]** Record architecture decisions (this ADR process).
- **[0002]** Implement in **Go/client-go** — ecosystem fit, informer maturity, latency, and direct embeddability by (Go-based) agent builders.
- **[0003]** Supply scope via a **`TaskContract` CRD** referenced from the request.
- **[0004]** **Fail-closed** on unknown closure; informer-backed state with `GET` fallback.

## 11. Relationship to the research

This repo is the **engineering contribution** of the KRSM doctorate. The model (`R, A, S, G(S), C, Safe, B`), the computability/decidability results, and the false-negative lower bound are proven in the thesis; this code must *satisfy* those properties (it is the artifact the theory describes). Where the implementation and the formal definitions could drift, the formal definitions win and the code is reconciled to them. Research-only apparatus (benchmark labelling study, multi-tool comparison, 10²–10⁵ scale measurement) is deliberately **not** in this repo.
