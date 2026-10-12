# 3. Supply task scope via a TaskContract CRD

Date: 2026-06-18

## Status

Accepted

## Context

`Safe(T, S, A)` needs the task's authorised scope `scope(T)`, but Kubernetes admission requests carry no native notion of "the task this action belongs to." The scope must travel with the action, be auditable, and be hard for a buggy agent to mis-state silently.

## Decision

Supply scope through a **`TaskContract` custom resource** that the agent references from the request (via annotation/label); KRSM resolves it from an informer over the CRD. Inline signed-token and out-of-band session-map mechanisms are documented fallbacks, not the default.

## Consequences

- **Declarative and auditable.** The authorised scope is a cluster object with history, not hidden request state.
- **Cluster-native.** Resolved from an informer, consistent with the state path; no extra synchronous reads on the hot path.
- **Open question — trust.** If the agent both creates the `TaskContract` and acts under it, a compromised agent could over-grant. Who may create/sign a contract, and whether KRSM validates it against an issuer policy, is deferred to the threat model ([SECURITY.md](../../SECURITY.md)) and may produce a follow-up ADR.

## Amendment (2026-10-12)

The decision stands: scope still arrives through a `TaskContract` CR (`krsm.io/v1alpha1`) that the request references, now as the Level-3 rung of ADR-0011's progressive channel (`krsm.io/scope: "<ns>/<name>"`, read from the request's `oldObject`). One part of the text above changed during implementation:

- **Resolution is a live GET, not an informer.** The webhook reads the referenced contract with a deadline-bounded live GET through the dynamic client (`webhook/contract_getter.go`), and any failure fails closed as `scope-unresolved`. Slice-6 design rev 2 made this change because an informer cache creates a revocation window: a deleted or tightened contract would keep authorising until the cache caught up, which is a false ALLOW. The cost is one synchronous read on the opt-in Level-3 path. The "no extra synchronous reads on the hot path" consequence above still holds for the closure state, which remains informer-backed. See [the slice-6 design](../design/v0.5-slice6-scope-channel.md).

The trust question above is still open. For v0.5 it is answered only partly, and the residuals are explicit (details in [SECURITY.md](../../SECURITY.md#scope-channel-residuals-v05)):

- **(a) Bearer capabilities.** Contract and target references are honoured because they are present, bounded to the target's namespace (and, for a re-root, to the target's owner ancestors) and to `oldObject`. Identity/UID/expiry binding is deferred to #42.
- **(b) A contract's grant is not bounded by its own namespace.** The control is authorship: no agent identity may hold create/update/patch on `taskcontracts`, which slice 7 (#25) RBAC must enforce.
- **(c) Annotate now, use next.** An annotation added in one request governs the next request on that object. With the Level-1 bound this reaches only the target's own owner tree; tracked with #42.
