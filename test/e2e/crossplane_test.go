package e2e

import (
	"context"
	"fmt"
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

func clusterClient(t *testing.T) *k8s.Client {
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
	cl := clusterClient(t)

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
	// A suspect that names itself and then explains nothing is the complaint this
	// tool exists to answer, and this assertion caught a real one against a live
	// cluster — the composed NopResource ranked top with an empty Reasons list.
	//
	// Two shapes produce that and the run could not tell them apart, because the
	// object was not in the artifact: a condition that is False but carries
	// neither reason nor message (conditionLine renders it "", so
	// blockingMessages drops it), or no conditions at all because the provider
	// had not reconciled yet. diagnose.go now explains both; the workflow now
	// collects the composed objects and waits for status, so a recurrence names
	// which one it was. Printing the conditions here is the third leg of that —
	// this tier runs weekly and nobody will still have the cluster.
	if len(top.Reasons) == 0 {
		t.Errorf("the top suspect must explain itself: %s/%s state=%s\nconditions as stored: %s",
			top.Kind, top.Name, nodeState(d, top.Kind, top.Name), describeStored(cl, top))
	}
	// Logged on SUCCESS too, deliberately. This tier's job is to notice when
	// upstream changes the shape of what it writes, and a passing run that
	// records nothing cannot do that. It also settles which shape the original
	// empty-Reasons failure was: a synthetic "(reported with no reason or
	// message)" lead means the provider writes bare conditions, while real
	// condition text means it does not and the earlier failure was the
	// wait-for-existence race that this workflow no longer has.
	t.Logf("top suspect %s/%s reasons: %q", top.Kind, top.Name, top.Reasons)
	t.Logf("as stored: %s", describeStored(cl, top))
}

// nodeState reads the state off the flat tree entry behind a suspect.
func nodeState(d *xp.Diagnosis, kind, name string) string {
	for _, n := range d.Tree {
		if n.Kind == kind && n.Name == name {
			return n.State
		}
	}
	return "<not in tree>"
}

// describeStored re-reads the suspect from the cluster and renders its raw
// conditions. FlatNode does not carry conditions and Suspect carries only the
// derived reasons — which is exactly the information missing when the derivation
// is what is suspect.
func describeStored(cl *k8s.Client, s xp.Suspect) string {
	target, err := cl.Resolve(s.APIVersion, s.Kind)
	if err != nil {
		return fmt.Sprintf("<resolve failed: %v>", err)
	}
	obj, err := cl.Get(context.Background(), target, s.Namespace, s.Name)
	if err != nil {
		return fmt.Sprintf("<get failed: %v>", err)
	}
	conds := xp.Conditions(obj)
	if len(conds) == 0 {
		return "<no status conditions at all>"
	}
	var out string
	for _, c := range conds {
		out += fmt.Sprintf("[%s=%s reason=%q message=%q] ", c.Type, c.Status, c.Reason, c.Message)
	}
	return out
}

// TestCrossplaneV1ClaimAndClusterScopedXR covers the other half of hard rule 2:
// a namespaced Claim -> cluster-scoped XR (spec.resourceRef) -> composed
// resources (spec.resourceRefs). Nothing in the v2 fixture reaches this path.
func TestCrossplaneV1ClaimAndClusterScopedXR(t *testing.T) {
	requireCrossplane(t)
	cl := clusterClient(t)

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
	cl := clusterClient(t)

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
	cl := clusterClient(t)

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
	cl := clusterClient(t)

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
