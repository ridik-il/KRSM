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
make check     # fmt + vet + test — run before you push
```

Toolchain: Go 1.24+, `gofmt -s`, `golangci-lint`, `staticcheck`. CI runs all of these on every PR; they must pass.

## Pull requests

1. Open an issue first for anything non-trivial, so the design discussion happens before the code.
2. One logical change per PR; keep commits small and messages clear.
3. Update docs/ADRs alongside the code.
4. Ensure `make check` and CI are green.

## Reporting security issues

Do **not** open a public issue for vulnerabilities — see [SECURITY.md](SECURITY.md).

## License

By contributing, you agree your contributions are licensed under [Apache-2.0](LICENSE).
