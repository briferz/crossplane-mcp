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

See [DESIGN.md](./DESIGN.md) for the architecture and rationale.

## Goals & Non-Goals

A quick orientation — full detail and phased plan in [ROADMAP.md](./ROADMAP.md).

**Goals:** read-only diagnosis of stuck Crossplane resources (root-cause ranking
over Claim/XR→MR trees), Crossplane-aware for both v2 and v1/LegacyCluster,
LLM-optimized output, safe to point at production, and discovery/schema tools.

**Non-Goals:** it **never mutates the cluster** (no create/update/delete/apply or
remediation — diagnose-and-advise only); it is **not** a composition-authoring
tool, **not** a general-purpose Kubernetes tool, and has **no GUI**.

## Tools

| Tool | Purpose |
|---|---|
| `diagnose` | Walk the tree from a resource, rank blocking resources (deepest first) with full messages + recent events. **Start here** when you know the resource. |
| `list_unhealthy` | Triage the whole cluster: list composite resources (XRs) and claims that are not Ready/Synced — tiny rows ready to feed straight into `diagnose`. **Start here** when you don't yet know *what* is broken. |
| `get_resource_tree` | The composition tree as a flat, parent-indexed node list with per-node Ready/Synced/Healthy state. |
| `get_resource` | One resource, pruned to conditions, recent events, and spec — plus `paused` and, while terminating, `deletionTimestamp` + `finalizers`. |
| `list_providers` | Every Provider package with installed/healthy state; failing ones add full condition messages, events (e.g. the `UnpackPackage` registry error), per-revision health, and upgrade-skew notes. **Escalate here** when a managed resource's error is cryptic. |
| `list_functions` | Composition Function packages, same shape — a crashlooping function pod is invisible from the XR. |
| `list_configurations` | Configuration packages, same shape — the trail when Compositions/XRDs an XR needs are missing. |
| `list_contexts` | Available kubeconfig contexts. |

Every tool declares `readOnlyHint` at the MCP protocol level (so clients can
treat calls as safe), and the server publishes the recommended
`list_unhealthy` → `diagnose` → `get_resource` workflow — escalating to the
package-health tools when a provider/function is the suspect — as MCP
instructions.
Kind inputs are forgiving: `Bucket`, `bucket`, and `buckets` all resolve to the
same kind (exact kind matches always win, so nothing previously valid changes).

## Install

**Homebrew** (distributed as a cask)

```sh
brew install --cask briferz/tap/crossplane-mcp
```

> The macOS binaries are unsigned; the cask strips the `com.apple.quarantine`
> attribute on install so it runs without a Gatekeeper prompt.

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

When the latest condition is a transient transport error (`unexpected EOF`,
`connection reset`, …) but a composition error keeps recurring, `diagnose`
surfaces that persistent root cause rather than the flake.

For `provider-terraform` / OpenTofu resources, `diagnose` also decodes the
base64+gzip error blob (the `echo "…" | base64 -d | gunzip` hint TF prints) and
surfaces the actionable `Error: … on main.tf line NN` in a `decodedErrors`
field — boilerplate trimmed and token-light — so the real cause is in front of
the agent without shelling out.

Each suspect also carries a `lifecycle` label that separates a **wedged teardown**
from a resource **failing to come up**: a resource being deleted shows
`Terminating (stuck 140d)` (with its `deletionTimestamp` and how long it has
lingered), while one still provisioning shows `Creating (blocked, 5d)` — so an
agent routes to "unblock the finalizer" vs "fix the create" immediately. A
terminating suspect also lists its `finalizers`, naming what still holds the
deletion (`get_resource` likewise shows `deletionTimestamp` + `finalizers`
while a resource is terminating).

A **paused** resource (`crossplane.io/paused: "true"`) is flagged explicitly:
the annotation suspends reconciliation entirely — conditions go stale and a
deletion can never finish — yet nothing in `status` says so. Suspects carry
`paused: true`, a lead reason, and a `Paused (blocked, 5d)` /
`Terminating (paused, 140d)` lifecycle label; tree nodes, `list_unhealthy`
triage rows, and `get_resource` carry `paused` too (packages honour the same
annotation and get the same treatment in the package-health tools).

