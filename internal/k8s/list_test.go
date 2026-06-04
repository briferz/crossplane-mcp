package k8s

import (
	"context"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

func discoFixture() *discoveryfake.FakeDiscovery {
	resources := []*metav1.APIResourceList{
		{
			GroupVersion: "legacy.example.org/v1",
			APIResources: []metav1.APIResource{
				// v1-style cluster-scoped XR.
				{Name: "xclusters", Kind: "XCluster", Namespaced: false, Categories: []string{"crossplane", "composite"}},
			},
		},
		{
			GroupVersion: "apps.example.org/v1",
			APIResources: []metav1.APIResource{
				// v2-style namespaced XR.
				{Name: "xapps", Kind: "XApp", Namespaced: true, Categories: []string{"crossplane", "composite"}},
				// Subresource — must be skipped despite inheriting categories.
				{Name: "xapps/status", Kind: "XApp", Namespaced: true, Categories: []string{"composite"}},
				// v1 claim.
				{Name: "appclaims", Kind: "AppClaim", Namespaced: true, Categories: []string{"crossplane", "claim"}},
				// Managed resource — only matched when category=managed.
				{Name: "buckets", Kind: "Bucket", Namespaced: true, Categories: []string{"crossplane", "managed"}},
				// Decoy with no Crossplane category.
				{Name: "configmaps", Kind: "ConfigMap", Namespaced: true},
			},
		},
		{
			// Same group+resource as apps.example.org/v1 xapps, second served
			// version — must dedup to one target.
			GroupVersion: "apps.example.org/v2",
			APIResources: []metav1.APIResource{
				{Name: "xapps", Kind: "XApp", Namespaced: true, Categories: []string{"composite"}},
			},
		},
	}
	return &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{Resources: resources}}
}

func findKind(kinds []CompositeKind, kind string) (CompositeKind, bool) {
	for _, k := range kinds {
		if k.Kind == kind {
			return k, true
		}
	}
	return CompositeKind{}, false
}

func TestDiscoverComposite(t *testing.T) {
	cl := &Client{Disco: discoFixture()}

	kinds, notes, err := cl.DiscoverComposite()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(notes) != 0 {
		t.Errorf("expected no notes, got %v", notes)
	}
	// Default categories composite+claim: XCluster, XApp, AppClaim. Bucket
	// (managed), ConfigMap (no category), and xapps/status (subresource) excluded.
	if len(kinds) != 3 {
		t.Fatalf("expected 3 discovered kinds, got %d: %+v", len(kinds), kinds)
	}

	xc, ok := findKind(kinds, "XCluster")
	if !ok || xc.Namespaced || xc.Category != "composite" {
		t.Errorf("XCluster should be cluster-scoped composite, got %+v (ok=%v)", xc, ok)
	}
	xa, ok := findKind(kinds, "XApp")
	if !ok || !xa.Namespaced || xa.Category != "composite" {
		t.Errorf("XApp should be namespaced composite, got %+v (ok=%v)", xa, ok)
	}
	if xa.GVR.Version != "v1" {
		t.Errorf("dedup should keep the first served version (v1), got %q", xa.GVR.Version)
	}
	cm, ok := findKind(kinds, "AppClaim")
	if !ok || !cm.Namespaced || cm.Category != "claim" {
		t.Errorf("AppClaim should be namespaced claim, got %+v (ok=%v)", cm, ok)
	}
	if _, ok := findKind(kinds, "Bucket"); ok {
		t.Error("Bucket (managed) must not appear under default categories")
	}
	if _, ok := findKind(kinds, "ConfigMap"); ok {
		t.Error("ConfigMap (no category) must not appear")
	}
}

func TestDiscoverCompositeManagedOptIn(t *testing.T) {
	cl := &Client{Disco: discoFixture()}
	kinds, _, err := cl.DiscoverComposite(CategoryManaged)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(kinds) != 1 || kinds[0].Kind != "Bucket" {
		t.Errorf("category=managed should return only Bucket, got %+v", kinds)
	}
}

// stubDisco lets a test drive ServerGroupsAndResources' (lists, err) directly —
// the stock fake does not surface a discovery error cleanly. Only the one method
// DiscoverComposite calls is implemented; the rest would panic if reached.
type stubDisco struct {
	discovery.DiscoveryInterface
	lists []*metav1.APIResourceList
	err   error
}

func (s stubDisco) ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
	return nil, s.lists, s.err
}

func TestDiscoverCompositePartialDiscovery(t *testing.T) {
	// An aggregated API group is unavailable, but some resources still came back:
	// degrade to a note, do not fail.
	lists := []*metav1.APIResourceList{
		{GroupVersion: "apps.example.org/v1", APIResources: []metav1.APIResource{
			{Name: "xapps", Kind: "XApp", Namespaced: true, Categories: []string{"composite"}},
		}},
	}
	cl := &Client{Disco: stubDisco{lists: lists, err: errors.New("group metrics.k8s.io is unavailable")}}
	kinds, notes, err := cl.DiscoverComposite()
	if err != nil {
		t.Fatalf("partial discovery must not hard-fail, got %v", err)
	}
	if len(kinds) != 1 {
		t.Errorf("expected the available kind despite partial discovery, got %d", len(kinds))
	}
	if !hasNote(notes, "partial discovery") {
		t.Errorf("expected a partial-discovery note, got %v", notes)
	}
}

