package xp

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/briferz/crossplane-mcp/internal/k8s"
)

// The traversal caps are the only thing standing between a pathological cluster
// graph and an unbounded walk, and neither ran in any test. These drive BuildTree
// against a real (faked) client so the caps are pinned where they actually fire.

// walkErrMapper rejects every RESTMapping, pushing Resolve onto the lenient
// discovery scan — the view the fake discovery populates.
type walkErrMapper struct{ meta.RESTMapper }

func (walkErrMapper) RESTMapping(gk schema.GroupKind, _ ...string) (*meta.RESTMapping, error) {
	return nil, errors.New("no mapping for " + gk.String())
}

// walkObj builds a namespaced object carrying an optional v1 spec.resourceRefs list.
func walkObj(t *testing.T, apiVersion, kind, ns, name string, refs ...map[string]any) *unstructured.Unstructured {
	t.Helper()
	o := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   map[string]any{"name": name},
	}}
	if ns != "" {
		_ = unstructured.SetNestedField(o.Object, ns, "metadata", "namespace")
	}
	if len(refs) > 0 {
		list := make([]any, len(refs))
		for i, r := range refs {
			list[i] = r
		}
		if err := unstructured.SetNestedSlice(o.Object, list, "spec", "resourceRefs"); err != nil {
			t.Fatalf("set refs on %s/%s: %v", kind, name, err)
		}
	}
	return o
}

func walkRef(apiVersion, kind, ns, name string) map[string]any {
	return map[string]any{"apiVersion": apiVersion, "kind": kind, "namespace": ns, "name": name}
}

// chainClient builds a linear chain chain0 -> chain1 -> ... -> chain{n-1}, one
// ref per object, so the walk descends exactly one level per hop.
func chainClient(t *testing.T, n int) (*k8s.Client, *unstructured.Unstructured) {
	t.Helper()
	const gv = "chain.example.org/v1"

	resources := []*metav1.APIResourceList{
		{GroupVersion: gv, APIResources: []metav1.APIResource{
			{Name: "chains", SingularName: "chain", Kind: "Chain", Namespaced: true},
		}},
	}
	disco := &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{Resources: resources}}

	objs := make([]runtime.Object, 0, n)
	var root *unstructured.Unstructured
	for i := range n {
		name := fmt.Sprintf("chain%d", i)
		var refs []map[string]any
		if i < n-1 {
			refs = append(refs, walkRef(gv, "Chain", "ns", fmt.Sprintf("chain%d", i+1)))
		}
		o := walkObj(t, gv, "Chain", "ns", name, refs...)
		if i == 0 {
			root = o
		}
		objs = append(objs, o)
	}

	gvrToListKind := map[schema.GroupVersionResource]string{
		{Group: "chain.example.org", Version: "v1", Resource: "chains"}: "ChainList",
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToListKind, objs...)
	return &k8s.Client{Dyn: dyn, Disco: disco, Mapper: walkErrMapper{}}, root
}

// manyChildrenClient builds one Parent fanning out to n childless Leaf objects,
// so the node cap trips inside the child loop rather than on depth.
func manyChildrenClient(t *testing.T, n int) (*k8s.Client, *unstructured.Unstructured) {
	t.Helper()
	const gv = "wide.example.org/v1"

	resources := []*metav1.APIResourceList{
		{GroupVersion: gv, APIResources: []metav1.APIResource{
			{Name: "parents", SingularName: "parent", Kind: "Parent", Namespaced: true},
			{Name: "leaves", SingularName: "leaf", Kind: "Leaf", Namespaced: true},
		}},
	}
	disco := &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{Resources: resources}}

	refs := make([]map[string]any, 0, n)
	objs := make([]runtime.Object, 0, n+1)
	for i := range n {
		name := fmt.Sprintf("leaf%d", i)
		refs = append(refs, walkRef(gv, "Leaf", "ns", name))
		objs = append(objs, walkObj(t, gv, "Leaf", "ns", name))
	}
	root := walkObj(t, gv, "Parent", "ns", "root", refs...)
	objs = append(objs, root)

	gvrToListKind := map[schema.GroupVersionResource]string{
		{Group: "wide.example.org", Version: "v1", Resource: "parents"}: "ParentList",
		{Group: "wide.example.org", Version: "v1", Resource: "leaves"}:  "LeafList",
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToListKind, objs...)
	return &k8s.Client{Dyn: dyn, Disco: disco, Mapper: walkErrMapper{}}, root
}

// TestBuildTreeMaxDepthCap pins the depth guard. The chain is deliberately two
// longer than maxDepth so a ref is genuinely skipped at the boundary: that keeps
// the assertion meaningful regardless of whether Capped is later tightened to
// mean "something was actually skipped" rather than "the limit was reached".
func TestBuildTreeMaxDepthCap(t *testing.T) {
	cl, root := chainClient(t, maxDepth+2)

	node, stats := BuildTree(context.Background(), cl, root)

	if stats.Nodes != maxDepth+1 {
		t.Errorf("Stats.Nodes = %d, want %d (depths 0..%d)", stats.Nodes, maxDepth+1, maxDepth)
	}
	if !stats.Capped {
		t.Error("a chain longer than maxDepth must report Capped")
	}
	flat := node.Flatten()
	if len(flat) != maxDepth+1 {
		t.Fatalf("flattened %d nodes, want %d", len(flat), maxDepth+1)
	}
	if deepest := flat[len(flat)-1]; deepest.Depth != maxDepth {
		t.Errorf("deepest node Depth = %d, want %d", deepest.Depth, maxDepth)
	}
}

// TestBuildTreeMaxNodesCap pins the node budget, which trips inside the child
// loop rather than on entry to build().
func TestBuildTreeMaxNodesCap(t *testing.T) {
	cl, root := manyChildrenClient(t, maxNodes+10)

	node, stats := BuildTree(context.Background(), cl, root)

	if stats.Nodes != maxNodes {
		t.Errorf("Stats.Nodes = %d, want the cap %d", stats.Nodes, maxNodes)
	}
	if !stats.Capped {
		t.Error("a fan-out wider than maxNodes must report Capped")
	}
	if len(node.Children) >= maxNodes+10 {
		t.Errorf("walked %d children, want fewer than the %d refs offered", len(node.Children), maxNodes+10)
	}
}
