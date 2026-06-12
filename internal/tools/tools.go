// Package tools registers the read-only Crossplane diagnostic tools on an MCP
// server. Inputs are intentionally small (kind/name/namespace) and outputs are
// pruned, token-light JSON suited to LLM consumption.
package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/briferz/crossplane-mcp/internal/k8s"
	"github.com/briferz/crossplane-mcp/internal/xp"
)

// readOnly builds the tool annotations shared by every tool this server
// registers: all of them are read-only (the project's core promise) and their
// domain is the configured cluster, not an open world. Declaring it at the
// protocol level lets MCP clients treat the calls as safe — e.g. skip
// mutation-style confirmation prompts.
func readOnly(title string) *mcp.ToolAnnotations {
	closedWorld := false
	return &mcp.ToolAnnotations{
		Title:         title,
		ReadOnlyHint:  true,
		OpenWorldHint: &closedWorld,
	}
}

// Register adds every diagnostic tool to the server. If rec is non-nil, each
// tool call's input/output is appended to it for later inspection.
func Register(s *mcp.Server, cl *k8s.Client, rec *Recorder) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "diagnose",
		Annotations: readOnly("Diagnose a stuck Crossplane resource"),
		Description: "Diagnose a stuck Crossplane resource. Walks the composite (XR) → managed " +
			"resource tree from the given resource, finds resources with a failing Ready/Synced/" +
			"Healthy condition, and ranks them so the deepest (most likely root cause) comes first " +
			"with full, untruncated condition messages and recent events. Suspects also carry a " +
			"lifecycle label (Terminating (stuck Nd) / Creating (blocked, Nd) / Paused), decoded " +
			"provider-terraform/OpenTofu error blobs (decodedErrors), a paused flag " +
			"(crossplane.io/paused), and — while terminating — the finalizers holding deletion. " +
			"Use this first when something is not becoming Ready.",
	}, recorded(rec, "diagnose", diagnoseHandler(cl)))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_resource_tree",
		Annotations: readOnly("Get the Crossplane composition tree"),
		Description: "Return the Crossplane composition tree (Claim/XR → composed/managed resources) " +
			"rooted at the given resource, with each node's Ready/Synced/Healthy state. Structured " +
			"equivalent of `crossplane resource trace`, as JSON.",
	}, recorded(rec, "get_resource_tree", treeHandler(cl)))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_resource",
		Annotations: readOnly("Get one resource, pruned"),
		Description: "Fetch a single Kubernetes/Crossplane resource, pruned to its status conditions, " +
			"recent events, and spec (noisy metadata like managedFields removed). Also surfaces " +
			"paused (crossplane.io/paused) and, while the resource is terminating, its " +
			"deletionTimestamp + finalizers.",
	}, recorded(rec, "get_resource", getResourceHandler(cl)))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_unhealthy",
		Annotations: readOnly("List unhealthy Crossplane resources"),
		Description: "Triage the cluster: find broken Crossplane resources without knowing their names. " +
			"Lists composite resources (XRs) and claims and returns only those whose Ready/Synced condition " +
			"is not True (by default), each as a tiny row {apiVersion, kind, name, namespace, category, " +
			"state, ready, synced} (plus paused when crossplane.io/paused is set — a paused resource never " +
			"reconciles) ready to pass straight to diagnose. Use this FIRST to answer \"what is " +
			"failing?\", then feed an item into diagnose for the root-cause tree, or get_resource for one " +
			"resource's detail. Output is flat, capped, and ordered most-actionable first (Blocked before " +
			"Pending), with no condition messages/events. Omitting namespace scans cluster-wide (needs " +
			"cluster read); set namespace to scope to one namespace (the RBAC-safe path; cluster-scoped v1 " +
			"XRs are then skipped). Forbidden API groups/namespaces are reported in notes, never failing the call.",
	}, recorded(rec, "list_unhealthy", listUnhealthyHandler(cl)))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_providers",
		Annotations: readOnly("List Crossplane Providers and their health"),
		Description: "Check whether a Crossplane Provider package is the real problem. Use when diagnose's " +
			"deepest suspect is a managed resource stuck with a cryptic Synced/Ready message, or when all " +
			"MRs of one provider fail together: a crashlooping provider pod, a failed image pull, an " +
			"incompatible Crossplane version, or an un-approved upgrade (revisionActivationPolicy: Manual) " +
			"is invisible from the MR itself. Lists every Provider (cluster-scoped; no namespace) with " +
			"installed/healthy status; non-Ready ones add full untruncated condition messages, recent " +
			"events (e.g. the UnpackPackage registry error), per-revision health rows, and upgrade-skew " +
			"notes. A failing revision's name is by default also its runtime Deployment's name in the " +
			"Crossplane install namespace (a DeploymentRuntimeConfig can override it) — pivot there " +
			"(outside this server) for pod-level crash detail. A paused package " +
			"(crossplane.io/paused) never reconciles, including deletion.",
	}, recorded(rec, "list_providers", listPackagesHandler(cl, "Provider", "ProviderRevision")))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_functions",
		Annotations: readOnly("List Crossplane composition Functions and their health"),
		Description: "Check whether a Crossplane composition Function package is the real problem. Use when " +
			"a composition pipeline step fails or an XR reports a function/gRPC error: a crashlooping " +
			"function pod or unhealthy FunctionRevision is invisible from the XR. Same row shape and " +
			"skew/event handling as list_providers (cluster-scoped; no namespace). Functions require " +
			"Crossplane >= 1.14; on older clusters the notes say so instead of returning an empty list.",
	}, recorded(rec, "list_functions", listPackagesHandler(cl, "Function", "FunctionRevision")))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_configurations",
		Annotations: readOnly("List Crossplane Configurations and their health"),
		Description: "Check whether a Crossplane Configuration package is the real problem. Use when " +
			"Compositions or XRDs an XR needs are missing or outdated: the Configuration that ships them " +
			"may be failing to install, unpack, or resolve its dependencies. Same row shape as " +
			"list_providers (cluster-scoped; no namespace); Configurations run no pods, so rows never " +
			"carry runtime health.",
	}, recorded(rec, "list_configurations", listPackagesHandler(cl, "Configuration", "ConfigurationRevision")))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_contexts",
		Annotations: readOnly("List kubeconfig contexts"),
		Description: "List the available kubeconfig contexts (empty when running in-cluster).",
	}, recorded(rec, "list_contexts", contextsHandler(cl)))
}

