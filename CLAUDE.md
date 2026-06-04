# CLAUDE.md

Guidance for working in this repository.

## What this is

`crossplane-mcp` is a **read-only diagnostic MCP server for Crossplane**. It gives
an AI assistant Crossplane-aware tools to debug stuck resources: it walks the
Composite Resource (XR) → Managed Resource (MR) tree, ranks the deepest failing
resource first, and returns full condition messages + events. See
[README.md](./README.md) and [DESIGN.md](./DESIGN.md) for the full picture.

## Layout

- `cmd/crossplane-mcp/` — stdio entry point, flags, server wiring.
- `internal/k8s/` — read-only Kubernetes client: kubeconfig/in-cluster auth,
  kind→GVR resolution, get, events, contexts.
- `internal/xp/` — Crossplane diagnostic logic: condition classification, tree
  walk, root-cause ranking. **Pure and unit-tested here** (`*_test.go`).
- `internal/tools/` — MCP tool registration + handlers (the read-only tools:
  `diagnose`, `list_unhealthy`, `get_resource_tree`, `get_resource`, `list_contexts`).

## Build / test / checks

```sh
make build      # bin/crossplane-mcp
make test       # go test -race + coverage (no cluster needed)
make lint       # golangci-lint
make vulncheck  # govulncheck
make check      # fmt-check + vet + lint + test + vulncheck (mirrors CI)
```

- **golangci-lint** must be run as **`go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2`**.
  A prebuilt golangci-lint binary built with an older Go *refuses* this module
  (go.mod targets go 1.26); building it from source with the local toolchain is
  required. CI does this; `make lint` assumes a v2 binary on PATH.
- Add tests for diagnostic logic in `internal/xp` — it's pure and needs no cluster.

## Hard rules (do not violate)

1. **Read-only invariant.** Only `get` / `list` / `watch` verbs, ever. Never
   `Create` / `Update` / `Delete` / `Patch` / `Apply`, and no write-capable
   clients. This is the project's core promise (safe to point at production).
   New tools must preserve it.
2. **Crossplane v2 *and* v1/LegacyCluster.** Handle namespaced XRs (v2, no
   Claims) and cluster-scoped Claims (v1). The tree-walk and namespace logic
   must cope with both. **Note the ref location differs by version:** v1 XRs put
   composed refs at top-level `spec.resourceRefs`; **v2 namespaced XRs nest
   Crossplane machinery under `spec.crossplane`, so composed refs are at
   `spec.crossplane.resourceRefs`.** The tree-walker must read both.
3. **No secret contents in output.** Report connection-secret presence/status
   only, never values.
4. **Token-light output.** Prune `managedFields` / noisy annotations; return only
   failing conditions/events by default. Never re-introduce truncation of
   condition messages (the whole point over `crossplane resource trace`).

## Conventions

- **Conventional Commits are required** — release-please parses them to compute
  versions and the changelog. Use `feat:`, `fix:`, `docs:`, `ci:`, `chore:`,
  `refactor:`, etc. Breaking changes: `feat!:` or a `BREAKING CHANGE:` footer.
- **Pre-1.0 (`0.x`) versioning:** `feat:` and breaking changes bump the **minor**,
  `fix:` bumps the **patch** (configured in `release-please-config.json`).
- **`main` is protected** by a ruleset: required status checks (`build & test`,
  `golangci-lint`, `govulncheck`), **signed commits**, linear history, PR-only.
  Locally-authored (unsigned) commits need `gh pr merge --admin`; release-please
  and Dependabot commits are GitHub-signed and merge normally.
- Keep PRs focused; run `make check` before pushing.

## Releases — do NOT hand-edit these

Releases are automated (release-please + GoReleaser). **release-please owns these
files** — never edit them by hand; they're regenerated from commits:

- `CHANGELOG.md`
- `.release-please-manifest.json`
- `Casks/*` in the `briferz/homebrew-tap` repo

Flow: merge `feat:`/`fix:` PRs → release-please opens a `chore(main): release
X.Y.Z` PR → merging it tags + publishes (binaries, Homebrew cask, ghcr image).
See README "Releasing".

## Current state / next work

- v0.1.0 is released; install via `brew install --cask briferz/tap/crossplane-mcp`
  or `ghcr.io/briferz/crossplane-mcp`.
- The `diagnose` root-cause ranking sorts **blocking before pending, then
  deepest-first**, and attributes the cause to a **recurring high-count
  composition event over a transient transport-flake condition** (issue #24 P1).
  Validated against a real cluster via the `/e2e-fixture` skill and live EKS use.
- `diagnose` also **decodes provider-terraform/OpenTofu base64+gzip error blobs**
  (the `echo … | base64 -d | gunzip` hint) into `Suspect.DecodedErrors`, trimmed
  to the actionable `Error:`/`Summary:` lines (issue #24 #3/#5). Pure logic in
  `internal/xp/tofu.go`; **additive** — the verbatim condition stays in `reasons`.
  Decoded text is surfaced as-is (not scrubbed); `sensitive`-marking is the
  TF/OpenTofu config's responsibility, not ours.
- Phase 2 (planned): provider/function/composition health + XRD/MR schema tools.
