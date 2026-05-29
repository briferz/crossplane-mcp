# crossplane-mcp

[![CI](https://github.com/briferz/crossplane-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/briferz/crossplane-mcp/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/briferz/crossplane-mcp.svg)](https://pkg.go.dev/github.com/briferz/crossplane-mcp)
[![Go Report Card](https://goreportcard.com/badge/github.com/briferz/crossplane-mcp)](https://goreportcard.com/report/github.com/briferz/crossplane-mcp)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](./LICENSE)

A **read-only diagnostic MCP server for Crossplane.** It gives an AI assistant
(Claude, etc.) Crossplane-*aware* tools to debug stuck resources: it walks the
Composite Resource (XR) → Managed Resource (MR) tree, pinpoints the resource
that is actually blocking, and returns full condition messages, events, and
provider errors — structured for an LLM, not pretty-printed for a terminal.

> **Status:** early MVP (Phase 1). Read-only by design — only `get`/`list`
> verbs are ever issued, so it is safe to point at a production cluster.

## Why

`crossplane resource trace` prints a tree to your terminal and **truncates
condition messages to 64 characters**. When an XR is stuck `Ready: False` while
every managed resource reports `Ready/Synced: True`, it still leaves *you* to
find the blocker. This server instead:

- **walks the tree and ranks the deepest failing resource first** — the likely
  root cause, not the propagated top-level symptom;
- returns **full, untruncated** `Ready`/`Synced`/`Healthy` messages plus
  correlated events;
- prunes noise (`managedFields`, etc.) so responses stay token-light;
- handles both **Crossplane v2** (namespaced XRs, no Claims) and v1 /
  LegacyCluster (Claims) trees.

See [DESIGN.md](./DESIGN.md) for the full rationale and roadmap.

## Tools

| Tool | Purpose |
|---|---|
| `diagnose` | Walk the tree from a resource, rank blocking resources (deepest first) with full messages + recent events. **Start here.** |
| `get_resource_tree` | The composition tree as a flat, parent-indexed node list with per-node Ready/Synced/Healthy state. |
| `get_resource` | One resource, pruned to conditions, recent events, and spec. |
| `list_contexts` | Available kubeconfig contexts. |

## Install

**Homebrew**

```sh
brew install briferz/tap/crossplane-mcp
```

**Container image** (GitHub Container Registry)

```sh
docker pull ghcr.io/briferz/crossplane-mcp:latest
```

**Pre-built binaries** — download from the [latest release](https://github.com/briferz/crossplane-mcp/releases/latest).

**From source**

```sh
go install github.com/briferz/crossplane-mcp/cmd/crossplane-mcp@latest
# or, in a clone:
make build
```

## Run

The server speaks MCP over stdio. It uses your kubeconfig (honouring
`KUBECONFIG` and `--context`), falling back to in-cluster config.

```sh
crossplane-mcp --context my-cluster
```

### Configure in an MCP client

```json
{
  "mcpServers": {
    "crossplane": {
      "command": "/path/to/crossplane-mcp",
      "args": ["--context", "my-cluster"]
    }
  }
}
```

### Example

> "The `App` claim `app-xyz` in namespace `team-a` won't become Ready — why?"

The assistant calls `diagnose` with `{kind: "App", name: "app-xyz", namespace:
"team-a"}` and gets back the deepest blocking resource (say a `Bucket` failing
with `AccessDenied: invalid credentials`) instead of the unhelpful top-level
"waiting for composite resource to become Ready".

## Flags

| Flag | Default | Description |
|---|---|---|
| `--kubeconfig` | `$KUBECONFIG` / `~/.kube/config` | Path to kubeconfig. |
| `--context` | current-context | Kubeconfig context to use. |
| `--version` | | Print version and exit. |

## Development

```sh
make test       # unit tests with race detector + coverage (no cluster required)
make lint       # golangci-lint
make vulncheck  # govulncheck
make check      # mirror all CI gates locally (fmt, vet, lint, test, vulncheck)
```

CI (build, test, `go vet`, `gofmt`, `golangci-lint`, `govulncheck`) runs on every
push and PR via [GitHub Actions](./.github/workflows/ci.yml).

## Releasing

Tagging `vX.Y.Z` triggers the [release workflow](./.github/workflows/release.yml),
which publishes cross-platform binaries + checksums, the Homebrew formula, and a
multi-arch container image to `ghcr.io`.

```sh
git tag v0.1.0
git push origin v0.1.0
```

One-time prerequisites:

- **Homebrew tap:** create a public `briferz/homebrew-tap` repository, and add a
  repo secret `HOMEBREW_TAP_TOKEN` (a PAT with write access to it) so GoReleaser
  can push the formula.
- **Container registry:** the image publishes via the built-in `GITHUB_TOKEN`;
  make the `ghcr.io` package public in the repo's package settings if you want
  unauthenticated pulls.

## License

[Apache License 2.0](./LICENSE).
