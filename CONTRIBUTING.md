# Contributing to KRSM

Thanks for your interest. KRSM is an early, design-led project — the [design](docs/DESIGN.md) and [roadmap](docs/ROADMAP.md) are the source of truth for *what* we're building and *why*.

## Ground rules

- **Design before code.** Non-trivial changes start from the design. If a change implies a design decision, add or update an [ADR](docs/adr/) in the same PR.
- **Test-first.** New behaviour comes with tests. The closure engine is validated against the golden-file scenarios in `testdata/` — add a scenario when you add a behaviour.
- **Keep the verdict path model-free.** KRSM's allow/deny decision is a decision procedure over live state, never an LLM judgement. PRs that put a model in the verdict path will be declined (see DESIGN §1, non-goals).

## Local workflow

```bash
make build     # build the binary
make test      # go test -race
make lint      # golangci-lint (install: https://golangci-lint.run)
make check     # fmt + vet + lint + staticcheck + race tests — mirrors CI; run before you push
```

Toolchain: Go per `go.mod` (the CI source of truth), `gofmt -s`, `golangci-lint`, `staticcheck`.
`make check` mirrors the PR gate; CI additionally runs `govulncheck`, CodeQL, dependency review, and
(weekly) an OpenSSF Scorecard scan. All required checks must pass before merge to `main`.

## Pull requests

1. Open an issue first for anything non-trivial, so the design discussion happens before the code.
2. One logical change per PR; keep commits small and messages clear.
3. Update docs/ADRs alongside the code.
4. Ensure `make check` and CI are green.

## Maintainers

- Riyad Ilyasov ([@ridik-il](https://github.com/ridik-il)) — project owner
- Emil Hasanov ([@justem1l](https://github.com/justem1l)) — maintainer

Maintainers review incoming PRs and either may approve an application-code change
(see [.github/CODEOWNERS](.github/CODEOWNERS)). The CI/release/supply-chain config
is owned by the project owner.

## Reporting security issues

Do **not** open a public issue for vulnerabilities — see [SECURITY.md](SECURITY.md).

## License

By contributing, you agree your contributions are licensed under [Apache-2.0](LICENSE).
