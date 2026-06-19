# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/), and the project aims to follow
[Semantic Versioning](https://semver.org/) once it reaches a usable release.

## [Unreleased]

### Added
- Project scaffolding: README, design doc, roadmap, ADRs, and CI.
- `krsm` CLI skeleton (`version`, `help`).
- `krsm check <dir>` — runs the closure check for a scenario directory
  (`cluster.yaml` / `request.yaml` / `scope.yaml`) and prints the
  `ACTION` / `SCOPE` / `CLOSURE` / `VERDICT` report. Exit codes: `0` allow/warn,
  `2` block, `1` usage/load error; `WARN` detail goes to stderr. Every corpus
  scenario is now a runnable demo (completes ROADMAP v0.2).
- `internal/scenario` package: shared loader turning scenario YAML into the
  `closure` types, used by both the CLI and the golden tests (keeps the public
  `closure` API stdlib-only).

[Unreleased]: https://github.com/ridik-il/krsm/commits/main
