package xp

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/briferz/crossplane-mcp/internal/k8s"
)

// Traversal safety limits to avoid runaway walks on large or cyclic graphs.
const (
	maxNodes = 200
	maxDepth = 20
)

// Node is one resource in the Crossplane tree.
type Node struct {
	APIVersion string      `json:"apiVersion"`
	Kind       string      `json:"kind"`
	Name       string      `json:"name"`
	Namespace  string      `json:"namespace,omitempty"`
	State      string      `json:"state"`
	Health     Health      `json:"health"`
	Conditions []Condition `json:"conditions,omitempty"`
	Children   []*Node     `json:"children,omitempty"`
	Error      string      `json:"error,omitempty"`

	// Internal, not serialised.
	uid   string
	depth int
}

// Stats reports traversal coverage.
type Stats struct {
	Nodes  int  `json:"nodes"`
	Capped bool `json:"capped,omitempty"`
}

// FlatNode is the non-recursive, token-light projection of a tree node used in
// tool output. Hierarchy is encoded via Parent (index into the slice, -1 for
// the root) and Depth. Full per-node conditions are intentionally omitted —
// fetch them with get_resource when needed.
type FlatNode struct {
	Depth      int    `json:"depth"`
	Parent     int    `json:"parent"`
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace,omitempty"`
	State      string `json:"state"`
	Health     Health `json:"health"`
	Error      string `json:"error,omitempty"`
}

// Flatten projects the tree into a depth-first slice of FlatNodes.
func (n *Node) Flatten() []FlatNode {
	var out []FlatNode
	var rec func(node *Node, parent int)
	rec = func(node *Node, parent int) {
		idx := len(out)
		out = append(out, FlatNode{
			Depth:      node.depth,
			Parent:     parent,
			APIVersion: node.APIVersion,
			Kind:       node.Kind,
			Name:       node.Name,
			Namespace:  node.Namespace,
			State:      node.State,
			Health:     node.Health,
			Error:      node.Error,
		})
		for _, c := range node.Children {
			rec(c, idx)
		}
	}
	if n != nil {
		rec(n, -1)
	}
	return out
}

type ref struct {
	apiVersion string
	kind       string
	name       string
	namespace  string
}

// BuildTree walks the composition tree starting from root, following
// spec.resourceRefs (composite → composed) and spec.resourceRef (claim → XR).
// Composed resources that are themselves composites recurse naturally.
func BuildTree(ctx context.Context, cl *k8s.Client, root *unstructured.Unstructured) (*Node, Stats) {
	st := &Stats{}
	visited := map[string]bool{}
	node := build(ctx, cl, root, 0, visited, st)
	return node, *st
}

func build(ctx context.Context, cl *k8s.Client, obj *unstructured.Unstructured, depth int, visited map[string]bool, st *Stats) *Node {
	conds := Conditions(obj)
	health, state := Classify(conds)
	n := &Node{
		APIVersion: obj.GetAPIVersion(),
		Kind:       obj.GetKind(),
		Name:       obj.GetName(),
		Namespace:  obj.GetNamespace(),
		State:      state,
		Health:     health,
		Conditions: conds,
		uid:        string(obj.GetUID()),
		depth:      depth,
	}
	st.Nodes++

	if depth >= maxDepth || st.Nodes >= maxNodes {
		st.Capped = true
		return n
	}

	for _, r := range childRefs(obj) {
		if r.name == "" || r.kind == "" {
			continue
		}
		key := r.apiVersion + "/" + r.kind + "/" + r.namespace + "/" + r.name
		if visited[key] {
			continue
		}
		visited[key] = true

		child := fetchChild(ctx, cl, r, obj.GetNamespace(), depth+1, visited, st)
		n.Children = append(n.Children, child)
	}
	return n
}

func fetchChild(ctx context.Context, cl *k8s.Client, r ref, parentNS string, depth int, visited map[string]bool, st *Stats) *Node {
	target, err := cl.Resolve(r.apiVersion, r.kind)
	if err != nil {
		st.Nodes++
		return &Node{APIVersion: r.apiVersion, Kind: r.kind, Name: r.name, Namespace: r.namespace, State: StatePending, Error: err.Error()}
	}

	ns := r.namespace
	if target.Namespaced && ns == "" {
		ns = parentNS // v2 composed MRs often omit namespace, inheriting the XR's
	}

	obj, err := cl.Get(ctx, target, ns, r.name)
	if err != nil {
		st.Nodes++
		return &Node{APIVersion: r.apiVersion, Kind: r.kind, Name: r.name, Namespace: ns, State: StatePending, Error: err.Error()}
	}
	return build(ctx, cl, obj, depth, visited, st)
}

// childRefs collects downward references: spec.resourceRefs (list, composite →
// composed) plus spec.resourceRef (single, claim → XR).
func childRefs(obj *unstructured.Unstructured) []ref {
	var refs []ref
	if list, found, _ := unstructured.NestedSlice(obj.Object, "spec", "resourceRefs"); found {
		for _, it := range list {
			if m, ok := it.(map[string]any); ok {
				refs = append(refs, refFromMap(m))
			}
		}
	}
	if m, found, _ := unstructured.NestedMap(obj.Object, "spec", "resourceRef"); found {
		refs = append(refs, refFromMap(m))
	}
	return refs
}

func refFromMap(m map[string]any) ref {
	return ref{
		apiVersion: str(m, "apiVersion"),
		kind:       str(m, "kind"),
		name:       str(m, "name"),
		namespace:  str(m, "namespace"),
	}
}
