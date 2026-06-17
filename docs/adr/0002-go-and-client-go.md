# 2. Implement in Go with client-go

Date: 2026-06-18

## Status

Accepted

## Context

KRSM must run as a Kubernetes validating admission webhook with an in-process, indexed mirror of live cluster state, and must return verdicts inside the API server's webhook timeout — at clusters up to ~10⁵ resources. It should also be **embeddable** by the teams most likely to adopt it (builders of autonomous Kubernetes agents). A working reference implementation of the closure engine already exists in Python.

## Decision

Implement the production artifact in **Go using client-go**. Keep the Python implementation as a **reference oracle and spec** that seeds the golden-file scenarios; do not maintain it as a parallel production engine.

## Consequences

- **Ecosystem fit.** Go is the language of the Kubernetes control plane; OPA/Gatekeeper, Kyverno, and controller-runtime are all Go. This aids credibility, contribution, and operational parity.
- **Performance.** client-go shared informers/listers/indexers are the reference implementation of the cached-state pattern KRSM needs; Go gives predictable low-latency on the synchronous admission path.
- **Embeddability.** Agent builders working in Go can import the `closure`/`scope` packages directly as an SDK.
- **No rewrite tax.** Starting in Go avoids a later Python→Go migration.
- **Cost.** The Python engine is not maintained going forward; its value is as the spec and a parity check during the port. Two languages briefly coexist (Go code + Python oracle + language-agnostic YAML scenarios).
- **Caveat.** Earlier research framing named the Python `kubernetes` client among resources; the split here (Go for the enforcement point, Python only as oracle/offline tooling) should be confirmed with the thesis supervisor, as it touches the methods/resources text.
