// Package tools registers the read-only Crossplane diagnostic tools on an MCP
// server. Inputs are intentionally small (kind/name/namespace) and outputs are
// pruned, token-light JSON suited to LLM consumption.
package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/briferz/crossplane-mcp/internal/k8s"
	"github.com/briferz/crossplane-mcp/internal/xp"
)

// Register adds every diagnostic tool to the server.
func Register(s *mcp.Server, cl *k8s.Client) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "diagnose",
		Description: "Diagnose a stuck Crossplane resource. Walks the composite (XR) → managed " +
			"resource tree from the given resource, finds resources with a failing Ready/Synced/" +
			"Healthy condition, and ranks them so the deepest (most likely root cause) comes first " +
			"with full, untruncated condition messages and recent events. Use this first when " +
			"something is not becoming Ready.",
	}, diagnoseHandler(cl))

	mcp.AddTool(s, &mcp.Tool{
		Name: "get_resource_tree",
		Description: "Return the Crossplane composition tree (Claim/XR → composed/managed resources) " +
			"rooted at the given resource, with each node's Ready/Synced/Healthy state. Structured " +
			"equivalent of `crossplane resource trace`, as JSON.",
	}, treeHandler(cl))

	mcp.AddTool(s, &mcp.Tool{
		Name: "get_resource",
		Description: "Fetch a single Kubernetes/Crossplane resource, pruned to its status conditions, " +
			"recent events, and spec (noisy fields like managedFields removed).",
	}, getResourceHandler(cl))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_contexts",
		Description: "List the available kubeconfig contexts (empty when running in-cluster).",
	}, contextsHandler(cl))
}

// --- diagnose ---

type DiagnoseInput struct {
	Kind        string `json:"kind" jsonschema:"resource kind, e.g. XPostgreSQLInstance, Bucket, or a Claim kind"`
	Name        string `json:"name" jsonschema:"resource name"`
	APIVersion  string `json:"apiVersion,omitempty" jsonschema:"optional apiVersion (group/version) to disambiguate the kind"`
	Namespace   string `json:"namespace,omitempty" jsonschema:"namespace; required for namespaced kinds, omit for cluster-scoped ones"`
	IncludeTree bool   `json:"includeTree,omitempty" jsonschema:"also return the full annotated tree, not just the suspects"`
}

func diagnoseHandler(cl *k8s.Client) mcp.ToolHandlerFor[DiagnoseInput, *xp.Diagnosis] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in DiagnoseInput) (*mcp.CallToolResult, *xp.Diagnosis, error) {
		obj, err := fetch(ctx, cl, in.APIVersion, in.Kind, in.Namespace, in.Name)
		if err != nil {
			return nil, nil, err
		}
		tree, stats := xp.BuildTree(ctx, cl, obj)
		return nil, xp.Diagnose(ctx, cl, tree, stats, in.IncludeTree), nil
	}
}

// --- get_resource_tree ---

type TreeInput struct {
	Kind       string `json:"kind" jsonschema:"resource kind to root the tree at"`
	Name       string `json:"name" jsonschema:"resource name"`
	APIVersion string `json:"apiVersion,omitempty" jsonschema:"optional apiVersion (group/version) to disambiguate the kind"`
	Namespace  string `json:"namespace,omitempty" jsonschema:"namespace; required for namespaced kinds, omit for cluster-scoped ones"`
}

type TreeOutput struct {
	Nodes []xp.FlatNode `json:"nodes"`
	Stats xp.Stats      `json:"stats"`
}

func treeHandler(cl *k8s.Client) mcp.ToolHandlerFor[TreeInput, *TreeOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in TreeInput) (*mcp.CallToolResult, *TreeOutput, error) {
		obj, err := fetch(ctx, cl, in.APIVersion, in.Kind, in.Namespace, in.Name)
		if err != nil {
			return nil, nil, err
		}
		tree, stats := xp.BuildTree(ctx, cl, obj)
		return nil, &TreeOutput{Nodes: tree.Flatten(), Stats: stats}, nil
	}
}

// --- get_resource ---

type GetResourceInput struct {
	Kind       string `json:"kind" jsonschema:"resource kind"`
	Name       string `json:"name" jsonschema:"resource name"`
	APIVersion string `json:"apiVersion,omitempty" jsonschema:"optional apiVersion (group/version) to disambiguate the kind"`
	Namespace  string `json:"namespace,omitempty" jsonschema:"namespace; required for namespaced kinds, omit for cluster-scoped ones"`
}

type ResourceView struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Name       string         `json:"name"`
	Namespace  string         `json:"namespace,omitempty"`
	State      string         `json:"state"`
	Health     xp.Health      `json:"health"`
	Conditions []xp.Condition `json:"conditions,omitempty"`
	Events     []k8s.Event    `json:"events,omitempty"`
	Spec       map[string]any `json:"spec,omitempty"`
}

func getResourceHandler(cl *k8s.Client) mcp.ToolHandlerFor[GetResourceInput, *ResourceView] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in GetResourceInput) (*mcp.CallToolResult, *ResourceView, error) {
		obj, err := fetch(ctx, cl, in.APIVersion, in.Kind, in.Namespace, in.Name)
		if err != nil {
			return nil, nil, err
		}
		conds := xp.Conditions(obj)
		health, state := xp.Classify(conds)
		spec, _, _ := unstructured.NestedMap(obj.Object, "spec")

		view := &ResourceView{
			APIVersion: obj.GetAPIVersion(),
			Kind:       obj.GetKind(),
			Name:       obj.GetName(),
			Namespace:  obj.GetNamespace(),
			State:      state,
			Health:     health,
			Conditions: conds,
			Spec:       spec,
		}
		if ev, err := cl.Events(ctx, obj.GetNamespace(), string(obj.GetUID()), 10); err == nil {
			view.Events = ev
		}
		return nil, view, nil
	}
}

// --- list_contexts ---

type ContextsInput struct{}

type ContextsOutput struct {
	Contexts  []k8s.ContextInfo `json:"contexts"`
	InCluster bool              `json:"inCluster,omitempty"`
}

func contextsHandler(cl *k8s.Client) mcp.ToolHandlerFor[ContextsInput, *ContextsOutput] {
	return func(_ context.Context, _ *mcp.CallToolRequest, _ ContextsInput) (*mcp.CallToolResult, *ContextsOutput, error) {
		ctxs, err := cl.Contexts()
		if err != nil {
			return nil, nil, err
		}
		return nil, &ContextsOutput{Contexts: ctxs, InCluster: ctxs == nil}, nil
	}
}

// fetch resolves a kind to its resource type and gets the object.
func fetch(ctx context.Context, cl *k8s.Client, apiVersion, kind, namespace, name string) (*unstructured.Unstructured, error) {
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	target, err := cl.Resolve(apiVersion, kind)
	if err != nil {
		return nil, err
	}
	return cl.Get(ctx, target, namespace, name)
}
