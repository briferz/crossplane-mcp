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
make build        # bin/crossplane-mcp
make test         # go test -race + coverage (no cluster needed)
make lint         # golangci-lint
make vulncheck    # govulncheck
make e2e-envtest  # tier 1: vs a real kube-apiserver + etcd (test/e2e)
make e2e-native   # tier 2: any real cluster with controllers; no Crossplane
make e2e-crossplane # tier 3: a real cluster with Crossplane installed
make check        # fmt-check + vet + lint + test + e2e-vet + vulncheck (mirrors CI)
```

**Test tiers.** Unit tests (`make test`) use fakes and need no cluster.
`test/e2e` is a **nested Go module** running against a real kube-apiserver +
etcd via envtest — covering what fakes structurally cannot: discovery
cache invalidation, the `involvedObject.uid` field selector (`dynamicfake`
ignores field selectors entirely), discovery categories over the real wire, and
both v1/v2 ref shapes. It is a separate module on purpose, and that is
load-bearing: a `//go:build` tag would still pull `controller-runtime` into the
**shipped** `go.mod` and `govulncheck`'s surface (`go mod tidy` resolves imports
in tagged files), and the harness *must* write while the server must not — the
module boundary is what lets the `forbidigo` read-only rule keep its exact
semantics with **no exclusion and no `//nolint`**. Keep `ENVTEST_K8S_VERSION` in
step with the `client-go` minor.

That module now serves **three** tiers, selected by environment rather than
build tags (one `go vet`, no tag combination that compiles in CI but not
locally) — `make e2e-envtest` / `e2e-native` / `e2e-crossplane`. They are
ordered by how much can break underneath them: envtest reaches no network at run
time (setup-envtest fetches the apiserver/etcd binaries beforehand),
`CLUSTER_E2E` (native readiness: real controllers and kubelet, no Crossplane)
adds `kindest/node` and one pause image, `CROSSPLANE_E2E` adds three registries
nobody here controls. That ordering is why they are separate
CI jobs — a marketplace outage must not obscure a native-readiness regression.
Setting either variable asserts a cluster exists: the tests then fail rather
than skip, because a silent skip is how a tier stops running and nobody
notices. See `test/e2e/README.md`.

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
- **The squash subject is what release-please parses — not the PR title, and not
  the branch's commits.** An unrecognised type is dropped SILENTLY: the release
  workflow still succeeds, and the release PR is simply never updated. Writing
  `deps:` (not a Conventional Commits type) when squash-merging a Dependabot PR
  cost a whole extra PR to recover the missing changelog entry, and the only
  symptom was a release PR whose `updatedAt` never moved. **After squash-merging
  anything that should appear in the changelog, check that the release PR
  actually changed** — a green release workflow does not mean it did.
  - Dependabot's default `build(deps):` is correct, and `build` is hidden from
    the changelog by release-please's defaults (this repo sets no
    `changelog-sections`) — fine, since bumping `actions/checkout` changes
    nothing for a user. Use `fix(deps):` when a bump changes *shipped*
    behaviour (e.g. the go-sdk v1.7.0 bump, which changed the negotiated MCP
    protocol).
- **Pre-1.0 (`0.x`) versioning:** `feat:` and breaking changes bump the **minor**,
  `fix:` bumps the **patch** (configured in `release-please-config.json`).
- **`main` is protected** by a ruleset: required status checks (`build & test`,
  `golangci-lint`, `govulncheck`), **signed commits**, linear history, PR-only.