func TestDiscoverCompositeHardError(t *testing.T) {
	cl := &Client{Disco: stubDisco{lists: nil, err: errors.New("discovery server down")}}
	_, _, err := cl.DiscoverComposite()
	if err == nil || !strings.Contains(err.Error(), "discover resources") {
		t.Errorf("expected a hard error when discovery returns nothing, got %v", err)
	}
}

func uobj(apiVersion, kind, ns, name string) *unstructured.Unstructured {
	meta := map[string]any{"name": name}
	if ns != "" {
		meta["namespace"] = ns
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion, "kind": kind, "metadata": meta,
	}}
}

func listFixture(t *testing.T) (*Client, []CompositeKind) {
	t.Helper()
	gvrToListKind := map[schema.GroupVersionResource]string{
		{Group: "apps.example.org", Version: "v1", Resource: "xapps"}:       "XAppList",
		{Group: "apps.example.org", Version: "v1", Resource: "appclaims"}:   "AppClaimList",
		{Group: "legacy.example.org", Version: "v1", Resource: "xclusters"}: "XClusterList",
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToListKind,
		uobj("apps.example.org/v1", "XApp", "team-a", "app1"),
		uobj("apps.example.org/v1", "AppClaim", "team-b", "claim1"),
		uobj("legacy.example.org/v1", "XCluster", "", "cluster1"),
	)
	kinds := []CompositeKind{
		{Target: Target{GVR: schema.GroupVersionResource{Group: "apps.example.org", Version: "v1", Resource: "xapps"}, Namespaced: true, Kind: "XApp"}, Category: "composite"},
		{Target: Target{GVR: schema.GroupVersionResource{Group: "apps.example.org", Version: "v1", Resource: "appclaims"}, Namespaced: true, Kind: "AppClaim"}, Category: "claim"},
		{Target: Target{GVR: schema.GroupVersionResource{Group: "legacy.example.org", Version: "v1", Resource: "xclusters"}, Namespaced: false, Kind: "XCluster"}, Category: "composite"},
	}
	return &Client{Dyn: dyn}, kinds
}

func TestListAllClusterWide(t *testing.T) {
	cl, kinds := listFixture(t)
	res := cl.ListAll(context.Background(), kinds, "")
	if len(res.Objects) != 3 {
		t.Fatalf("cluster-wide scan should list all 3 objects, got %d (notes %v)", len(res.Objects), res.Notes)
	}
	if len(res.Notes) != 0 {
		t.Errorf("expected no notes cluster-wide, got %v", res.Notes)
	}
}

func TestListAllNamespaceScopedSkipsClusterScoped(t *testing.T) {
	cl, kinds := listFixture(t)
	res := cl.ListAll(context.Background(), kinds, "team-a")
	// Only the team-a XApp; the team-b claim is elsewhere; the cluster-scoped
	// XCluster is skipped with a note.
	if len(res.Objects) != 1 || res.Objects[0].Object.GetName() != "app1" {
		t.Fatalf("expected only app1 in team-a, got %+v", res.Objects)
	}
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "cluster-scoped XCluster") {
		t.Errorf("expected a skip note for the cluster-scoped XR, got %v", res.Notes)
	}
}

func TestListAllForbiddenDegradesGracefully(t *testing.T) {
	cl, kinds := listFixture(t)
	dyn := cl.Dyn.(*dynamicfake.FakeDynamicClient)
	dyn.PrependReactor("list", "appclaims", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "apps.example.org", Resource: "appclaims"}, "", errors.New("rbac"))
	})
	res := cl.ListAll(context.Background(), kinds, "")
	// The forbidden type is skipped with a note; the others still list. No panic,
	// no propagated error (ListAll has no error return — that IS the contract).
	if len(res.Objects) != 2 {
		t.Fatalf("expected 2 objects when one type is forbidden, got %d", len(res.Objects))
	}
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "forbidden") {
		t.Errorf("expected a forbidden note, got %v", res.Notes)
	}
}

func TestListAllSkipNotes(t *testing.T) {
	forbidGVR := schema.GroupResource{Group: "apps.example.org", Resource: "xapps"}

	t.Run("not found", func(t *testing.T) {
		cl, kinds := listFixture(t)
		cl.Dyn.(*dynamicfake.FakeDynamicClient).PrependReactor("list", "xapps", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewNotFound(forbidGVR, "")
		})
		res := cl.ListAll(context.Background(), kinds, "")
		if !hasNote(res.Notes, "not found") {
			t.Errorf("expected a not-found note, got %v", res.Notes)
		}
	})

	t.Run("generic error", func(t *testing.T) {
		cl, kinds := listFixture(t)
		cl.Dyn.(*dynamicfake.FakeDynamicClient).PrependReactor("list", "xapps", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("boom")
		})
		res := cl.ListAll(context.Background(), kinds, "")
		if !hasNote(res.Notes, "boom") {
			t.Errorf("expected a generic-error note, got %v", res.Notes)
		}
	})

	t.Run("forbidden in namespace", func(t *testing.T) {
		cl, kinds := listFixture(t)
		cl.Dyn.(*dynamicfake.FakeDynamicClient).PrependReactor("list", "xapps", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(forbidGVR, "", errors.New("rbac"))
		})
		res := cl.ListAll(context.Background(), kinds, "team-a")
		if !hasNote(res.Notes, "in team-a") || !hasNote(res.Notes, "re-call with an explicit namespace") {
			t.Errorf("expected forbidden-in-namespace note with remediation hint, got %v", res.Notes)
		}
	})
}

func hasNote(notes []string, sub string) bool {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}
