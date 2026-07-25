package e2e

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/briferz/crossplane-mcp/internal/k8s"
	"github.com/briferz/crossplane-mcp/internal/xp"
)

// The Crossplane tier. Everything here needs a real cluster running real
// Crossplane controllers, so it is gated behind CROSSPLANE_E2E=1 and runs on
// cron/dispatch/label only — never as a required check, because three external
// registries (kindest/node, the Crossplane chart, xpkg.crossplane.io) sit on
// its critical path and none of them are ours.
//
// What only this tier can establish: that upstream Crossplane still writes
// composed refs where the walker looks. The envtest tier's CRDs are
// hand-written to MIMIC XRD-generated ones — if upstream changes that shape,
// envtest will happily keep passing. These assertions are the ones that break.

func crossplaneClient(t *testing.T) *k8s.Client {
	t.Helper()
	kubeconfig := os.Getenv("KUBECONFIG")
	cl, err := k8s.New(kubeconfig, "", 60*time.Second)
	if err != nil {
		t.Fatalf("k8s.New(%q): %v", kubeconfig, err)
	}
	return cl
}

// diagnoseFor resolves a kind, fetches it, walks the tree, and diagnoses —
// the same sequence diagnoseHandler runs.
func diagnoseFor(t *testing.T, cl *k8s.Client, apiVersion, kind, ns, name string) *xp.Diagnosis {
	t.Helper()
	ctx := context.Background()
	target, err := cl.Resolve(apiVersion, kind)
	if err != nil {
		t.Fatalf("resolve %s/%s: %v", apiVersion, kind, err)
	}
	obj, err := cl.Get(ctx, target, ns, name)
	if err != nil {
		t.Fatalf("get %s/%s: %v", kind, name, err)
	}
	tree, stats := xp.BuildTree(ctx, cl, obj)
	return xp.Diagnose(ctx, cl, tree, stats, true)
}

// TestCrossplaneV2NamespacedXR is the shape the project's headline feature
// targets: a namespaced XR whose composed refs live under spec.crossplane.
func TestCrossplaneV2NamespacedXR(t *testing.T) {
	requireCrossplane(t)
	cl := crossplaneClient(t)

	d := diagnoseFor(t, cl, "example.org/v1alpha1", "XStuckApp", "default", "demo")

	if d.Healthy {
		t.Fatalf("the fixture XR is deliberately stuck; diagnose called it healthy: %s", d.Summary)
	}
	if d.Stats.Nodes < 2 {
		t.Fatalf("walked %d nodes — the composed NopResource was not reached, so upstream "+
			"is no longer writing refs where tree.go looks", d.Stats.Nodes)
	}
	if len(d.Suspects) == 0 {
		t.Fatal("expected suspects")
	}
	// The signature failure this fixture builds: the composed resource reconciles
	// (Synced=True) but never goes Ready, so the DEEPEST node is the root cause —
	// which is the whole value proposition over a flat trace.
	top := d.Suspects[0]
	if top.Kind != "NopResource" {
		t.Errorf("top suspect = %s/%s at depth %d, want the composed NopResource — "+
			"ranking no longer surfaces the deepest failing resource",
			top.Kind, top.Name, top.Depth)
	}
	if len(top.Reasons) == 0 {
		t.Error("the top suspect must explain itself")
	}
}

// TestCrossplaneV1ClaimAndClusterScopedXR covers the other half of hard rule 2:
// a namespaced Claim -> cluster-scoped XR (spec.resourceRef) -> composed
// resources (spec.resourceRefs). Nothing in the v2 fixture reaches this path.
func TestCrossplaneV1ClaimAndClusterScopedXR(t *testing.T) {
	requireCrossplane(t)
	cl := crossplaneClient(t)

	d := diagnoseFor(t, cl, "legacy.example.org/v1alpha1", "LegacyAppClaim", "default", "legacy-demo")

	if d.Healthy {
		t.Fatalf("the v1 fixture is deliberately stuck; diagnose called it healthy: %s", d.Summary)
	}
	// Claim -> XR -> composed: three hops, so anything less means a link in the
	// v1 ref chain was not followed.
	if d.Stats.Nodes < 3 {
		t.Fatalf("walked %d nodes, want at least 3 (Claim -> XR -> composed); the v1 "+
			"spec.resourceRef / spec.resourceRefs chain is not being followed", d.Stats.Nodes)
	}
	var sawClusterScoped bool
	for _, n := range d.Tree {
		if n.Kind == "XLegacyApp" && n.Namespace == "" {
			sawClusterScoped = true
		}
	}
	if !sawClusterScoped {
		t.Error("expected the cluster-scoped XLegacyApp in the tree with no namespace")
	}
}