- **The signed-commits rule is evaluated on the PR's BRANCH commits, not on the
  merge result.** This distinction hides itself: GitHub signs the squash commit
  it creates, so `main`'s history reads `verified=true` for every release even
  when the branch commit was unsigned. Checking `main` therefore tells you
  nothing about whether a merge will be allowed. Check the branch:
  `gh api repos/briferz/crossplane-mcp/pulls/N/commits --jq '.[].commit.verification'`.
  - Locally-authored commits are unsigned here, so they need `--admin`.
  - **Dependabot's own commits ARE signed** (`verified=true`,
    `author=dependabot[bot]` — checked on #56), so a pure Dependabot PR should
    merge normally. Pushing your own commit onto its branch makes the branch
    unsigned and reintroduces `--admin`, which is why #62 needed it.
  - **release-please's branch commit is NOT signed**, and is attributed to a
    human account rather than a bot — checked on the 0.8.1 release PR, where a
    plain `gh pr merge` was rejected with *"the base branch policy prohibits the
    merge"* and `--admin` was required. This file previously claimed release PRs
    "merge normally"; that survived four releases because only the signed result
    on `main` had ever been looked at. `release.yml` authenticates with a PAT,
    which explains the human *attribution*; it does not by itself explain the
    missing signature, since GitHub does sign commits made through some API
    paths under PAT auth. What the unsigned result shows is only that
    release-please is not creating the commit through a path GitHub auto-signs
    — **which path, and whether to change it, is an open question worth
    settling**, since this also makes release commits indistinguishable from
    hand-authored ones.
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
- **`ListAll` projects at ingestion** (`internal/k8s/list.go`): every page used to
  accumulate whole objects — `managedFields` and
  `kubectl.kubernetes.io/last-applied-configuration` included — before any
  display cap applied, so `list_unhealthy{category:"managed"}` on a large estate
  could OOM the stdio process mid-call. `ProjectTriageFields` trims each object
  as it arrives (rebuilding the map, not deleting keys, so the bulk is actually
  released); the packages path passes `nil` because `BuildPackages` reads far
  more. **The projector hardcodes `BuildUnhealthy`'s read-set and cannot live in
  `xp` (import cycle) — `TestBuildUnhealthyProjectionParity` is the pin.** Add a
  field to `BuildUnhealthy`'s reads without adding it to the projector and every
  row silently loses it; nothing else in the suite notices. Paging deliberately
  runs to completion rather than stopping at the caller's limit: the pre-cap
  `Scanned`/`Summary` totals and the global Blocked-before-Pending ordering both
  need the whole set.
- **Per-kind native readiness** (`internal/xp/native.go`): `ClassifyObject` wraps
  `Classify` with access to the whole object and applies an **exact `(group,
  kind)` table** — `apps/Deployment`, `batch/Job`, `core/PersistentVolumeClaim`,
  `core/Pod`, `apps/StatefulSet`. Never a polarity heuristic: for `core/Node`
  (`MemoryPressure`/`DiskPressure`/`PIDPressure`) and
  `policy/PodDisruptionBudget` (`DisruptionAllowed`), **`False` is the healthy
  value**, so any "native condition False → Blocked" fold is wrong on contact.
  Kinds outside the table keep `Classify`'s verdict — strictly additive.
  This also fixed a **live false positive**: a Pod in phase `Succeeded` is
  `Ready=False`/`PodCompleted` forever, which `Classify` read as `StateBlocked`
  — tier 0 — so every finished init/migration Pod was the top-ranked suspect
  permanently. Rules keep `Pending` and `Blocked` distinct (a rollout in flight,
  a running Job, and a `WaitForFirstConsumer` PVC are `Pending`, never
  `Blocked`), and every non-Ready verdict carries a reason via
  `Node.nativeReasons` → `causeMessages` — pinned by
  `TestNativeVerdictsAlwaysExplained`. `Classify`'s signature is unchanged, and
  `BuildUnhealthy` deliberately stays on it: `k8s.ProjectTriageFields` drops
  `spec` and `status.phase`, so object-aware classification is unavailable there.
