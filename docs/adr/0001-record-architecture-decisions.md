# 1. Record architecture decisions

Date: 2026-06-18

## Status

Accepted

## Context

KRSM is both a tool and the engineering artifact of a doctoral thesis. Design decisions need to be discoverable, justified, and revisitable — by future contributors, by reviewers, and by the thesis itself, where the implementation must be shown to satisfy the formal model.

## Decision

We use Architecture Decision Records (ADRs) — short, numbered, append-only documents in `docs/adr/`, one per significant decision. A decision that changes is superseded by a new ADR, not edited away. Format: Context → Decision → Consequences.

## Consequences

- The reasoning behind the architecture is part of the repo and reviewable in PRs.
- Drift between the code and the research's formal definitions is caught where decisions are recorded.
- Small overhead per significant change; trivial changes need no ADR.