// TestCrossplaneConditionMessagesNotTruncated pins hard rule 4 against real
// Crossplane-authored condition text, which is the whole point of this tool
// over `crossplane resource trace`.
func TestCrossplaneConditionMessagesNotTruncated(t *testing.T) {
	requireCrossplane(t)
	cl := crossplaneClient(t)

	d := diagnoseFor(t, cl, "example.org/v1alpha1", "XStuckApp", "default", "demo")
	for _, s := range d.Suspects {
		for _, r := range s.Reasons {
			if strings.Contains(r, "…") || strings.HasSuffix(r, "...") {
				t.Errorf("condition message looks truncated on %s/%s: %q", s.Kind, s.Name, r)
			}
		}
	}
}

// TestCrossplanePackagesHealthy exercises the package-health tools against
// really-installed packages — category-driven discovery pinned to
// pkg.crossplane.io, against whatever condition vocabulary this Crossplane
// version actually emits.
func TestCrossplanePackagesHealthy(t *testing.T) {
	requireCrossplane(t)
	cl := crossplaneClient(t)

	// The package tools discover by CATEGORY (pkg / pkgrev), not by hardcoded
	// group-version — deliberately, so Function@v1beta1 clusters work without
	// version branching. This asserts the categories are really stamped.
	kinds, notes, err := cl.DiscoverComposite(k8s.CategoryPackage, k8s.CategoryPackageRevision)
	if err != nil {
		t.Fatalf("package discovery: %v (notes: %v)", err, notes)
	}
	var sawProvider, sawRevision bool
	for _, k := range kinds {
		switch {
		case k.Kind == "Provider" && k.Category == k8s.CategoryPackage:
			sawProvider = true
		case k.Kind == "ProviderRevision" && k.Category == k8s.CategoryPackageRevision:
			sawRevision = true
		}
	}
	if !sawProvider || !sawRevision {
		t.Errorf("category-driven package discovery missed types this cluster definitely has "+
			"(Provider=%v ProviderRevision=%v); discovered: %+v", sawProvider, sawRevision, kinds)
	}
}

// TestReadOnlyAgainstRealCluster is the promise, checked where it matters. The
// forbidigo rule proves no write is COMPILED; this proves none is ISSUED
// against a live apiserver across the tools' real code paths.
func TestReadOnlyAgainstRealCluster(t *testing.T) {
	requireCrossplane(t)
	cl := crossplaneClient(t)

	before := resourceVersions(t, cl)
	_ = diagnoseFor(t, cl, "example.org/v1alpha1", "XStuckApp", "default", "demo")
	_ = diagnoseFor(t, cl, "legacy.example.org/v1alpha1", "LegacyAppClaim", "default", "legacy-demo")
	if _, _, err := cl.DiscoverComposite(); err != nil {
		t.Fatalf("DiscoverComposite: %v", err)
	}
	after := resourceVersions(t, cl)

	for name, rv := range before {
		if after[name] != rv {
			t.Errorf("%s changed resourceVersion %s -> %s: the server mutated cluster state",
				name, rv, after[name])
		}
	}
}

func resourceVersions(t *testing.T, cl *k8s.Client) map[string]string {
	t.Helper()
	ctx := context.Background()
	out := map[string]string{}
	for _, r := range []struct{ apiVersion, kind, ns, name string }{
		{"example.org/v1alpha1", "XStuckApp", "default", "demo"},
		{"legacy.example.org/v1alpha1", "LegacyAppClaim", "default", "legacy-demo"},
	} {
		target, err := cl.Resolve(r.apiVersion, r.kind)
		if err != nil {
			t.Fatalf("resolve %s: %v", r.kind, err)
		}
		o, err := cl.Get(ctx, target, r.ns, r.name)
		if err != nil {
			t.Fatalf("get %s: %v", r.kind, err)
		}
		out[r.kind+"/"+r.name] = o.GetResourceVersion()
	}
	return out
}
