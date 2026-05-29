# crossplane-mcp

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

## Build

```sh
go build -o bin/crossplane-mcp ./cmd/crossplane-mcp
# or
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

## Development

```sh
make test   # unit tests (no cluster required)
make vet
make fmt
```

## License

TBD.
