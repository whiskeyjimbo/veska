# Contributing to Veska

Thanks for your interest in Veska. Contributions of all kinds are welcome -
bug reports, fixes, docs, and features.

## Licensing of contributions

Veska is licensed under **AGPL-3.0-only** (see [`LICENSE`](LICENSE)).

Contributions are accepted into the public project under those same terms
(**inbound = outbound**). You retain copyright in your contributions; the record
of authorship is the git history.

In addition, by submitting a contribution you grant the project maintainer a
perpetual, worldwide, irrevocable, royalty-free license to use, reproduce,
modify, and **relicense or sublicense** your contribution, including under
commercial or other non-AGPL terms. This lets the project be dual-licensed or
relicensed in the future without tracking down every contributor. There is no
separate signed CLA - opening a pull request constitutes agreement. Please only
submit work you have the right to license this way.

If you add a third-party dependency, run `make notices` and commit the updated
`THIRD_PARTY_NOTICES`, and make sure the dependency's license is compatible with
AGPL-3.0 (permissive licenses - MIT/BSD/Apache-2.0 - are fine; copyleft or
source-available licenses generally are not).

Every source file carries the SPDX license identifier (and only that - no
per-file copyright line; copyright is recorded in [`LICENSE`](LICENSE) and the
git history). New files start with:

```go
// SPDX-License-Identifier: AGPL-3.0-only
```

`tools/licensing/add-spdx.sh` stamps any file missing the header.

## Development setup

You need Go (see the version in [`go.mod`](go.mod)) and a C toolchain - Veska
uses cgo for tree-sitter and the SQLite FTS5 driver.

```bash
make build        # thin/fat build of veska + symlinks
make all          # the full gate: build + test + vet + lint + layercheck + ratchets
```

`make all` is exactly what CI runs. Get it green before opening a PR.

Useful targets:

```bash
make test                 # go test ./...
make vet                  # go vet ./...
make lint                 # golangci-lint
make layercheck           # enforce hexagonal layering (domain/ports must not import infra)
go test ./internal/core/domain/...   # a single package
```

## Architecture & conventions

Read [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) before making structural
changes - Veska follows DDD-lite inside a hexagonal (ports-and-adapters) shell,
and `make layercheck` enforces the dependency direction. A few house rules:

- **Commit messages**: one-line conventional commits (`type(area): description`).
- Run `go fmt ./...` before committing; new code reuses the existing patterns
  rather than introducing new architectural ones.
- Keep the ubiquitous language (`Node`, `Edge`, `Graph`, `Task`) consistent
  across domain, ports, adapters, and CLI output.

## Pull requests

1. Fork and branch from `main`.
2. Make your change with tests.
3. Ensure `make all` passes.
4. Open a PR describing the change and motivation. Link any related issue.

## Reporting bugs / requesting features

Use [GitHub Issues](../../issues). For security-sensitive reports, follow
[`SECURITY.md`](SECURITY.md) instead of opening a public issue.