When the stuck resource's error points at the machinery itself — every MR of
one provider failing together, a cryptic gRPC/function error, a Composition
that doesn't exist — **`list_providers` / `list_functions` /
`list_configurations`** check the package layer: a healthy package costs a
tiny row, while a failing one shows its full `Installed`/`Healthy` condition
messages, the failing revision (whose name is by default also its runtime
Deployment's name — the pivot to pod logs), recent events such as the
`UnpackPackage`
registry error, and **upgrade skew**: an edited `spec.package` that never
unpacked, a `Manual`-policy revision waiting for approval with nothing active,
an old revision still serving while the new one is wedged (e.g. `incompatible
Crossplane version`), or package health lagging a failing new revision.

### Least-privilege RBAC

The server only ever issues `get`/`list`/`watch`, so it can run under a role
that *cannot* mutate anything. [`deploy/rbac.yaml`](./deploy/rbac.yaml) ships
two ready-made options:

- **Recommended** (standard Crossplane install): bind the aggregated
  `crossplane-view` ClusterRole that Crossplane's RBAC manager maintains — it
  automatically covers every XRD-defined and provider-defined resource type as
  they are installed, and on a default install already includes the events
  read that `diagnose`/`get_resource` use.
- **Fully explicit** (RBAC manager disabled): the standalone
  `crossplane-mcp-viewer` ClusterRole plus the manifest's small events-viewer
  role, with one rule per XR/MR API group your platform serves.

Either way, if your **v2 XRs compose native Kubernetes resources** directly
(Deployments, ConfigMaps, …), add explicit read rules for those types —
neither `crossplane-view` nor the Crossplane groups cover them, and without
read access the tree reports such a child as unreachable. The manifest shows
how (naming exact resources, never a core-group wildcard, so Secrets stay
unreadable).

For a namespace-scoped setup, bind either role with a namespaced `RoleBinding`
and call `list_unhealthy` with an explicit `namespace`. Note the package-health
tools are then out of reach: package types are cluster-scoped (and their events
live in the `default` namespace).

## Flags

| Flag | Default | Description |
|---|---|---|
| `--kubeconfig` | `$KUBECONFIG` / `~/.kube/config` | Path to kubeconfig. |
| `--context` | current-context | Kubeconfig context to use. |
| `--log-file` | | Append a JSONL record of each tool call to this path (or `-` for stderr). Also via `CROSSPLANE_MCP_LOG_FILE`. |
| `--version` | | Print version and exit. |

## Capturing tool calls

To inspect what the server saw and returned — useful for debugging, sharing a
case, or tuning — set a log file (handy when you can't pass flags through your
MCP client):

```sh
export CROSSPLANE_MCP_LOG_FILE=~/crossplane-mcp.jsonl
crossplane-mcp --context my-cluster
```

The path expands a leading `~` and `$VARS` itself, so the same value works
whether set via a shell or an MCP client's JSON config (which has no shell to
expand them); an absolute path always works.

Each tool call appends one JSON line: `{time, tool, durationMs, input, output, error}`.
The file is created `0600` (with any missing parent directories created `0700`),
so a fresh path works without a manual `mkdir -p`. Logging goes only to the
file/stderr — never stdout,
which is the MCP protocol channel. (`-` writes to stderr for ad-hoc debugging and
may interleave with other process output; use a file for clean JSONL.)

By default, two masks run before each record is written:

- **key-based** — scalar values under sensitive keys (`password`, `token`,
  `secret`, `credential`, `apikey`, `accesskey`, `privatekey`, `connection`,
  `dsn`, …) become `[redacted]`, so inline credentials in a resource `spec`
  aren't written verbatim (reference structures like a `secretRef`'s name are kept);
- **content-based** — every logged string is scrubbed for a few high-precision
  secret shapes (PEM private keys, AWS access-key IDs, JWTs, `Authorization:
  Bearer` tokens), which catches credential material the key-based mask misses —
  including in provider *error* text and the decoded OpenTofu blob (`decodedErrors`).

Disable both with `--log-redact=false` or `CROSSPLANE_MCP_LOG_REDACT=false`.

> **Sensitivity:** both masks are **best-effort, not a guarantee.** The content
> scrub is deliberately high-precision — it won't catch an arbitrary or
> unusually-shaped secret, and it intentionally does **not** mask identifiers like
> account IDs or ARNs (often the actionable detail). Redaction applies only to the
> log, never to the live tool response; values that must stay hidden should be
> marked `sensitive` in the Terraform/OpenTofu config. The server never reads
> Kubernetes Secret objects. Treat the log as potentially sensitive and **review
> it before sharing off a machine that touches production.**

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

Releases are automated with [release-please](https://github.com/googleapis/release-please)
driven by [Conventional Commits](https://www.conventionalcommits.org/) — no manual
tagging.

1. Merge `feat:` / `fix:` PRs to `main` as usual.
2. release-please opens (and keeps updating) a **release PR** titled
   `chore(main): release X.Y.Z` with the next version and an updated `CHANGELOG.md`.
3. **Merge that release PR** to cut the release: release-please creates the
   `vX.Y.Z` tag and GitHub release, then [GoReleaser](./.github/workflows/release.yml)
   attaches cross-platform binaries + checksums and publishes the Homebrew cask,
   and a multi-arch container image is pushed to `ghcr.io`.

Versioning is pre-1.0 (`0.x`): `feat:` and breaking changes bump the **minor**,
`fix:` bumps the **patch**. This is configured in
[`release-please-config.json`](./release-please-config.json); the current version
is tracked in [`.release-please-manifest.json`](./.release-please-manifest.json).

One-time prerequisites:

- **Homebrew tap:** create a public `briferz/homebrew-tap` repository, and add a
  repo secret `HOMEBREW_TAP_TOKEN` (a PAT with write access to it) so GoReleaser
  can push the cask.
- **Container registry:** the image publishes via the built-in `GITHUB_TOKEN`;
  make the `ghcr.io` package public in the repo's package settings if you want
  unauthenticated pulls.
- **Allow release-please:** in repo Settings → Actions → General, enable
  "Allow GitHub Actions to create and approve pull requests" so it can open the
  release PR.

## Contributing

Contributions are welcome! See [CONTRIBUTING.md](./CONTRIBUTING.md) for setup and
guidelines, and the [Code of Conduct](./CODE_OF_CONDUCT.md). To report a security
issue, follow [SECURITY.md](./SECURITY.md). File bugs and ideas via the
[issue templates](https://github.com/briferz/crossplane-mcp/issues/new/choose).

## License

[Apache License 2.0](./LICENSE).
