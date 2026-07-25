package tools

import (
	"context"
	"testing"

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

// The tree walk's client-facing half — per-ref Resolve, the v2 parent-namespace
// inheritance branch, the visited/cycle dedup, and both fetchChild error paths —
// runs against a real client only here. The pure ref-parsing side is covered in
// internal/xp/tree_test.go; everything after it was previously unexercised, so a
// regression in effective-namespace resolution or dedup would have passed CI on
// the project's two headline tools.

// refM builds a composed-resource ref as it appears in unstructured spec data.
// An empty namespace is omitted rather than written as "" — the common v2 shape,
// where composed refs inherit the parent XR's namespace.
func refM(apiVersion, kind, namespace, name string) map[string]any {
	m := map[string]any{"apiVersion": apiVersion, "kind": kind, "name": name}
	if namespace != "" {
		m["namespace"] = namespace
	}
	return m
}

// setRefs writes refs at the given spec path: {"spec","resourceRefs"} for v1
// composites, {"spec","crossplane","resourceRefs"} for v2 namespaced ones.
func setRefs(t *testing.T, o *unstructured.Unstructured, refs []map[string]any, path ...string) {
	t.Helper()
	list := make([]any, len(refs))
	for i, r := range refs {
		list[i] = r
	}
	if err := unstructured.SetNestedSlice(o.Object, list, path...); err != nil {
		t.Fatalf("setRefs %v: %v", path, err)
	}
}

// treeClient models one v2 namespaced tree and one v1 cluster-scoped tree.
//
// The v2 root (XApp/app1) deliberately covers every branch of build()'s child
// loop in a single walk: two refs that omit namespace (inheritance), a ref to a
// kind absent from discovery (Resolve failure), a ref whose kind resolves but
// whose object does not exist (Get failure), and — from its child — a ref back
// to the root (cycle dedup).
func treeClient(t *testing.T) *k8s.Client {
	t.Helper()

	resources := []*metav1.APIResourceList{
		{GroupVersion: "apps.example.org/v1", APIResources: []metav1.APIResource{
			{Name: "xapps", SingularName: "xapp", Kind: "XApp", Namespaced: true},
			{Name: "xdatabases", SingularName: "xdatabase", Kind: "XDatabase", Namespaced: true},
		}},
		{GroupVersion: "infra.example.org/v1", APIResources: []metav1.APIResource{
			{Name: "buckets", SingularName: "bucket", Kind: "Bucket", Namespaced: true},
			{Name: "queues", SingularName: "queue", Kind: "Queue", Namespaced: true},
			// Widget resolves but has no objects, exercising fetchChild's Get
			// failure. Ghost is deliberately absent, exercising its Resolve failure.
			{Name: "widgets", SingularName: "widget", Kind: "Widget", Namespaced: true},
		}},
		{GroupVersion: "legacy.example.org/v1", APIResources: []metav1.APIResource{
			{Name: "xclusters", SingularName: "xcluster", Kind: "XCluster", Namespaced: false},
			{Name: "clusterbuckets", SingularName: "clusterbucket", Kind: "ClusterBucket", Namespaced: false},
		}},
	}
	disco := &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{Resources: resources}}

	app1 := uobj("apps.example.org/v1", "XApp", "team-a", "app1", condM("Ready", "False"))
	setRefs(t, app1, []map[string]any{
		refM("apps.example.org/v1", "XDatabase", "", "db1"),
		refM("infra.example.org/v1", "Bucket", "", "bucket1"),
		refM("infra.example.org/v1", "Ghost", "", "ghost1"),
		refM("infra.example.org/v1", "Widget", "", "missing-widget"),
	}, "spec", "crossplane", "resourceRefs")

	db1 := uobj("apps.example.org/v1", "XDatabase", "team-a", "db1", condM("Ready", "True"), condM("Synced", "True"))
	setRefs(t, db1, []map[string]any{
		refM("infra.example.org/v1", "Queue", "team-a", "queue1"),
		refM("apps.example.org/v1", "XApp", "", "app1"),
	}, "spec", "crossplane", "resourceRefs")

	bucket1 := uobj("infra.example.org/v1", "Bucket", "team-a", "bucket1", condM("Ready", "True"), condM("Synced", "True"))
	queue1 := uobj("infra.example.org/v1", "Queue", "team-a", "queue1", condM("Synced", "False"))

	cluster1 := uobj("legacy.example.org/v1", "XCluster", "", "cluster1", condM("Synced", "False"))
	setRefs(t, cluster1, []map[string]any{
		refM("legacy.example.org/v1", "ClusterBucket", "", "cb1"),
	}, "spec", "resourceRefs")
	cb1 := uobj("legacy.example.org/v1", "ClusterBucket", "", "cb1", condM("Ready", "True"))

	gvrToListKind := map[schema.GroupVersionResource]string{
		{Group: "apps.example.org", Version: "v1", Resource: "xapps"}:            "XAppList",
		{Group: "apps.example.org", Version: "v1", Resource: "xdatabases"}:       "XDatabaseList",
		{Group: "infra.example.org", Version: "v1", Resource: "buckets"}:         "BucketList",
		{Group: "infra.example.org", Version: "v1", Resource: "queues"}:          "QueueList",
		{Group: "infra.example.org", Version: "v1", Resource: "widgets"}:         "WidgetList",
		{Group: "legacy.example.org", Version: "v1", Resource: "xclusters"}:      "XClusterList",
		{Group: "legacy.example.org", Version: "v1", Resource: "clusterbuckets"}: "ClusterBucketList",
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToListKind,
		app1, db1, bucket1, queue1, cluster1, cb1)
	return &k8s.Client{Dyn: dyn, Disco: disco, Mapper: errMapper{}}
}

