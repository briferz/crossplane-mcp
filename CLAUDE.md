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
  `diagnose`, `list_unhealthy`, `get_resource_tree`, `get_resource`,
  `list_providers`, `list_functions`, `list_configurations`, `list_contexts`).

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
   New tools must preserve it. **Enforced mechanically, not by convention:** a
   `forbidigo` rule in `.golangci.yml` fails the lint gate (a required check on
   `main`) on any dynamic-client write method, and
   `TestHandlersIssueOnlyReadVerbs` drives every handler against the dynamic
   fake asserting only `get`/`list` actions — a backstop for anything routed
   through `Client.Dyn`, which is where every cluster call goes today.
   `Client.Dyn` stays a write-capable `dynamic.Interface` because client-go
   offers no read-only variant and cross-package tests inject a fake.
   **Re-verify the rule after a client-go bump:** it is a denylist of the eight
   write methods, so a *renamed* interface stops matching silently and a *newly
   added* write method is not covered at all (RE2 has no lookahead, so an
   allowlist inversion is not expressible).
2. **Crossplane v2 *and* v1/LegacyCluster.** Handle namespaced XRs (v2, no
   Claims) and cluster-scoped Claims (v1). The tree-walk and namespace logic
   must cope with both. **Note the ref location differs by version:** v1 XRs put
   composed refs at top-level `spec.resourceRefs`; **v2 namespaced XRs nest
   Crossplane machinery under `spec.crossplane`, so composed refs are at
   `spec.crossplane.resourceRefs`.** The tree-walker must read both.
3. **No secret contents in output.** Report connection-secret presence/status
   only, never values. Precise scope: a Secret referenced by an XR *is* fetched
   during a tree walk like any other node — the promise is about what leaves the
   process, not what it reads. It holds because the output structs are closed
   projections (`ResourceView` carries `spec`, and a core/v1 Secret has none);
   `TestSecretContentsNeverReturned` pins that, so a future raw-object field
   fails there rather than silently disclosing.
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
  Decoded text is surfaced as-is in the response (identifiers like ARNs kept); the
  `--log-file` recorder applies a best-effort high-precision secret scrub
  (PEM/AWS-key/JWT/Bearer, in `internal/tools/record.go`). `sensitive`-marking
  remains the TF/OpenTofu config's responsibility, not ours.
- `diagnose` suspects carry a **`deletionTimestamp` + derived `lifecycle` label**
  distinguishing a wedged teardown (`Terminating (stuck 140d)`) from a blocked
  create (`Creating (blocked, 5d)`) — issue #24 #4. Pure logic in
  `internal/xp/lifecycle.go`; the age clock is the injectable `nowFn` so the
  logic stays unit-testable. Additive (FlatNode also surfaces raw
  `deletionTimestamp`); ranking unchanged.
- Tier-1 usability batch (one PR): every tool declares the MCP `readOnlyHint`
  annotation and the server publishes workflow `Instructions` (protocol-level
  read-only promise); kind resolution is **lenient** (case-insensitive +
  plural/singular resource names, exact Kind matches always win —
  `internal/k8s/client.go scanForKind`; a gv-constrained fallback reads
  ServerGroupsAndResources so non-preferred served versions resolve too);
  suspects/tree/get_resource/`list_unhealthy` rows surface **`paused`**
  (`crossplane.io/paused` — lead reason + `Paused (blocked, Nd)` /
  `Terminating (paused, Nd)` lifecycle labels) and terminating suspects (and
  `get_resource`) list **`finalizers`**; a least-privilege read-only RBAC
  manifest ships in
  `deploy/rbac.yaml` (aggregated `crossplane-view` + events role, or a
  standalone explicit ClusterRole; native types composed by v2 XRs need
  explicit extra rules — never a core-group wildcard, Secrets must stay
  unreadable).
