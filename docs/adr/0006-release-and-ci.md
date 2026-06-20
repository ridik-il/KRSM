# 6. Release automation and CI / supply-chain hardening

Date: 2026-06-20

## Status

Accepted

## Context

v0.1.0 and v0.2.0 were tagged and released by hand. There is no release automation,
and CI (`.github/workflows/ci.yml`) runs only `gofmt`, `go vet`, `go build`,
`go test -race`, and golangci-lint. Three gaps matter for a project that is at once a
**security tool**, a doctoral engineering artifact, and a future embeddable SDK:

- **CI does not enforce what the docs claim.** DESIGN §8 and CONTRIBUTING state that
  `staticcheck` runs "on every change" — but it is not in the workflow at all. The Go
  version is pinned three inconsistent ways (`go.mod` says 1.24, CI hardcodes "1.24",
  the local toolchain is 1.25). Actions float on mutable major tags (`@v4`), the exact
  supply-chain surface KRSM's own thesis is about controlling.
- **No supply-chain assurance.** No vulnerability scanning, no SAST, no SBOM, no signed
  artifacts. A security tool with none of these is not credible to the audiences (agent
  builders, a thesis committee) it must convince.
- **No repeatable release.** "Release v0.3" has no defined meaning beyond a manual tag.

The public `closure` SDK must remain standard-library-only (ADR-0002, ADR-0005,
DESIGN §7); none of this may add a module dependency to it.

## Decision

**Release-time (tag-triggered `release.yml` + `.goreleaser.yaml`).** Adopt **goreleaser**
to build the `krsm` CLI for linux/darwin/windows × amd64/arm64 with a SHA-256
`checksums.txt`; extract that version's section from `CHANGELOG.md` as the GitHub Release
body; generate a **syft** SPDX SBOM per archive; and **sign** the checksums file with
**keyless cosign** (GitHub OIDC / Fulcio, no long-lived keys). The git **tag is the single
source of truth**: it drives the SDK module version (`go get …@vX.Y.Z`), the binary version
(`-ldflags -X main.version=<tag>`, overriding `cmd/krsm`'s `0.0.0-dev`), and the Release.

**SDK publishing** is by tag only (Go modules); a `/v2`+ module-path suffix is required only
after the first *post-1.0* breaking change. The v0.2 `Object.Selector` retype was breaking
but legal pre-1.0 and is recorded in CHANGELOG.

**The webhook container image (GHCR + `ko`) is deferred to v0.5** — sketched as a commented
stub in `.goreleaser.yaml`, not built now.

**PR-time (`ci.yml`, `codeql.yml`, `scorecard.yml`).** Upgrade `ci.yml`: add **staticcheck**
(closing the stated-but-false gap), make `go-version-file: go.mod` the sole version source,
**pin every action to a commit SHA** (with a version comment; Dependabot keeps them fresh),
add least-privilege `permissions:`, `concurrency` cancellation, coverage, **govulncheck**, and
PR **dependency-review**. Add **CodeQL** (Go SAST) and **OpenSSF Scorecard** (+ README badge)
on push/schedule. Add **Dependabot** (gomod + github-actions), **CODEOWNERS**, and a PR
template. golangci-lint is pinned to **v1.64.8** to match the local toolchain; migrating to
golangci-lint v2 (a config-schema change) is deferred.

**Branch protection on `main`** requires these checks to merge: `build-test`, `lint`,
`govulncheck`, `dependency-review`, and CodeQL `analyze`. Scorecard runs on push/schedule
(never on PRs) so it is *not* a required check.

## Consequences

- **The project practices the control it preaches.** SHA-pinned actions are content-addressed
  and cannot be repointed at malicious code — the pipeline analogue of pre-execution blast-radius
  control. The SBOM + keyless signature give a verifiable supply-chain story.
- **CI equals the documented local gate.** `make check` is updated to include lint + staticcheck,
  so local == CI; CONTRIBUTING is corrected to match.
- **Releases are reproducible from a tag.** Cutting a release = finalize the CHANGELOG section,
  then `git tag vX.Y.Z && git push --tags`; the rest is automated. (The maintainer runs the tag.)
- **`closure` stays stdlib-only.** goreleaser, syft, cosign, and the scanners are CI/dev tooling;
  none enters `go.mod` — still enforced by `internal/archguard`.
- **Deferred (tracked):** golangci-lint v2 migration; the webhook image (v0.5); SLSA provenance
  attestation and a Homebrew tap (v1.0); Codecov integration.
- **Cost:** SHA pins are unreadable without the version comment and need Dependabot to stay
  current; four workflow files instead of one; goreleaser/cosign/syft become CI prerequisites.
