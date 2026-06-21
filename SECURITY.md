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

- **Scope-channel trust.** If a task's authorised scope is agent-supplied, a compromised or buggy agent could declare an over-broad `TaskContract`. Who may create/sign a contract, and how KRSM validates it against an issuer policy, is an open design decision ([adr/0003](docs/adr/0003-scope-channel.md)).
- **Webhook attack surface.** The admission webhook validates every `AdmissionReview` strictly and holds itself to the standard it enforces.
- **Fail-closed default.** When KRSM cannot compute an action's closure, it denies — an unknown blast radius must never be silently admitted.
- **State staleness.** Verdicts run against an informer-backed snapshot; the watch-vs-persistence race and its mitigations are tracked in [adr/0004](docs/adr/0004-state-freshness.md).

Findings that sharpen this threat model are very welcome.