// nodeNamed finds a flattened node by name, failing the test when absent.
func nodeNamed(t *testing.T, nodes []xp.FlatNode, name string) xp.FlatNode {
	t.Helper()
	for _, n := range nodes {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("no node named %q in %d nodes", name, len(nodes))
	return xp.FlatNode{}
}

func countNamed(nodes []xp.FlatNode, name string) int {
	n := 0
	for _, node := range nodes {
		if node.Name == name {
			n++
		}
	}
	return n
}

// TestTreeHandlerV2MultiHop walks a namespaced v2 XR end to end and pins every
// branch of the child loop: namespace inheritance for refs that omit it, cycle
// dedup on a back-reference to the root, and both fetchChild error paths.
func TestTreeHandlerV2MultiHop(t *testing.T) {
	h := treeHandler(treeClient(t))

	_, out, err := h(context.Background(), nil, TreeInput{
		APIVersion: "apps.example.org/v1", Kind: "XApp", Namespace: "team-a", Name: "app1",
	})
	if err != nil {
		t.Fatalf("tree: %v", err)
	}

	// root + db1 + queue1 + bucket1 + ghost1(error) + missing-widget(error).
	// The db1 -> app1 back-ref contributes no node.
	if out.Stats.Nodes != 6 {
		t.Errorf("Stats.Nodes = %d, want 6 (nodes: %v)", out.Stats.Nodes, names(out.Nodes))
	}
	if out.Stats.Capped {
		t.Error("a 6-node tree must not be reported as capped")
	}

	// Refs that omit namespace inherit the parent XR's.
	for _, name := range []string{"db1", "bucket1"} {
		n := nodeNamed(t, out.Nodes, name)
		if n.Namespace != "team-a" {
			t.Errorf("%s.Namespace = %q, want team-a (inherited from the parent XR)", name, n.Namespace)
		}
		if n.Depth != 1 {
			t.Errorf("%s.Depth = %d, want 1", name, n.Depth)
		}
	}

	queue := nodeNamed(t, out.Nodes, "queue1")
	if queue.Depth != 2 || queue.Namespace != "team-a" || queue.State != xp.StateBlocked {
		t.Errorf("queue1 = depth %d ns %q state %q, want 2/team-a/%s",
			queue.Depth, queue.Namespace, queue.State, xp.StateBlocked)
	}

	// The back-reference from db1 to the root must be skipped by the visited
	// dedup rather than re-expanded as its own subtree.
	if got := countNamed(out.Nodes, "app1"); got != 1 {
		t.Errorf("found %d nodes named app1, want exactly 1 — the cycle back-ref was not deduped", got)
	}

	// A kind absent from discovery fails to Resolve; a kind that resolves but
	// whose object is missing fails to Get. Both must surface as error nodes
	// rather than silently vanishing from the tree.
	for _, name := range []string{"ghost1", "missing-widget"} {
		n := nodeNamed(t, out.Nodes, name)
		if n.Error == "" {
			t.Errorf("%s must carry the fetch error, got none", name)
		}
		if n.State != xp.StatePending {
			t.Errorf("%s.State = %q, want %s", name, n.State, xp.StatePending)
		}
	}
}

// TestTreeHandlerV1ClusterScoped walks a cluster-scoped v1 XR through the
// top-level spec.resourceRefs path, the shape the v2 test does not reach.
func TestTreeHandlerV1ClusterScoped(t *testing.T) {
	h := treeHandler(treeClient(t))

	_, out, err := h(context.Background(), nil, TreeInput{
		APIVersion: "legacy.example.org/v1", Kind: "XCluster", Name: "cluster1",
	})
	if err != nil {
		t.Fatalf("tree: %v", err)
	}

	if out.Stats.Nodes != 2 {
		t.Fatalf("Stats.Nodes = %d, want 2 (nodes: %v)", out.Stats.Nodes, names(out.Nodes))
	}
	for _, n := range out.Nodes {
		if n.Namespace != "" {
			t.Errorf("cluster-scoped node %s carries namespace %q, want empty", n.Name, n.Namespace)
		}
	}
	cb := nodeNamed(t, out.Nodes, "cb1")
	if cb.Depth != 1 {
		t.Errorf("cb1.Depth = %d, want 1", cb.Depth)
	}
}

// TestDiagnoseHandlerWiresTreeWalk is a smoke check that diagnose consumes the
// same walk as get_resource_tree — not a re-test of the ranking itself, which
// internal/xp/diagnose_test.go already covers.
func TestDiagnoseHandlerWiresTreeWalk(t *testing.T) {
	h := diagnoseHandler(treeClient(t))

	_, out, err := h(context.Background(), nil, DiagnoseInput{
		APIVersion: "apps.example.org/v1", Kind: "XApp", Namespace: "team-a", Name: "app1",
	})
	if err != nil {
		t.Fatalf("diagnose: %v", err)
	}

	if out.Healthy {
		t.Error("a tree containing a Blocked resource must not be reported healthy")
	}
	if out.Stats.Nodes != 6 {
		t.Errorf("Stats.Nodes = %d, want 6 — diagnose should walk the same tree as get_resource_tree", out.Stats.Nodes)
	}
	if len(out.Suspects) == 0 {
		t.Fatal("expected at least one suspect")
	}
	// Blocked outranks Pending, then deepest first: queue1 (Blocked, depth 2)
	// beats the Blocked root and the two Pending error nodes.
	if out.Suspects[0].Name != "queue1" {
		t.Errorf("top suspect = %s (depth %d), want the deepest Blocked node queue1",
			out.Suspects[0].Name, out.Suspects[0].Depth)
	}
}

func names(nodes []xp.FlatNode) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Name
	}
	return out
}
