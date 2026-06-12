# Roadmap

> **Vision:** the best way for an AI assistant to *understand and debug* a
> Crossplane control plane — turning "why is my resource stuck?" into a few
> safe, read-only tool calls that pinpoint the actual cause.

This document describes what `crossplane-mcp` is for, what it deliberately
won't do, and where it's headed. It's a direction, not a commitment — see
[How to influence it](#how-to-influence-the-roadmap).

## Goals

- **Read-only diagnosis of stuck resources.** Walk the Claim / Composite
  Resource (XR) → Managed Resource (MR) tree, pinpoint the root-cause resource,
  and surface conditions (`Ready`/`Synced`/`Healthy`), events, and provider
  errors — ranked, not dumped.
- **Crossplane-aware, not generic.** Understand both **Crossplane v2**
  (namespaced XRs, `spec.crossplane.resourceRefs`, no Claims) and
  **v1 / LegacyCluster** (Claims), composition functions, and packages — the
  things a generic Kubernetes tool can't reason about.
- **LLM-optimized output.** Structured, token-light, and untruncated where it
  matters (full condition messages), so a model gets signal, not noise.
- **Safe to point at production.** Only `get`/`list`/`watch`; never returns
  secret *contents*; works under a least-privilege role — namespace-scoped
  where possible (ready-made manifest: [deploy/rbac.yaml](./deploy/rbac.yaml);
  the cluster-scoped package-health tools need the cluster-wide binding).
- **Discovery & schema.** Help reason about what providers, functions, and
  compositions exist (and their health), and what fields resources support.
- **Runs anywhere.** A stdio MCP server with multiple install methods (Homebrew,
  container, binaries, source) that works with any MCP client.

## Non-Goals

These keep the project focused. They are deliberate, not "not yet."

- **Mutating cluster state — ever.** `crossplane-mcp` never creates, updates,
  deletes, applies, or remediates resources. It diagnoses and *advises*; a human
  (or a different tool) acts. This is permanent: it's what makes "safe to point
  at production" literally true.
- **Composition authoring.** Scaffolding, generating, validating, or
  `crossplane render`-ing XRDs, Compositions, or Functions is out of scope.
  (A separate, clearly-experimental local helper could exist someday — but it
  isn't this server's job.)
- **A general-purpose Kubernetes tool.** It's Crossplane-focused. For generic
  cluster work, use `kubectl` or a general Kubernetes MCP server.
- **A UI.** No TUI, web dashboard, or graphical front end — the MCP client is
  the interface.
- **Managing Crossplane's lifecycle.** Installing, upgrading, or configuring
  Crossplane itself is out of scope (use Helm).
- **Cloud-provider-specific logic** beyond what the provider CRDs already expose.

## Roadmap

### Phase 1 — Diagnostics MVP ✅ (shipped, v0.1.x)
- `diagnose` — walk the tree, rank the deepest blocking resource as the likely
  root cause, with full condition messages + recent events. Real-world feedback
  (#24) added event-weighted ranking (a recurring composition event beats a
  transient transport-flake condition), `provider-terraform`/OpenTofu
  error-blob decoding (the `base64 -d | gunzip` hint → `decodedErrors`), and a
  `lifecycle` label separating a wedged teardown (`Terminating (stuck Nd)`) from
  a blocked create (`Creating (blocked, Nd)`).
- `get_resource_tree`, `get_resource`, `list_contexts`.
- Handles Crossplane v2 (namespaced XRs) and v1/LegacyCluster.
- Read-only, token-light output; validated against a live v2 cluster.
- Usability batch (#36): protocol-level read-only declaration (MCP
  `readOnlyHint` annotations + server workflow instructions), lenient kind
  inputs (`Bucket`/`bucket`/`buckets` all resolve), `paused`
  (`crossplane.io/paused`) and terminating-`finalizers` surfacing across
  diagnose/tree/triage/get_resource, and a least-privilege RBAC manifest
  ([deploy/rbac.yaml](./deploy/rbac.yaml)).

### Phase 2 — Discovery & schema (in progress)
- ✅ `list_unhealthy` — cluster-wide triage: list not-Ready/not-Synced XRs and
  claims (via Crossplane discovery categories) so an agent can find *what* to
  `diagnose` without leaving the server. Shipped from real-world feedback (#24).
- ✅ `list_providers` / `list_functions` / `list_configurations` — installed
  packages, revisions, health, and upgrade/version skew (failed unpack,
  awaiting manual approval, wedged rollout, GC lag), with full condition
  messages and package events on failing rows. Closes the "MR stuck with a
  cryptic Synced error and nowhere to go" gap.
- `list_compositions` / `describe_composition` — including the function pipeline
  steps a Composition runs.
- `explain_xrd` / `get_schema` — XRD and MR/XR field schemas so a model can
  reason about whether a spec is valid.

### Phase 3 — Deeper diagnosis
- Live, `--watch`-style diagnosis (observe until a resource becomes Ready or is
  deleted).
- Composition **function-pipeline** failure insight (reconstructed from XR
  status + `FunctionRevision` conditions; optionally the v2.x alpha pipeline
  inspector).
- Richer root-cause ranking heuristics (beyond depth + blocked-before-pending —
  e.g. condition reason codes, event recency).
- Connection-secret *status* surfacing (presence/readiness, never contents).

### Later / under consideration
- `crossplane render` as a **separate, clearly-experimental** local helper
  (no cluster, no mutation) — only if it preserves the read-only/local identity.
- HTTP/SSE transport for in-cluster deployment.
- Multi-cluster ergonomics (per-call context selection).

## How to influence the roadmap

Priorities are driven by real diagnostic pain. If something is missing or
mis-prioritized:

- Open a [feature request](https://github.com/briferz/crossplane-mcp/issues/new/choose)
  describing the *diagnostic problem* (not just a proposed tool).
- Start a discussion for larger directional ideas.

Proposals that fit the **Goals** and respect the **Non-Goals** above are the
easiest to land — see [CONTRIBUTING.md](./CONTRIBUTING.md).
