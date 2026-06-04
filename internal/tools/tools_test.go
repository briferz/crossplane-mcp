package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/briferz/crossplane-mcp/internal/k8s"
	"github.com/briferz/crossplane-mcp/internal/xp"
)

// TestRegisterAllToolsSchemas guards that every tool's input/output schema is
// inference-safe: the SDK's schema inferer rejects recursive Go types, so a bad
// output struct would panic here. No cluster access happens during registration.
func TestRegisterAllToolsSchemas(t *testing.T) {
	s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	Register(s, &k8s.Client{}, nil)
}

func uobj(apiVersion, kind, ns, name string, conds ...map[string]any) *unstructured.Unstructured {
	meta := map[string]any{"name": name}
	if ns != "" {
		meta["namespace"] = ns
	}
	o := map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": meta}
	if conds != nil {
		cs := make([]any, len(conds))
		for i, c := range conds {
			cs[i] = c
		}
		o["status"] = map[string]any{"conditions": cs}
	}
	return &unstructured.Unstructured{Object: o}
}

func condM(typ, status string) map[string]any {
	return map[string]any{"type": typ, "status": status}
}

// listUnhealthyClient wires a fake discovery + dynamic client modelling a mixed
// cluster: namespaced v2 XR (XApp) and claim (AppClaim), plus a cluster-scoped
// v1 XR (XCluster), with a blocked, a ready, and a pending instance.
func listUnhealthyClient() *k8s.Client {
	resources := []*metav1.APIResourceList{
		{GroupVersion: "apps.example.org/v1", APIResources: []metav1.APIResource{
			{Name: "xapps", Kind: "XApp", Namespaced: true, Categories: []string{"composite"}},
			{Name: "appclaims", Kind: "AppClaim", Namespaced: true, Categories: []string{"claim"}},
		}},
		{GroupVersion: "legacy.example.org/v1", APIResources: []metav1.APIResource{
			{Name: "xclusters", Kind: "XCluster", Namespaced: false, Categories: []string{"composite"}},
		}},
	}
	disco := &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{Resources: resources}}

	gvrToListKind := map[schema.GroupVersionResource]string{
		{Group: "apps.example.org", Version: "v1", Resource: "xapps"}:       "XAppList",
		{Group: "apps.example.org", Version: "v1", Resource: "appclaims"}:   "AppClaimList",
		{Group: "legacy.example.org", Version: "v1", Resource: "xclusters"}: "XClusterList",
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToListKind,
		uobj("apps.example.org/v1", "XApp", "team-a", "blocked-app", condM("Ready", "False")),
		uobj("apps.example.org/v1", "XApp", "team-a", "ready-app", condM("Ready", "True"), condM("Synced", "True")),
		uobj("apps.example.org/v1", "AppClaim", "team-a", "pending-claim"), // no conditions -> Pending
		uobj("legacy.example.org/v1", "XCluster", "", "blocked-cluster", condM("Synced", "False")),
	)
	return &k8s.Client{Dyn: dyn, Disco: disco}
}

