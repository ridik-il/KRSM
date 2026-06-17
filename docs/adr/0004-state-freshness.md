# 4. Informer-backed state with a fail-closed default

Date: 2026-06-18

## Status

Accepted

## Context

Closure must be computed against live state fast enough for the admission path, without per-request API fan-out. But cached state can be stale relative to the moment of persistence (the watch-vs-write race), and KRSM must never admit an action whose true blast radius it does not know.

## Decision

Maintain an **informer-backed, incrementally indexed** mirror of the tracked resource types; compute closure against it with **zero synchronous API reads** in the common case, falling back to on-demand `GET` only on cold start / cache miss. **Fail closed**: when KRSM cannot compute the closure (cache cold, scope unresolvable, internal error), **deny**, and distinguish *"closure says escape"* from *"cannot compute closure"* in the response reason.

## Consequences

- Fast verdicts that scale with blast radius, not cluster size (the indexes give `O(c·d)`).
- A crash or gap in closure computation becomes a denial, not an open door.
- **Open question — staleness.** Neighbours read from the index may have changed since the snapshot. Mitigations to evaluate: bounded informer resync, `resourceVersion` checks, fail-closed on detected drift. Tracked here until resolved.
