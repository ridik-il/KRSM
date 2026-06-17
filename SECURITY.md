# Security Policy

KRSM is a security control for Kubernetes clusters, so we take its own security seriously.

## Reporting a vulnerability

**Please do not report security vulnerabilities through public GitHub issues.**

Instead, report privately via [GitHub Security Advisories](../../security/advisories/new) (preferred) or by email to the maintainer. Include:

- a description of the issue and its impact,
- steps to reproduce (a minimal scenario is ideal),
- affected version/commit.

We aim to acknowledge reports promptly and to coordinate a fix and disclosure timeline with you.

## Scope and threat model (early)

KRSM's security posture is part of its design — see [docs/DESIGN.md §9](docs/DESIGN.md). Areas of particular interest:

- **Scope-channel trust.** If a task's authorised scope is agent-supplied, a compromised or buggy agent could declare an over-broad `TaskContract`. Who may create/sign a contract, and how KRSM validates it against an issuer policy, is an open design decision ([adr/0003](docs/adr/0003-scope-channel.md)).
- **Webhook attack surface.** The admission webhook validates every `AdmissionReview` strictly and holds itself to the standard it enforces.
- **Fail-closed default.** When KRSM cannot compute an action's closure, it denies — an unknown blast radius must never be silently admitted.
- **State staleness.** Verdicts run against an informer-backed snapshot; the watch-vs-persistence race and its mitigations are tracked in [adr/0004](docs/adr/0004-state-freshness.md).

Findings that sharpen this threat model are very welcome.