func TestListUnhealthyHandler(t *testing.T) {
	h := listUnhealthyHandler(listUnhealthyClient())

	_, out, err := h(context.Background(), nil, ListUnhealthyInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Scanned != 4 {
		t.Errorf("Scanned = %d, want 4", out.Scanned)
	}
	if out.Summary.Blocked != 2 || out.Summary.Pending != 1 || out.Summary.Ready != 1 {
		t.Errorf("summary = %+v, want blocked/pending/ready 2/1/1", out.Summary)
	}
	if len(out.Items) != 3 {
		t.Fatalf("default should return 3 unhealthy items, got %d: %+v", len(out.Items), out.Items)
	}
	// Blocked before pending; the Ready instance is excluded.
	for _, it := range out.Items {
		if it.Name == "ready-app" {
			t.Error("Ready resource must not be returned by default")
		}
	}
	if out.Items[0].State != xp.StateBlocked || out.Items[2].State != xp.StatePending {
		t.Errorf("expected blocked first, pending last; got %s … %s", out.Items[0].State, out.Items[2].State)
	}
	// The cluster-scoped XR is included cluster-wide with an empty namespace.
	var sawCluster bool
	for _, it := range out.Items {
		if it.Kind == "XCluster" {
			sawCluster = true
			if it.Namespace != "" {
				t.Errorf("cluster-scoped XR should have empty namespace, got %q", it.Namespace)
			}
		}
	}
	if !sawCluster {
		t.Error("expected the cluster-scoped XCluster among results")
	}
}

func TestListUnhealthyHandlerNamespaceScoped(t *testing.T) {
	h := listUnhealthyHandler(listUnhealthyClient())

	_, out, err := h(context.Background(), nil, ListUnhealthyInput{Namespace: "team-a"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// team-a has the blocked XApp and the pending claim; the cluster-scoped XR is
	// skipped with a note.
	for _, it := range out.Items {
		if it.Kind == "XCluster" {
			t.Errorf("cluster-scoped XR must be skipped when a namespace filter is set, got %+v", it)
		}
	}
	if len(out.Items) != 2 {
		t.Errorf("expected 2 items in team-a, got %d: %+v", len(out.Items), out.Items)
	}
	var noted bool
	for _, n := range out.Notes {
		if strings.Contains(n, "cluster-scoped XCluster") {
			noted = true
		}
	}
	if !noted {
		t.Errorf("expected a skip note for the cluster-scoped XR, got %v", out.Notes)
	}
}

func TestListUnhealthyHandlerIncludeHealthy(t *testing.T) {
	h := listUnhealthyHandler(listUnhealthyClient())
	_, out, err := h(context.Background(), nil, ListUnhealthyInput{IncludeHealthy: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Items) != 4 {
		t.Fatalf("IncludeHealthy should return all 4 items, got %d: %+v", len(out.Items), out.Items)
	}
	var sawReady bool
	for _, it := range out.Items {
		if it.Name == "ready-app" && it.State == xp.StateReady {
			sawReady = true
		}
	}
	if !sawReady {
		t.Error("expected the Ready app to appear with IncludeHealthy")
	}
	if out.Summary.Blocked != 2 || out.Summary.Pending != 1 || out.Summary.Ready != 1 {
		t.Errorf("summary should be unchanged 2/1/1, got %+v", out.Summary)
	}
}

func TestListUnhealthyHandlerUnknownCategory(t *testing.T) {
	h := listUnhealthyHandler(listUnhealthyClient())
	_, _, err := h(context.Background(), nil, ListUnhealthyInput{Category: "composites"}) // typo
	if err == nil || !strings.Contains(err.Error(), "unknown category") {
		t.Errorf("expected an unknown-category error, got %v", err)
	}
}

func TestListUnhealthyHandlerTrimsNamespace(t *testing.T) {
	h := listUnhealthyHandler(listUnhealthyClient())
	_, out, err := h(context.Background(), nil, ListUnhealthyInput{Namespace: "   "})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// A whitespace-only namespace must be treated as cluster-wide (trimmed to
	// empty), so the cluster-scoped XR appears rather than being skipped.
	var sawCluster bool
	for _, it := range out.Items {
		if it.Kind == "XCluster" {
			sawCluster = true
		}
	}
	if !sawCluster {
		t.Error("whitespace namespace should be trimmed to a cluster-wide scan (XCluster expected)")
	}
}

func TestListUnhealthyHandlerKindFilter(t *testing.T) {
	h := listUnhealthyHandler(listUnhealthyClient())
	_, out, err := h(context.Background(), nil, ListUnhealthyInput{Kind: "appclaim"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Items) != 1 || out.Items[0].Kind != "AppClaim" {
		t.Errorf("kind filter should return only the AppClaim, got %+v", out.Items)
	}
}

func TestListUnhealthyHandlerEmptyCategoryNote(t *testing.T) {
	h := listUnhealthyHandler(listUnhealthyClient())
	// No managed resources are registered, so discovery finds nothing.
	_, out, err := h(context.Background(), nil, ListUnhealthyInput{Category: "managed"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Items) != 0 {
		t.Errorf("expected no items for an absent category, got %+v", out.Items)
	}
	var noted bool
	for _, n := range out.Notes {
		if strings.Contains(n, "no resource types found") {
			noted = true
		}
	}
	if !noted {
		t.Errorf("expected a 'no resource types found' note, got %v", out.Notes)
	}
}

func TestClampLimit(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, defaultListLimit}, {-5, defaultListLimit}, {50, 50}, {500, 500}, {1000, maxListLimit},
	}
	for _, c := range cases {
		if got := clampLimit(c.in); got != c.want {
			t.Errorf("clampLimit(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}
