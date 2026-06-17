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
