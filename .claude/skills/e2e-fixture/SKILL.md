---
name: e2e-fixture
description: Spin up a local kind cluster with Crossplane and a deliberately-stuck composite resource, then run crossplane-mcp's `diagnose` against it to validate/tune root-cause ranking on real Crossplane resources. Use for real-cluster diagnose validation.
disable-model-invocation: true
---

# e2e-fixture

Stand up a **deterministic, credential-free** Crossplane environment whose
composite resource is stuck `Ready: False` while its composed managed resource
reports `Synced: True, Ready: False` — the signature failure mode `diagnose` is
built for — then run the server against it and check the output.

This is the harness for the project's "real-cluster diagnose validation" step.
It uses **provider-nop** so a managed resource can be made to hang without any
cloud account.

## Prerequisites

Check these and stop with a clear message if missing:

- `docker` running, `kind`, `kubectl`, `helm`
- `go` (to build the server) — or an installed `crossplane-mcp` on PATH
- Network access to pull Crossplane + packages

## Steps

Run from the repo root. Treat the bundled manifests as a **starting point** and
adapt at runtime (package versions and the XRD/composition API shapes drift —
verify current versions on the Upbound/Crossplane marketplace if a package fails
to become Healthy).

1. **Cluster.** Create an isolated cluster:
   ```sh
   kind create cluster --name xpmcp-e2e
   ```

2. **Install Crossplane (v2).**
   ```sh
   helm repo add crossplane-stable https://charts.crossplane.io/stable && helm repo update
   helm install crossplane crossplane-stable/crossplane \
     --namespace crossplane-system --create-namespace --wait
   ```
   Confirm it's v2 (`kubectl get deploy -n crossplane-system`); the fixture
   targets namespaced XRs.

3. **Install packages** (provider-nop + function-patch-and-transform):
   ```sh
   kubectl apply -f .claude/skills/e2e-fixture/manifests/00-packages.yaml
   kubectl wait --for=condition=Healthy provider.pkg.crossplane.io/provider-nop --timeout=180s
   kubectl wait --for=condition=Healthy function.pkg.crossplane.io/function-patch-and-transform --timeout=180s
   ```
   If a package isn't Healthy, check its version against the marketplace and
   update `00-packages.yaml`.

   Note: provider-nop v0.5.0 uses Crossplane v2 **"safe start"** — its
   `NopResource` CRD is created once the cluster's default
   ManagedResourceActivationPolicy (`["*"]`, the chart default) activates the
   MRD. The waits in step 4 cover that timing.

4. **Install the platform** (XRD + Composition), wait for the CRDs to register,
   then apply **the instance** (the XR):
   ```sh
   kubectl apply -f .claude/skills/e2e-fixture/manifests/10-platform.yaml
   kubectl wait --for=condition=Established crd/xstuckapps.example.org --timeout=60s
   # safe-start: the NopResource CRD appears after the default MRAP activates it
   kubectl wait --for=condition=Established crd/nopresources.nop.crossplane.io --timeout=120s
   kubectl apply -f .claude/skills/e2e-fixture/manifests/20-instance.yaml
   ```

5. **Let it settle**, then observe the broken tree with kubectl for ground truth
   (`NopResource` is namespaced in v0.5.0, so it lands in the XR's namespace):
   ```sh
   kubectl get xstuckapp demo -n default -o yaml          # XR: expect Ready False
   kubectl get nopresources.nop.crossplane.io -n default  # composed MR: Synced True, Ready False
   ```

6. **Run the server against it** and exercise `diagnose`. Build first
   (`make build` → `bin/crossplane-mcp`), then either:
   - wire it into your MCP client using the `mcpServers` config block in the
     [README](../../../README.md) (point it at `--context kind-xpmcp-e2e`), or
   - drive it over MCP stdio directly — newline-delimited JSON-RPC: `initialize`
     → `notifications/initialized` → `tools/call` for `diagnose`.

   Then call:
   - `diagnose { kind: "XStuckApp", name: "demo", namespace: "default" }`

7. **Assert (the validation):**
   - `healthy` is **false**.
   - The **deepest** suspect is the composed **NopResource** (not the top-level
     `XStuckApp`) — this is the core ranking behavior.
   - The NopResource's `Ready` condition message/reason is surfaced **untruncated**.
   - `get_resource_tree` shows `XStuckApp → NopResource` with per-node state.

   Triage:
   - If the leaf (NopResource) is **missing** from the tree entirely, it's a
     **traversal** bug, not ranking: v2 namespaced XRs keep composed refs under
     `spec.crossplane.resourceRefs` (not the v1 top-level `spec.resourceRefs`) —
     check `internal/xp/tree.go`.
   - If the leaf is present but the top-level XR outranks it, that's a **ranking**
     bug in `internal/xp/diagnose.go`. Use this fixture to iterate.

## Variations to try (extends coverage)

- Make the NopResource `Synced: False` too (an "apply error" shape).
- Add a second composed resource that is healthy, to confirm only the broken one
  is flagged.
- Nest a composite (an XR composing another XR) to test multi-level depth ranking.
- Create the instance in a non-`default` namespace to exercise namespace handling.

## Teardown

```sh
kind delete cluster --name xpmcp-e2e
```
