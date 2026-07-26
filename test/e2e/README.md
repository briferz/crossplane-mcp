# Integration tests

Runs `crossplane-mcp` against real Kubernetes — how real depends on the tier.

```sh
make e2e-envtest     # tier 1: resolves apiserver/etcd binaries, runs the suite (~11s)
make e2e-native      # tier 2: any real cluster (kind is enough); no Crossplane
make e2e-crossplane  # tier 3: needs a real cluster with Crossplane installed
make e2e-vet         # compile-check only (also part of `make check`)
```

Three tiers live in this one module, selected by environment rather than build
tags — one `go vet`, and no tag combination that compiles in CI but not locally:

| | Selected by | Has | Runs |
|---|---|---|---|
| **envtest** | default (needs `KUBEBUILDER_ASSETS`) | apiserver + etcd | every PR, ~11s |
| **native** | `CLUSTER_E2E=1` + a kubeconfig | + controllers, kubelet | weekly cron, `workflow_dispatch`, or an `e2e` PR label |
| **Crossplane** | `CROSSPLANE_E2E=1` + a kubeconfig | + Crossplane | same triggers |

They are ordered by how much can break underneath them. envtest depends on
nothing external; native adds `kindest/node` and one pause image; Crossplane
adds three registries nobody here controls. That ordering is why they are
separate CI jobs — a marketplace outage should not obscure a native-readiness
regression, and only the Crossplane tier is fragile enough to need that
allowance.

Setting either variable asserts a cluster exists; without one the tests fail
rather than skip, because a silent skip is how a tier stops running and nobody
notices. `CROSSPLANE_E2E=1` implies `CLUSTER_E2E`, since a Crossplane cluster
necessarily has controllers.

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

## The native tier (`CLUSTER_E2E=1`)

`internal/xp/native.go` encodes how upstream controllers write readiness into
status — per `(group, kind)`, never a polarity heuristic. Its unit tests assert
against conditions **this repo authored**, so they prove the rules read their own
fixtures back and nothing more. envtest cannot help: no kube-controller-manager
means no Deployment controller, and no kubelet means no pod ever fails to start.

| Test | What only real controllers can establish |
|---|---|
| `TestNativeReadinessDeploymentRolloutRecovery` | That `Progressing=False/ProgressDeadlineExceeded` is **transient**, not terminal. `deploymentReadiness` calls that pair Blocked with no "only if `Available != True`" guard — correctly, since a stuck rollout keeps serving under `maxSurge`. But if the controller never reset the condition, the same rule would strand a fully recovered Deployment at Blocked forever. The probe wedges a rollout, fixes it, and reads back what the controller actually did. |
| `TestNativeReadinessSucceededPodIsReady` | That a real terminated pod still reports `Ready=False/PodCompleted`. This is the shape behind the live false positive `native.go` was written for — a finished init or migration Pod ranked as the top suspect, permanently. If Kubernetes ever changes it, the Pod rule silently stops applying. |

The rollout probe is offline by construction on the failing half:
`imagePullPolicy: Never` plus an image that cannot exist locally fails instantly
with `ErrImageNeverPull` and touches no registry, so there is no pull backoff to
race. The recovery half needs one real image; CI preloads it with `kind load
docker-image`, and `PROBE_IMAGE` overrides it elsewhere.

## Fixtures

`testdata/crds/` is installed at startup. `testdata/latecrd/` is installed
**mid-test, on purpose** — it is the provider-install-while-running case, and
splitting the directories is what keeps it invisible until then.

## Version pinning

`ENVTEST_K8S_VERSION` in the Makefile tracks the repo's `client-go` minor
(currently `1.36.2` against `client-go v0.36.3`). Bump both together: testing
against a materially different apiserver than the one the client targets is how
this tier would quietly stop meaning anything.
