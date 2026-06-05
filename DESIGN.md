# crossplane-mcp — Design

A **read-only diagnostic MCP server for Crossplane**, targeted at platform
engineers. It gives an AI assistant Crossplane-*aware* tools to debug stuck
resources: walking the Composite Resource (XR) → Managed Resource (MR) tree,
pinpointing the root-cause node, and surfacing full condition messages, events,
provider errors, and composition-function pipeline failures — structured for an
LLM, not pretty-printed for a terminal.

> **Status:** design. No code yet.

---

## 1. Why this exists

The landscape (researched 2026-05):

- **No maintained Crossplane-specific MCP server exists.** The only prior art,
  `cychiang/crossplane-mcp-server`, was a WIP Python project **archived by its
  owner on 2025-09-24**.
- **Upbound's official `marketplace-mcp-server`** only queries the Upbound
  Marketplace *catalog* (package discovery). It never touches a live cluster.
- **Generic Kubernetes MCP servers** (`kkb0318/kubernetes-mcp`,
  `patrickdappollonio/mcp-kubernetes-ro`, `manusa/kubernetes-mcp-server`) prove
  the read-only-in-Go pattern and can `list`/`describe` Crossplane CRDs
  *generically* — but **none understand the Claim/XR/MR relationship tree,
  condition propagation, or function-pipeline failures.** They treat a `Bucket`
  like any other custom resource.

That Crossplane **semantic awareness** is the entire reason to build this.

### The pain we target

