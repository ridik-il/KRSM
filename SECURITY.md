# Security Policy

KRSM is a security control for Kubernetes clusters, so we take its own security seriously.

## Supported versions

KRSM is pre-1.0. Security fixes land on `main` and in the latest tagged release;
older tags are not back-patched.

## Reporting a vulnerability

**Please do not report security vulnerabilities through public GitHub issues.**

Report privately through one of these channels:

- **Preferred:** open a private advisory at <https://github.com/ridik-il/KRSM/security/advisories/new>.
- **Email:** ilyasov.2003@gmail.com.

Include:

- a description of the issue and its impact,
- steps to reproduce (a minimal scenario is ideal),
- affected version/commit.

## Disclosure timeline

We follow coordinated disclosure:

- **Within 3 days** — acknowledge your report.
- **Within 14 days** — confirm the issue and share an assessment and a fix plan.
- **Within 90 days** — release a fix and publish an advisory, crediting you unless
  you ask otherwise. We will agree any deviation from this window with you.

## Scope and threat model (early)

KRSM's security posture is part of its design — see [docs/DESIGN.md §9](docs/DESIGN.md). Areas of particular interest:

- **Scope-channel trust.** If a task's authorised scope is agent-supplied, a compromised or buggy agent could declare an over-broad `TaskContract`. Who may create/sign a contract, and how KRSM validates it against an issuer policy, is an open design decision ([adr/0003](docs/adr/0003-scope-channel.md)). The v0.5 webhook's known residuals are listed under [Scope-channel residuals (v0.5)](#scope-channel-residuals-v05) below.
- **Webhook attack surface.** The admission webhook validates every `AdmissionReview` strictly and holds itself to the standard it enforces.
- **Fail-closed default.** When KRSM cannot compute an action's closure, it denies — an unknown blast radius must never be silently admitted.
- **State staleness.** Verdicts run against an informer-backed snapshot; the watch-vs-persistence race and its mitigations are tracked in [adr/0004](docs/adr/0004-state-freshness.md).

Findings that sharpen this threat model are very welcome.

## Scope-channel residuals (v0.5)

The webhook's progressive scope channel (slice 6, [adr/0003](docs/adr/0003-scope-channel.md), ADR-0011) reads two governing annotations from the request's `oldObject`: `krsm.io/scope` (Level 3, names a `TaskContract`) and `krsm.io/target` (Level 1, re-roots the derived ownership tree). Every resolution failure fails closed in both modes (`invalid` for malformed syntax, otherwise `scope-unresolved`). The following widening paths are **known and accepted for v0.5**. They are stated here so that operators can deploy around them.

- **(a) Contract and target references are bearer capabilities.** KRSM honours a reference because it is present, not because of who wrote it. The bounds are:
  - a `krsm.io/scope` reference must name a contract in the request target's own namespace (a cluster-scoped request cannot use Level 3);
  - a `krsm.io/target` re-root must be the target itself or an owner ancestor of it in the target's namespace;
  - both are read from `oldObject` only.

  Within those bounds, any identity that can annotate an object can point it at any contract in that namespace. Binding a reference to the requester's identity, a contract UID and an expiry is deferred to [#42](https://github.com/ridik-il/KRSM/issues/42).
- **(b) A contract's grant is not bounded by its own namespace.** The same-namespace rule bounds *where a contract lives*, not *what it grants*. A `TaskContract` in `prod` may carry a `dim: namespace` clause for `kube-system`, or an ownership root on a cluster-scoped object. This is deliberate, because operator-authored contracts legitimately need such grants. The control is who may author contracts. **Hard deployment requirement:** no agent identity may hold `create`, `update` or `patch` on `taskcontracts.krsm.io`. Slice 7 ([#25](https://github.com/ridik-il/KRSM/issues/25)) RBAC must enforce this. A deployment that grants an agent write access to contracts lets that agent authorise anything.
- **(c) An agent can annotate in one request and use the annotation in the next.** The `oldObject`-only rule stops an UPDATE from authorising *itself*: an annotation added in a request does not govern that request. A later request on the same object does see it. An agent that may update an object can therefore add `krsm.io/target` (admitted under the derived scope) and act under the re-rooted scope next. With the Level-1 bound in (a), this reaches only the target's own owner tree in its namespace, which is the documented Level-1 use. The same two-step can add a `krsm.io/scope` reference to any same-namespace contract, which is residual (a). Denying agent edits of governing annotations is tracked with [#42](https://github.com/ridik-il/KRSM/issues/42).
- **(d) Reconfiguring an annotation key makes the old annotations inert.** `--target-annotation` and `--scope-annotation` select the keys KRSM reads. After a change, objects still carrying the old key fall to the Level-0 derived scope of the request's own target, which may be **wider or narrower** than the re-root or contract the old key named. `krsm serve` logs a startup warning for each key that differs from its default. Re-annotate governed objects when changing a key.
