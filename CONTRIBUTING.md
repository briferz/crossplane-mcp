# Contributing to crossplane-mcp

Thanks for your interest in improving `crossplane-mcp`! This document covers how
to get set up, the project's scope, and what we look for in a contribution.

By participating you agree to abide by our [Code of Conduct](./CODE_OF_CONDUCT.md).

## Project scope

`crossplane-mcp` is a **read-only diagnostic** MCP server for Crossplane. Two
principles guide what belongs here:

1. **Read-only by construction.** The server only issues `get`/`list`/`watch`
   verbs — it never creates, updates, or deletes cluster resources. Contributions
   that mutate cluster state are out of scope so the server stays safe to point
   at production. (Local, non-cluster operations like `crossplane render` could be
   considered separately — open an issue to discuss first.)
2. **Crossplane-aware diagnostics.** The value over a generic Kubernetes tool is
   understanding the Composite Resource (XR) → Managed Resource (MR) tree,
   condition propagation, and composition/function failures. Features should lean
   into that.

If you're unsure whether something fits, open an issue before writing code.

## Getting started

You'll need **Go** (the version in [`go.mod`](./go.mod)) and `make`.

```sh
git clone https://github.com/briferz/crossplane-mcp
cd crossplane-mcp
make build      # builds bin/crossplane-mcp
```

Run the full set of checks that CI runs, locally:

```sh
make check      # fmt-check + vet + lint + test + vulncheck
```

Individual targets:

| Command | What it does |
|---|---|
| `make test` | unit tests with the race detector + coverage (no cluster needed) |
| `make lint` | `golangci-lint run` |
| `make vulncheck` | `govulncheck ./...` |
| `make fmt` | format the tree with `gofmt` |

> `make lint` requires `golangci-lint` v2 on your `PATH`. If you don't have it,
> run it the same way CI does:
> `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest run ./...`

### Trying it against a cluster

The server speaks MCP over stdio and uses your kubeconfig:

```sh
./bin/crossplane-mcp --context my-cluster
```

See the [README](./README.md) for wiring it into an MCP client.

## Making a change

1. **Open or comment on an issue** for anything beyond a trivial fix, so we can
   agree on the approach.
2. **Branch** from `main` (e.g. `feat/...`, `fix/...`, `docs/...`).
3. **Keep PRs focused** — one logical change per PR is much easier to review.
4. **Add tests** for new behavior. The diagnosis logic in `internal/xp` is pure
   and unit-testable without a cluster; please cover it there.
5. **Run `make check`** and make sure it's green before pushing.
6. **Open a PR** and fill in the template. CI must pass.

### Commit messages

We use [Conventional Commits](https://www.conventionalcommits.org/) — the release
changelog is generated from them. Use a type prefix:

```
feat: add provider health tool
fix: scope event queries to the resource namespace
docs: clarify namespaced vs cluster-scoped kinds
ci: pin golangci-lint version
```

Signing off your commits (`git commit -s`) is appreciated but not required.

## Reporting bugs and requesting features

Use the [issue templates](https://github.com/briferz/crossplane-mcp/issues/new/choose).
For security issues, please follow [SECURITY.md](./SECURITY.md) instead of opening
a public issue.

## License

By contributing, you agree that your contributions will be licensed under the
project's [Apache-2.0 License](./LICENSE).