// --- list_unhealthy ---

type ListUnhealthyInput struct {
	Namespace      string `json:"namespace,omitempty" jsonschema:"limit the scan to one namespace (namespaced XRs/claims only); omit to scan cluster-wide where RBAC allows. Cluster-scoped v1 XRs are skipped when this is set"`
	Category       string `json:"category,omitempty" jsonschema:"which Crossplane discovery category to scan: composite (XRs), claim (v1 claims), or managed (provider managed resources). Omit to scan composite and claim"`
	Kind           string `json:"kind,omitempty" jsonschema:"only return resources of this kind (case-insensitive), e.g. XPostgreSQLInstance or a Claim kind"`
	IncludeHealthy bool   `json:"includeHealthy,omitempty" jsonschema:"also include Ready resources; default false returns only not-Ready/not-Synced ones"`
	Limit          int    `json:"limit,omitempty" jsonschema:"max items to return (default 100, hard cap 500); truncated is true in the output when more matched"`
}

type ListUnhealthyOutput struct {
	Items     []xp.UnhealthyItem  `json:"items,omitempty"`
	Summary   xp.UnhealthySummary `json:"summary"`
	Scanned   int                 `json:"scanned"`
	Truncated bool                `json:"truncated,omitempty"`
	Notes     []string            `json:"notes,omitempty"`
}

const (
	defaultListLimit = 100
	maxListLimit     = 500
)

func listUnhealthyHandler(cl *k8s.Client) mcp.ToolHandlerFor[ListUnhealthyInput, *ListUnhealthyOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in ListUnhealthyInput) (*mcp.CallToolResult, *ListUnhealthyOutput, error) {
		cats := []string{k8s.CategoryComposite, k8s.CategoryClaim}
		if c := strings.ToLower(strings.TrimSpace(in.Category)); c != "" {
			if c != k8s.CategoryComposite && c != k8s.CategoryClaim && c != k8s.CategoryManaged {
				return nil, nil, fmt.Errorf("unknown category %q; want composite, claim, or managed", c)
			}
			cats = []string{c}
		}
		kinds, notes, err := cl.DiscoverComposite(cats...)
		if err != nil {
			return nil, nil, err
		}
		if len(kinds) == 0 {
			notes = append(notes, "no resource types found for categories "+strings.Join(cats, ", ")+
				" (is Crossplane installed, and do you have discovery access?)")
		}

		res := cl.ListAll(ctx, kinds, strings.TrimSpace(in.Namespace))
		built := xp.BuildUnhealthy(res.Objects, xp.UnhealthyParams{
			Kind:           in.Kind,
			IncludeHealthy: in.IncludeHealthy,
			Limit:          clampLimit(in.Limit),
		})
		return nil, &ListUnhealthyOutput{
			Items:     built.Items,
			Summary:   built.Summary,
			Scanned:   built.Scanned,
			Truncated: built.Truncated,
			Notes:     append(notes, res.Notes...),
		}, nil
	}
}

func clampLimit(n int) int {
	switch {
	case n <= 0:
		return defaultListLimit
	case n > maxListLimit:
		return maxListLimit
	default:
		return n
	}
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
	APIVersion string    `json:"apiVersion"`
	Kind       string    `json:"kind"`
	Name       string    `json:"name"`
	Namespace  string    `json:"namespace,omitempty"`
	State      string    `json:"state"`
	Health     xp.Health `json:"health"`
	// Paused is true when the resource carries crossplane.io/paused="true":
	// reconciliation is suspended and the conditions below may be stale.
	Paused bool `json:"paused,omitempty"`
	// DeletionTimestamp and Finalizers are set only while the resource is being
	// deleted, so a wedged teardown names what is still holding it.
	DeletionTimestamp string         `json:"deletionTimestamp,omitempty"`
	Finalizers        []string       `json:"finalizers,omitempty"`
	Conditions        []xp.Condition `json:"conditions,omitempty"`
	Events            []k8s.Event    `json:"events,omitempty"`
	Spec              map[string]any `json:"spec,omitempty"`
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
			Paused:     xp.IsPaused(obj),
			Conditions: conds,
			Spec:       spec,
		}
		if dt := obj.GetDeletionTimestamp(); dt != nil && !dt.IsZero() {
			view.DeletionTimestamp = dt.UTC().Format(time.RFC3339)
			view.Finalizers = obj.GetFinalizers()
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
