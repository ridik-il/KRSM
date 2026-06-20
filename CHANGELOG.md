# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/), and the project aims to follow
[Semantic Versioning](https://semver.org/) once it reaches a usable release.

## [Unreleased]

## [0.2.0] - 2026-06-20

### Added
- `krsm check [--plain] <dir>` — runs the closure check for a scenario directory
  (`cluster.yaml` / `request.yaml` / `scope.yaml`) and prints the
  `ACTION` / `SCOPE` / `CLOSURE` / `VERDICT` report. Exit codes: `0` allow/warn,
  `2` block, `1` usage/load error. A `WARN`'s cross-boundary detail goes to stderr
  (with a self-contained verdict stub on stdout); `--plain` emits ASCII without
  emoji for CI logs / non-UTF8 terminals. Every corpus scenario is now a runnable
  demo (completes ROADMAP v0.2).
- `internal/scenario` package: shared loader turning scenario YAML into the
  `closure` types, used by both the CLI and the golden tests (keeps the public
  `closure` API stdlib-only).
- Kubernetes model fidelity (see `docs/design/k8s-model-fidelity.md`, ADR-0005):
  cluster-scoped resources resolve to namespace `""` so a Namespace delete no
  longer mis-counts a `PersistentVolume` as its contents; cross-reference coverage
  extended to `initContainers`, projected volume sources, pod `imagePullSecrets`,
  and `ephemeralContainers`; faithful label selectors — a pure-stdlib
  `LabelSelector` with `matchLabels` + `matchExpressions` (`In`/`NotIn`/`Exists`/
  `DoesNotExist`, absence-sensitive) so set-based selectors bind precisely instead
  of collapsing to the empty selector. Adds golden scenarios 14–19.
- The scenario loader rejects an unrecognised selector operator instead of
  silently binding nothing (fail-closed at the parse boundary), and a safety-
  invariant test asserts no namespaced kind is mislabelled cluster-scoped.
- Generative property tests (`closure/property_test.go`, stdlib `math/rand`) that
  exercise the termination and `|C| ≤ |R|` bounds on thousands of randomized
  adversarial graphs (cyclic/self/deep ownership, selector and cross-ref edges).
- `internal/archguard` test enforcing that `closure/` imports nothing under
  `k8s.io/` (the embeddable-SDK stdlib-only constraint).

### Changed
- **Breaking (pre-1.0 SDK):** `closure.Object.Selector` and `State.PodsMatching`
  retype from `map[string]string` to `closure.LabelSelector` (ADR-0005). A
  `matchLabels`-only selector reduces to the previous subset test, so the golden
  scenarios are unchanged; external embedders (none yet) would need to adapt.

## [0.1.0] - 2026-06-19

### Added
- Project scaffolding: README, design doc (`docs/DESIGN.md`), roadmap, ADRs
  0001–0004, Apache-2.0 license, and CI (`gofmt`, `golangci-lint`, `staticcheck`,
  `go test -race`).
- `krsm` CLI skeleton (`version`, `help`).
- `closure` — the embeddable, stdlib-only closure engine: model types
  (`GVK`/`Ref`/`Object`/`Action`/`Decision`), the four relations (ownerReference
  cascade, namespace containment, label-selector binding, cross-resource
  reference), effect-class gating per verb, a breadth-first closure walk with a
  visited-set guard (`|C| ≤ |R|`, terminating on cyclic ownerReferences), and the
  `Safe` verdict (scope-subset test, fail-closed on an unresolvable target).
- The failure-mode scenario corpus as golden tests under
  `closure/testdata/scenarios/`.

[Unreleased]: https://github.com/ridik-il/krsm/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/ridik-il/krsm/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/ridik-il/krsm/releases/tag/v0.1.0