- Package-health tools (`list_providers` / `list_functions` /
  `list_configurations`, Phase 2): one shared handler factory
  (`internal/tools/packages.go`) + pure builder (`internal/xp/packages.go`).
  Discovery is **category-driven** (`pkg`/`pkgrev` constants in
  `internal/k8s/list.go`, group pinned to `pkg.crossplane.io`) so
  Function@v1beta1 clusters (Crossplane 1.14–1.16) work with no version
  branching. **`Classify` ignores `Installed`** — packages use `classifyAll`
  (fold over ALL conditions; never a type whitelist — the pkg condition
  vocabulary churns across versions: revision `Healthy` on 1.x vs
  `RevisionHealthy`+`RuntimeHealthy` on 2.x, `Verified` only 1.19–2.1).
  Healthy rows are tiny; failing rows add full `reasons`, derived `skew`
  sentences (from spec/status fields only, never reason strings), `Installing`
  lifecycle labels (head-parameterized `lifecycleLabelFor`), only-when-signal
  revision rows (cap 5, keep current/Active/non-Ready), and unhealthy-only
  events for BOTH package and failing revisions (cluster-scoped → events land
  in the `default` namespace, which `Client.Events` already handles). Full
  detail is capped at the first 10 failing rows (`maxDetailedPackages`,
  mirrors diagnose's `maxSuspects`) — a mass failure (registry outage) goes
  compact beyond that with a note; the budget is pinned by
  `TestBuildPackagesMassFailureBudget` (~82 KiB worst case, was 542 KiB
  uncapped).
  RBAC-unlistable revisions suppress revision rows/counts and
  revision-derived skew (`RevisionsListed` guard; the stuck-unpack sentence,
  derived from package status alone, still renders) — missing data must never
  read as "no active revision". No RBAC rule changes (both rbac.yaml options
  already cover `pkg.crossplane.io`).
- **`Classify` is apiVersion-aware** (`internal/xp/conditions.go`): a resource in
  a built-in Kubernetes API group (core, `apps`/`batch`/`autoscaling`/`policy`/
  `extensions`, or `*.k8s.io`) carrying none of `Ready`/`Synced`/`Healthy`
  classifies **`StateUnknown`** — never a diagnose suspect unless terminating, no
  lifecycle label, and excluded from the "all Ready" claim. A conditions-less
  resource in a provider/XRD group stays `StatePending`, because a fresh MR must
  not read as healthy. The group is consulted **only** when the vocabulary is
  absent, so a native object that does carry `Ready` still classifies on it.
  Without this, every native resource a v2 XR composes was a permanent suspect
  and frequently the named root cause.
- **No signal drops at an output boundary** (each was silently lost before):
  suspects carry `error` plus an `unreachable:` lead reason for nodes the walk
  could not fetch (RBAC-forbidden / NotFound / unresolvable kind), so an error
  node — often the deepest, hence frequently the named root cause — can no
  longer be a bare kind/name with empty reasons. `Stats.Capped` now means
  resources were *actually skipped*, not merely that a limit was reached (a
  final leaf at `maxDepth` with no children skips nothing), and a capped
  traversal both caveats the summary and forbids `healthy:true` — the ranking
  ran over a partial tree. `attribute` leads with the first **non-noise**
  condition, since `status.conditions` has no defined order; an all-noise
  suspect keeps its lead, so provider-http/kubernetes "connection refused"
  (where it IS the root cause) is unaffected. `list_unhealthy` rows carry
  `deletionTimestamp` with a disjoint `summary.terminating` bucket, so a
  resource whose reconciler died mid-teardown can no longer count as Ready and
  vanish from triage.
- **Discovery staleness + request bounds** (`internal/k8s/client.go`): the memory
  discovery cache never expires and the deferred mapper's own reset is gated on
  `!Fresh()` (permanently false once populated), so a CRD installed mid-session
  — a provider install, a new XRD, i.e. exactly what prompts someone to reach
  for this tool — was invisible until restart. `scanForKind` now invalidates and
  retries **once** on a genuine not-found, rate-limited to one invalidation per
  5s per Client so a walk over many unresolvable refs cannot re-discover the
  cluster per miss. An ambiguous kind or a discovery transport error does *not*
  retry — re-reading discovery cannot fix either. `rest.Config.Timeout` is now
  set explicitly (`--request-timeout`, default 30s, `0` disables); note it is a
  per-request deadline on the shared config, so a future genuine Watch would
  need its own config with `Timeout` unset.
- Phase 2 (remaining, planned): composition tools (`list_compositions` /
  `describe_composition`) + XRD/MR schema tools (`explain_xrd` / `get_schema`).
