package k8s

import (
	"context"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// listChunkSize bounds each List request, matching kubectl's default
// --chunk-size. A single unbounded List of a large resource type can spike the
// API server / etcd; chunking with a Continue token keeps the cluster (which we
// may be pointed at in production) safe.
//
// This caps per-request size only — every page still accumulates in memory
// before any display cap applies, and the category=managed scope removes any
// practical bound on how many objects that is. ProjectTriageFields is what
// keeps the accumulation small; see its comment.
const listChunkSize = 500

// Crossplane stamps XRD-generated CRDs with these Kubernetes discovery
// categories — the mechanism behind `kubectl get composite/claim/managed`. We
// discover what to triage by category rather than by walking XRDs, so we see
// exactly what the API server currently serves under the caller's RBAC.
const (
	CategoryComposite = "composite" // composite resources (XRs): v1 cluster-scoped and v2 namespaced
	CategoryClaim     = "claim"     // v1 claims (namespaced)
	CategoryManaged   = "managed"   // provider managed resources
	// The package manager's CRDs are stamped the same way (stable since
	// Crossplane v1.11) — the mechanism behind `kubectl get pkg` / `pkgrev`.
	// All package kinds are cluster-scoped on every Crossplane release.
	CategoryPackage         = "pkg"    // Provider / Function / Configuration
	CategoryPackageRevision = "pkgrev" // ProviderRevision / FunctionRevision / ConfigurationRevision
)

// CompositeKind is a discovered resource type plus the Crossplane category it
// was matched under.
type CompositeKind struct {
	Target
	Category string
}

// Listed is one fetched object tagged with the category of the type it came
// from, so callers need not re-derive it.
type Listed struct {
	Category string
	Object   unstructured.Unstructured
}

// ListResult is the outcome of ListAll: the objects read, plus human-readable
// notes about anything skipped (forbidden, not found, or cluster-scoped under a
// namespace filter). A per-type failure never fails the whole call.
type ListResult struct {
	Objects []Listed
	Notes   []string
}

// DiscoverComposite returns the resource types Crossplane stamps with the given
// categories (default composite+claim). The Namespaced flag comes straight from
// discovery, which is how v1 cluster-scoped XRs, v2 namespaced XRs, and v1
// namespaced claims are distinguished with no version-specific branching.
//
// It reads ServerGroupsAndResources (not ServerPreferredResources): the former
// returns every served version with categories intact, so no resource is
// hidden, and it is the path the client-go discovery fake actually populates.
// Like scanForKind, partial discovery (an unavailable aggregated API group)
// is tolerated — it degrades to a note unless discovery returned nothing.
//
// Read-only: issues only discovery GET requests.
func (c *Client) DiscoverComposite(cats ...string) ([]CompositeKind, []string, error) {
	if len(cats) == 0 {
		cats = []string{CategoryComposite, CategoryClaim}
	}
	_, lists, err := c.Disco.ServerGroupsAndResources()
	if len(lists) == 0 && err != nil {
		return nil, nil, fmt.Errorf("discover resources: %w", err)
	}
	var notes []string
	if err != nil {
		notes = append(notes, "partial discovery: "+err.Error())
	}

	var out []CompositeKind
	seen := map[schema.GroupResource]bool{}
	for _, list := range lists {
		if list == nil {
			continue
		}
		gv, perr := schema.ParseGroupVersion(list.GroupVersion)
		if perr != nil {
			continue
		}
		for _, r := range list.APIResources {
			// Skip subresources (status, scale, …): they appear as their own
			// APIResource and inherit the parent's categories, so without this
			// guard we would try to List "buckets/status" and double-count.
			if strings.Contains(r.Name, "/") {
				continue
			}
			cat := matchCategory(r.Categories, cats)
			if cat == "" {
				continue
			}
			// ServerGroupsAndResources returns every served version; collapse to
			// one Target per group+resource.
			gr := schema.GroupResource{Group: gv.Group, Resource: r.Name}
			if seen[gr] {
				continue
			}
			seen[gr] = true
			out = append(out, CompositeKind{
				Target:   Target{GVR: gv.WithResource(r.Name), Namespaced: r.Namespaced, Kind: r.Kind},
				Category: cat,
			})
		}
	}
	return out, notes, nil
}

// matchCategory returns the first requested category present on the resource
// (in the precedence order of want), or "" if none match.
func matchCategory(have, want []string) string {
	for _, w := range want {
		if slices.Contains(have, w) {
			return w
		}
	}
	return ""
}

// ListAll lists every kind with per-type partial-failure tolerance. A namespaced
// kind is listed within namespace (or across all namespaces when namespace is
// empty); a cluster-scoped kind is listed cluster-wide, and skipped with a note
// when a namespace filter is set (a namespace cannot scope something that has
// none). A forbidden or not-found type is recorded in Notes and skipped — a
// single type's error is never returned as the call's error, so a least-
// privilege role still gets whatever it can read.
//
// Read-only: issues only dynamic List requests.
// ProjectTriageFields trims a listed object down to the fields triage reads,
// replacing its map rather than deleting keys so the discarded bulk is actually
// released. Listing cluster-wide retains every object before any cap applies, and
// the bulk of a managed resource is managedFields plus
// kubectl.kubernetes.io/last-applied-configuration — none of which triage looks
// at. Without this, list_unhealthy{category:"managed"} on a large estate holds
// the whole result set in memory at once.
//
// COUPLING, deliberate and narrow: this is the read-set of xp.BuildUnhealthy.
// It cannot live in xp (that package imports this one), so parity is pinned by
// TestBuildUnhealthyProjectionParity, which runs BuildUnhealthy over projected
// and unprojected copies of the same fixtures and requires identical results.
// If you add a field to BuildUnhealthy's reads, that test fails here.
func ProjectTriageFields(o *unstructured.Unstructured) {
	src := o.Object
	dst := make(map[string]any, 4)
	for _, k := range []string{"apiVersion", "kind"} {
		if v, ok := src[k]; ok {
			dst[k] = v
		}
	}
	if md, ok := src["metadata"].(map[string]any); ok {
		m := make(map[string]any, 4)
		for _, k := range []string{"name", "namespace", "deletionTimestamp"} {
			if v, ok := md[k]; ok {
				m[k] = v
			}
		}
		// Only the pause annotation, never the whole map: last-applied-
		// configuration alone can rival the object it annotates.
		if ann, ok := md["annotations"].(map[string]any); ok {
			if v, ok := ann[PausedAnnotation]; ok {
				m["annotations"] = map[string]any{PausedAnnotation: v}
			}
		}
		dst["metadata"] = m
	}
	if st, ok := src["status"].(map[string]any); ok {
		if c, ok := st["conditions"]; ok {
			dst["status"] = map[string]any{"conditions": c}
		}
	}
	o.Object = dst
}

// PausedAnnotation suspends reconciliation when set to "true". Mirrored here
// because ProjectTriageFields must retain it; xp.IsPaused is the reader.
const PausedAnnotation = "crossplane.io/paused"

// ListAll pages every kind into memory. project, when non-nil, trims each object
// as it arrives — see ProjectTriageFields. Paging deliberately runs to
// completion rather than stopping at the caller's display limit: the pre-cap
// Scanned/Summary totals and the global Blocked-before-Pending ordering both
// require seeing everything.
func (c *Client) ListAll(ctx context.Context, kinds []CompositeKind, namespace string, project func(*unstructured.Unstructured)) ListResult {
	var res ListResult
	for _, k := range kinds {
		// Stop promptly if the caller's context is done (client gone / timeout)
		// instead of firing a List for every remaining type.
		if err := ctx.Err(); err != nil {
			res.Notes = append(res.Notes, fmt.Sprintf("listing aborted: %v", err))
			return res
		}
		// A namespace filter cannot scope a cluster-scoped resource.
		if !k.Namespaced && namespace != "" {
			res.Notes = append(res.Notes, fmt.Sprintf("skipped cluster-scoped %s: namespace filter set", k.Kind))
			continue
		}
		var lister dynamic.ResourceInterface = c.Dyn.Resource(k.GVR)
		if k.Namespaced && namespace != "" {
			lister = c.Dyn.Resource(k.GVR).Namespace(namespace)
		}
		// Page through with a Continue token so one huge resource type can't be
		// fetched in a single unbounded List against the API server.
		cont := ""
		for {
			list, err := lister.List(ctx, metav1.ListOptions{Limit: listChunkSize, Continue: cont})
			if err != nil {
				res.Notes = append(res.Notes, listSkipNote(k, namespace, err))
				if ctx.Err() != nil {
					return res // cancelled mid-list: stop, don't try the rest
				}
				break
			}
			for i := range list.Items {
				o := list.Items[i]
				if project != nil {
					project(&o)
				}
				res.Objects = append(res.Objects, Listed{Category: k.Category, Object: o})
			}
			if cont = list.GetContinue(); cont == "" {
				break
			}
		}
	}
	return res
}

func listSkipNote(k CompositeKind, namespace string, err error) string {
	gr := k.GVR.GroupResource().String()
	switch {
	case apierrors.IsForbidden(err):
		if namespace != "" {
			// A namespace was already given; suggesting one would be contradictory.
			return fmt.Sprintf("skipped %s in %s: forbidden (RBAC)", gr, namespace)
		}
		if !k.Namespaced {
			// A namespace cannot scope a cluster-scoped kind (packages, v1 XRs),
			// so suggesting one would be unactionable advice.
			return fmt.Sprintf("skipped %s: forbidden (RBAC)", gr)
		}
		return fmt.Sprintf("skipped %s: forbidden (RBAC); re-call with an explicit namespace to scope within your access", gr)
	case apierrors.IsNotFound(err):
		return fmt.Sprintf("skipped %s: not found (CRD removed between discover and list?)", gr)
	default:
		return fmt.Sprintf("skipped %s: %v", gr, err)
	}
}
