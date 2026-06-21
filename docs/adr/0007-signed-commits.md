# 7. Signed commits

Date: 2026-06-21

## Status

Accepted

## Context

ADR-0006 hardened the *build and release* supply chain: SHA-pinned actions, SBOM,
keyless cosign signatures on release artifacts. It left one link unaddressed — the
authenticity of the commits themselves. Anyone can author a commit with an arbitrary
`user.name`/`user.email`; git does not, by itself, prove who wrote a change. For a
project that is simultaneously a **security tool** and a **doctoral engineering
artifact**, an unforgeable record of authorship is part of the same threat model the
tool exists to address: the integrity of what runs should be verifiable end to end,
not just at release time.

The maintainers have SSH keys already (used for auth); no GPG keys. SSH commit
signing (git ≥ 2.34) reuses an existing key with no GPG keyring to manage.

## Decision

**All commits are signed, and `main` requires verified signatures.**

- **Local default — SSH signing.** Maintainers configure `gpg.format = ssh`,
  `user.signingkey = <ssh-pubkey>`, and `commit.gpgsign = true` globally, so every
  commit and tag is signed by default. The signing public key is registered on the
  author's GitHub account as a **signing** key (distinct from an auth key) so GitHub
  marks the commit *Verified*. CONTRIBUTING documents the one-time setup.
- **Branch rule.** `main` enables required signatures (branch-protection
  `required_signatures`), so an unsigned or untrusted-key commit cannot land on it.
- **CI gate — `signed-commits` job in `ci.yml`.** PR-only. It reads each PR commit's
  `verification.verified` flag via the GitHub API and fails on any commit that is not
  verified, naming the offending shas and GitHub's reason. This surfaces the failure
  at PR time (with a clear message) rather than only at merge, and verifies the *same*
  property the branch rule enforces. It needs no checkout and no third-party action —
  just the runner's `gh` CLI — so it adds nothing to SHA-pin and keeps the
  Pinned-Dependencies surface unchanged (ADR-0006).

Bot commits (Dependabot, GitHub-authored merge commits) are signed by GitHub and so
pass verification — the rule does not impede automated dependency PRs.

This extends, and does not supersede, ADR-0006: `signed-commits` joins the required
status checks on `main` alongside `build-test`, `staticcheck`, `lint`, `govulncheck`,
`dependency-review`, and CodeQL `analyze`.

## Consequences

- **Authorship is verifiable end to end.** Combined with ADR-0006's signed release
  artifacts, the chain from a maintainer's commit to a downloadable binary is
  cryptographically attestable — the integrity story the thesis argues for.
- **Verified depends on key *and* email.** A commit shows *Verified* only when the
  committer email is a verified email on the GitHub account that holds the signing
  key. A mismatch yields a locally-signed but GitHub-*Unverified* commit, which the
  branch rule and CI gate will reject — maintainers must keep their commit email
  verified on their account.
- **Contributor friction.** External contributors must set up signing before a PR can
  merge; CONTRIBUTING gives the steps, and the CI job's error message links there.
- **No new dependency.** SSH signing reuses existing keys; the CI gate uses only `gh`.
  `closure` stays stdlib-only (ADR-0002/0005), still enforced by `internal/archguard`.
