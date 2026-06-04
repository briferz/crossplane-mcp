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
)

// Crossplane stamps XRD-generated CRDs with these Kubernetes discovery
// categories — the mechanism behind `kubectl get composite/claim/managed`. We
// discover what to triage by category rather than by walking XRDs, so we see
// exactly what the API server currently serves under the caller's RBAC.
const (
	CategoryComposite = "composite" // composite resources (XRs): v1 cluster-scoped and v2 namespaced
	CategoryClaim     = "claim"     // v1 claims (namespaced)
	CategoryManaged   = "managed"   // provider managed resources
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
// Like resolveByKind, partial discovery (an unavailable aggregated API group)
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
func (c *Client) ListAll(ctx context.Context, kinds []CompositeKind, namespace string) ListResult {
	var res ListResult
	for _, k := range kinds {
		ri := c.Dyn.Resource(k.GVR)
		var (
			list *unstructured.UnstructuredList
			err  error
		)
		switch {
		case k.Namespaced && namespace != "":
			list, err = ri.Namespace(namespace).List(ctx, metav1.ListOptions{})
		case !k.Namespaced && namespace != "":
			res.Notes = append(res.Notes, fmt.Sprintf("skipped cluster-scoped %s: namespace filter set", k.Kind))
			continue
		default:
			// Namespaced with no filter (all namespaces) or cluster-scoped.
			list, err = ri.List(ctx, metav1.ListOptions{})
		}
		if err != nil {
			res.Notes = append(res.Notes, listSkipNote(k, namespace, err))
			continue
		}
		for i := range list.Items {
			res.Objects = append(res.Objects, Listed{Category: k.Category, Object: list.Items[i]})
		}
	}
	return res
}

func listSkipNote(k CompositeKind, namespace string, err error) string {
	gr := k.GVR.GroupResource().String()
	switch {
	case apierrors.IsForbidden(err):
		loc := ""
		if namespace != "" {
			loc = " in " + namespace
		}
		return fmt.Sprintf("skipped %s%s: forbidden (RBAC); re-call with an explicit namespace if scanning cluster-wide", gr, loc)
	case apierrors.IsNotFound(err):
		return fmt.Sprintf("skipped %s: not found (CRD removed between discover and list?)", gr)
	default:
		return fmt.Sprintf("skipped %s: %v", gr, err)
	}
}