- **A suspect always explains itself** (`internal/xp/diagnose.go`
  `bareStateMessages`): real Crossplane writes `Ready=False` with **neither
  reason nor message** beside a fully-populated `Synced=True/ReconcileSuccess`.
  `conditionLine` renders the bare one as `""`, so `blockingMessages` dropped it
  and the top-ranked suspect arrived with empty `reasons`. No fixture here had
  ever written that shape — a fixture author always supplies a reason — which is
  why the whole unit suite passed while the live tier failed on its first run;
  `signals_test.go` now encodes it deliberately.
  Three guards, each pinned by a test that fails without it: `n.Error != ""`
  returns nil (an unreachable node has no conditions because it was never
  *read*, so "nothing has written status" is a claim about an object nobody
  looked at — note it **fabricates a line rather than losing one**: the fetch
  error is prepended after this runs, and chosen ahead of it in the summary, so
  `unreachable:` leads either way); a Blocked/Pending state gate (a
  terminating-but-Ready suspect's story is its lifecycle label, and `False` is
  the normal, non-failing value for `DisruptionAllowed` / `MemoryPressure`);
  and — **load-bearing** — it is applied only AFTER
  `attribute`/`reasonsWithEvent`, never inside `causeMessages`. These lines state
  the *absence* of an explanation, so they must behave like absence: `attribute`
  overrides to a recurring composition event only when the lead is empty or a
  transport flake (issue #24 P1), and a synthetic non-noise lead silently
  defeats that, demoting a real recurring event **with its full message** to a
  bare `Recurring event: R (xN)` suffix.
- **Golden fixtures** (`internal/xp/testdata/golden/`, embedded): objects
  captured verbatim from a live cluster by the Crossplane tier's *export golden
  fixtures* step. Every other fixture in `internal/xp` encodes what its author
  believed a provider emits, which is exactly why the whole unit suite passed
  while the live tier failed on its first real run. Reverting
  `bareStateMessages` now fails here in 0.5s with no cluster.
  `TestGoldenStillCoversBareConditions` fails if a re-capture no longer contains
  a bare condition — without it the coverage could evaporate while staying
  green. Both refuse to pass vacuously (empty fixture set, or no object
  classifying as a suspect, is a hard failure).
- **Protocol-level promises are pinned end to end** (`cmd/crossplane-mcp/
  main_test.go`): the read-only `Instructions` and every tool's `readOnlyHint`
  are only true if they survive the handshake — setting a field is not the
  promise, the client receiving it is. `newServer` is extracted from `main`
  purely so a test can drive the real thing. Relevant now because go-sdk v1.7.0
  negotiates protocol `2026-07-28`, which **replaces the initialize handshake
  with `server/discover`**; its seven `MCPGODEBUG` compatibility escape hatches
  are all scheduled for removal in **v1.9.0**, so that is the bump to watch.
- The native-readiness tier settled the open question behind
  `deploymentReadiness`: `ProgressDeadlineExceeded` is **transient**, not
  terminal. Against a live controller a wedged rollout goes
  `Progressing=False/ProgressDeadlineExceeded`, and once fixed returns to
  `Progressing=True/NewReplicaSetAvailable` — so the rule is correct as shipped
  and does not strand a recovered Deployment at Blocked.
- **Recurring failure mode: a check that reports success while being
  structurally unable to report failure.** Seen in an `e2e` label nobody had
  created (so its trigger could never fire), a `Stats.Nodes >= 3` assertion
  satisfied by an *unreachable* node while the fixture had never composed, and
  both failure notifiers exiting before doing anything for want of a repo.
  **Make a check fail on purpose before trusting it** — including the ones in
  CI and tooling, where the slow feedback loop hides this for weeks.
- Phase 2 (remaining, planned): composition tools (`list_compositions` /
  `describe_composition`) + XRD/MR schema tools (`explain_xrd` / `get_schema`).
- Open decisions (asked, unanswered): promote the native tier to per-PR (its
  assertions take ~22s but the job is ~2.5min wall including cluster creation;
  one external image; guards shipped `internal/xp` logic)? promote `integration`
  to a required check now it has burned in?
