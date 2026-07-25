# Integration tests (envtest)

Runs `crossplane-mcp` against a **real kube-apiserver + etcd** — no kubelet, no
controllers, no scheduler.

```sh
make e2e-envtest     # resolves the apiserver/etcd binaries, then runs the suite
make e2e-vet         # compile-check only (also part of `make check`)
```

## Why this is a separate Go module

Not stylistic. Two properties depend on it, and a `//go:build` tag would give
neither:

- **The shipped `go.mod` stays byte-identical.** `go mod tidy` resolves imports
  in build-tagged files too, so a tagged harness would pull `controller-runtime`
  and `apiextensions-apiserver` into the released binary's dependency graph —
  and therefore into what `govulncheck` gates on.
- **The read-only lint rule keeps its exact semantics.** The harness *must*
  write (CRDs, CRs, events) while the server under test must only ever read.
  `golangci-lint run ./...` does not descend into nested modules, so the
  `forbidigo` rule that bans dynamic-client writes needs **no exclusion and no
  `//nolint`** — which is what would actually blunt it.

Both are verified: `go test ./...` from the repo root skips this module, and
lint reports `0 issues` even though the harness calls `Create` on the dynamic
client.

## What this tier covers

Paths where a fake stands in for the apiserver, so a unit test cannot reach the
real behaviour:

| Test | What only a real apiserver can establish |
|---|---|
| `TestDiscoveryInvalidatesOnMiss` | A CRD installed **mid-session** becomes visible. The unit test asks a stub whether it was invalidated; the stub *is* the thing under question. Here the memcache, the `DeferredDiscoveryRESTMapper`, and the apiserver are all real. |
| `TestEventsUseFieldSelector` | `involvedObject.uid` actually filters. `dynamicfake` ignores field selectors entirely, so a broken selector passes the whole unit suite while attributing one resource's events to another. |
| `TestTreeWalkBothRefShapes` | Both `spec.resourceRefs` (v1, cluster-scoped) and `spec.crossplane.resourceRefs` (v2, namespaced) against stored, served objects. |
| `TestDiscoveryCategoriesSurviveTheWire` | The `composite`/`claim`/`managed` categories survive real aggregated discovery — the mechanism `list_unhealthy` and the package tools key on. |

Running at all also exercises the real TLS/HTTP-2 transport, kubeconfig loading,
and `rest.Config.Timeout` — the tripwire for a `client-go` or `x/net` bump.

## What it deliberately cannot cover

No controllers run, so nothing here says anything about **Crossplane's own**
behaviour: whether `crossplane-view` really aggregates, whether upstream still
writes composed refs where the walker looks, real package installs, or real
condition vocabularies. The CRDs in `testdata/crds/` are hand-written to *mimic*
XRD-generated ones; if upstream changes that shape, this tier will not notice.

That is the kind + real-Crossplane tier's job, and the reason it exists
separately.

## Fixtures

`testdata/crds/` is installed at startup. `testdata/latecrd/` is installed
**mid-test, on purpose** — it is the provider-install-while-running case, and
splitting the directories is what keeps it invisible until then.

## Version pinning

`ENVTEST_K8S_VERSION` in the Makefile tracks the repo's `client-go` minor
(currently `1.36.2` against `client-go v0.36.3`). Bump both together: testing
against a materially different apiserver than the one the client targets is how
this tier would quietly stop meaning anything.