The signature, hard-to-diagnose Crossplane failure mode (Crossplane issues
#2957, #1848, #5400): an **XR/Claim stuck `Ready: False`** while *every*
underlying managed resource reports `Ready: True` / `Synced: True` — with
nothing pointing at the actual blocker. Compounding factors:

- `crossplane resource trace` **truncates condition/status messages to 64
  chars** unless you pass `-o wide`, and emits a human-oriented tree, not
  structured data.
- Crossplane emits **minimal logs by default** — the official troubleshooting
  guidance is **events- and conditions-centric** (`kubectl describe`).
- Composition-function (Pipeline mode) failures are obscured by conditions/
  events alone.

So the differentiator is **not** "render the tree." It is:

1. **Walk the tree and pinpoint the deepest broken node** — return the
   root cause, not the top-level symptom.
2. **Full, untruncated** `Ready`/`Synced`/`Healthy` messages + correlated
   events + provider errors, as JSON.
3. **Function-pipeline awareness** — which step failed.
4. **Token-efficient output** — return only *failing* conditions/events; prune
   `managedFields` / `last-applied`, so the model isn't drowning in YAML.

---

## 2. Scope

**In scope (v1):** read-only diagnosis. Inspect, traverse, correlate, explain.

**Out of scope (v1):** any cluster mutation (create/apply/delete/patch),
composition *authoring*, `crossplane render`. These may come later behind an
explicit opt-in, but the v1 promise is "safe to point at production."

**Audience:** platform engineers building/operating Compositions, XRDs, and
providers.

---

## 3. Crossplane model the server must handle

Target **Crossplane v2** (GA 2025) while staying compatible with v1 /
LegacyCluster:

| Concept | v2 (default) | v1 / LegacyCluster |
|---|---|---|
| Composite Resource (XR) | **Namespaced** by default | Cluster-scoped |
| Managed Resource (MR) | Namespaced by default | Cluster-scoped |
| Claim | **Removed** (no claims for v2 XRs) | Exists (claim/XR duality) |
| XRD `scope` field | `Namespaced` (default), `Cluster`, `LegacyCluster` | n/a |

The tree-walker must:

- Handle a **Claim → XR → MR** chain (v1/LegacyCluster) **and** a namespaced
  **XR → MR** chain (v2), detecting which via the XRD `scope` and the resource's
  own references.
- Follow ownership via `spec.resourceRefs` / `spec.resourceRef` /
  `metadata.ownerReferences` and `spec.claimRef` where present.
- Be namespace-aware.

### Status fields that matter (highest signal first)

- `Ready` condition (availability) — on almost everything.
- `Synced` condition (reconciliation w/ external API) — on MRs.
- `Healthy` condition — on `ProviderRevision` / `FunctionRevision`.
- `status.conditions[].message` / `reason` — **the** root-cause text.
- Events (`kubectl get events` equivalent) — Crossplane's primary signal.
- Composition function results / pipeline step failures.
- Package: `Installed` / `Healthy` on Provider/Function/Configuration + revision
  version skew.

---

## 4. Architecture

```
                 ┌──────────────────────────────────────────┐
   MCP client    │  crossplane-mcp (Go)                      │
  (Claude, etc.) │                                           │
   ───stdio────▶ │  MCP layer: modelcontextprotocol/go-sdk   │
                 │     tools registered, schemas, transport  │
                 │                                           │
                 │  diagnosis engine                         │
                 │     tree walker · root-cause ranker ·     │
                 │     condition/event correlator · pruner   │
                 │                                           │
                 │  k8s access (client-go + crossplane-      │
                 │     runtime): dynamic/discovery clients,  │
                 │     unstructured, RESTMapper, kubeconfig  │
                 └───────────────────┬──────────────────────┘
                                     │ read-only API calls
                                     ▼
                          Kubernetes API server
                       (Crossplane CRDs + core objects)
```

**Decisions (locked):**

- **Language: Go.** Reuse `crossplane-runtime` + `client-go`; ability to lift
  the actual `crossplane resource trace` traversal logic rather than
  reimplement it.
- **Data access: direct Kubernetes API** via `client-go` (dynamic + discovery
  clients, `unstructured.Unstructured`, `RESTMapper`). **No shelling out** to
  the `crossplane`/`kubectl` binaries — full control over output shape and
  token budget, no binary dependency.
- **MCP SDK: official `github.com/modelcontextprotocol/go-sdk`.**
- **Transport: stdio** for v1 (local assistant). HTTP/SSE later if needed.

**Key packages (anticipated):**

- `k8s.io/client-go/dynamic`, `k8s.io/client-go/discovery`,
  `k8s.io/apimachinery/.../unstructured`, `restmapper`
- `k8s.io/client-go/tools/clientcmd` (kubeconfig + context selection)
- `github.com/crossplane/crossplane-runtime` (conditions, well-known types)
- `github.com/modelcontextprotocol/go-sdk`

---

## 5. Tool surface (v1)

All read-only. Inputs favor `{ kind, name, namespace?, apiVersion? }`. Outputs
are pruned JSON: status/conditions/events first, spec trimmed, noise removed.

| Tool | Purpose | Key output |
|---|---|---|
| **`diagnose`** ⭐ | Flagship. Walk the tree from a given resource, find the deepest non-Ready/non-Synced node, rank by likely root cause. | Ordered list of suspect resources w/ full condition messages, reasons, correlated events, and a one-line "likely cause" summary. |
| `list_unhealthy` | Cluster-wide triage: discover XRs + claims (via Crossplane discovery categories) and return the not-Ready/not-Synced ones. Answers "what is broken?" before `diagnose`. | Flat rows `{apiVersion, kind, name, namespace, category, state, ready, synced}` + pre-cap summary counts; RBAC-tolerant notes. |
| `get_resource_tree` | Structured Claim/XR → MR hierarchy with per-node status. The trace equivalent, as JSON. | Nested tree: each node `{kind, name, namespace, ready, synced, healthy, ageSeconds}`. |
| `get_resource` | One resource, pruned. | status/conditions, recent events, trimmed spec; `managedFields`/`last-applied` stripped. |
| `list_compositions` | Compositions installed, with mode + function pipeline steps. | `{name, compositeTypeRef, mode, pipeline:[step,functionRef]}`. |
| `describe_composition` | One composition incl. pipeline + which XRs use it. | full pipeline + referenced functions + status. |
| `list_providers` | Providers + revisions + health. | `{name, installed, healthy, version, revisions}`. |
| `list_functions` | Composition Functions + revisions + health. | same shape as providers. |
| `explain_xrd` | An XRD's schema, versions, and `scope`. | API surface a platform engineer can request. |
| `get_schema` | Field schema for an MR/XR kind (from CRD openAPIV3Schema). | so the model reasons about spec correctness. |
| `list_contexts` | Available kubeconfig contexts. | for multi-cluster selection. |

**Flagship behavior — `diagnose`:** the explicit win over generic k8s MCP
servers and over `trace`. Pseudo-logic:

1. Resolve the start resource (or accept a Claim/XR/MR).
2. BFS/DFS the ownership/ref tree, collecting each node's conditions + events.
3. Mark each node `OK` / `degraded` / `blocking`.
4. Identify the **deepest** `blocking` node(s) — the leaf that's actually
   failing, not the propagated `Ready:False` at the root.
5. Return ranked suspects with **untruncated** messages + correlated events +
   (if present) the failing function-pipeline step.
6. **Attribute the cause:** prefer the failing condition message, but when that
   condition is a transient transport flake (`unexpected EOF`, `connection
   reset`, `rpc … Unavailable`, …) and a Crossplane composition/validation
   **event** recurs with a high count, surface that recurring event as the root
   cause instead — and always surface the dominant recurring event. Avoids
   chasing a phantom network error over the persistent bug behind it (#24).
7. **Decode provider blobs:** when a condition/event carries a
   `provider-terraform`/OpenTofu `echo "…" | base64 -d | gunzip` hint, decode the
   base64+gzip blob and surface its actionable `Error:`/`Summary:` lines in a
   `decodedErrors` field (#24). Bounded (256 KiB in / 4 MiB out, single gzip
   member, keep-partial on a truncated stream), token-light (`[DEBUG]`/`[TRACE]`/
   OTEL boilerplate trimmed, 40-line budget, deduped within and across suspects),
   and **additive** — the verbatim condition message (hint included) is left
   untouched in `reasons`. Decoded text is not scrubbed; `sensitive`-marking is
   the TF/OpenTofu config's job.
8. **Label the lifecycle:** surface each suspect's `deletionTimestamp` (raw) and
   a derived `lifecycle` label that distinguishes a wedged teardown
   (`Terminating (stuck 140d)`) from a resource failing to come up
   (`Creating (blocked, 5d)`) — the age (how long stuck) is the signal (#24). The
   age clock is injected (`nowFn`) so the pure logic stays unit-testable; a
   resource being deleted is always labelled Terminating regardless of its age.

---

## 6. Token-efficiency policy

Large k8s objects wreck an LLM context. Defaults:

- Strip `metadata.managedFields`, `metadata.annotations["kubectl...last-applied"]`,
  and verbose generated annotations.
- Return **only failing/abnormal** conditions and events unless `verbose:true`.
- Never truncate condition `message`/`reason` (the 64-char trace limit is a bug
  to fix, not copy).
- Paginate / cap list results; report when results were capped (no silent
  truncation).
- `get_resource_tree` returns a compact status-only shape; full per-node detail
  is a follow-up `get_resource` call.

---

## 7. Auth & safety

- Standard **kubeconfig** (out-of-cluster) and **in-cluster** service account.
- Context selectable via `list_contexts` + a `context` tool arg.
- **Read-only by construction:** only `get`/`list`/`watch` verbs are ever
  issued. Document a minimal read-only `ClusterRole` users can bind.
- No secrets in output by default (connection-secret *contents* are never
  returned; presence/status only).

---

## 8. Roadmap

The phased plan, plus the project's **goals and non-goals**, now live in
[ROADMAP.md](./ROADMAP.md) (single source of truth, to avoid drift). In short:
Phase 1 (diagnostics MVP) shipped in v0.1.x; Phase 2 is discovery & schema
tools; Phase 3 is deeper diagnosis (live watch, function-pipeline insight,
richer ranking).

---

## 9. Open questions

- Pipeline-function diagnostics: reconstruct from XR status/events +
  `FunctionRevision` conditions (stable) vs. the alpha pipeline-inspector gRPC
  socket (richer but unstable). Lean stable for v1.
- Multi-cluster UX: single context per server process vs. per-call `context`
  arg.
- Root-cause ranking heuristics for `diagnose`: deepest-blocking-first plus
  event-recurrence weighting (a transient-flake condition yields to a high-count
  recurring composition event) landed from real-cluster feedback (#24); further
  tuning (reason codes, condition-type weighting, function-pipeline shapes) keeps
  iterating against real broken clusters.

---

## 10. References

- Crossplane troubleshooting guide — https://docs.crossplane.io/latest/guides/troubleshoot-crossplane/
- `crossplane` CLI reference (`resource trace`) — https://docs.crossplane.io/latest/cli/command-reference/
- Crossplane v2.0 announcement — https://blog.crossplane.io/announcing-crossplane-2-0/
- Crossplane v2.2 (pipeline inspector, `--watch`) — https://blog.crossplane.io/crossplane-v2-2-more-capable-more-reliable-more-observable/
- "Composite waiting to become Ready" failure mode — https://github.com/crossplane/crossplane/issues/2957
- Official MCP Go SDK discussion — https://github.com/orgs/modelcontextprotocol/discussions/364
- Prior art: cychiang/crossplane-mcp-server (archived), upbound/marketplace-mcp-server, kkb0318/kubernetes-mcp, patrickdappollonio/mcp-kubernetes-ro, manusa/kubernetes-mcp-server
