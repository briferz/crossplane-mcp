package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/briferz/crossplane-mcp/internal/k8s"
	"github.com/briferz/crossplane-mcp/internal/xp"
)

// packageGroup is the API group of Crossplane's package manager. Discovery is
// category-driven (pkg/pkgrev, so the served version — Function v1beta1 on
// Crossplane 1.14–1.16, v1 from 1.17 — is never hardcoded), but the group is
// also pinned: a third-party CRD that happens to reuse the same category names
// must never leak into the package tools.
const packageGroup = "pkg.crossplane.io"

type ListPackagesInput struct {
	Name          string `json:"name,omitempty" jsonschema:"case-insensitive substring matched against the package object name AND its OCI image ref (spec.package) — e.g. 'aws-s3' matches both provider-aws-s3 and xpkg.upbound.io/upbound/provider-aws-s3:v1; omit to list all. Filtered-out packages are excluded from scanned/summary"`
	UnhealthyOnly bool   `json:"unhealthyOnly,omitempty" jsonschema:"return only packages whose state is Blocked or Pending; default false returns every package — package counts are small, and seeing your suspect listed as Ready is itself the answer (look elsewhere)"`
	Limit         int    `json:"limit,omitempty" jsonschema:"max items to return (default 100, hard cap 500); truncated is true in the output when more matched. Revision rows per package are separately capped at 5 (revisionsTruncated), and in a mass failure only the first 10 failing packages carry full detail (reasons/skew/revisions/events) — further failing rows are compact, with a note; use name to drill into one"`
}

type ListPackagesOutput struct {
	Items     []xp.PackageRow     `json:"items,omitempty"`
	Summary   xp.UnhealthySummary `json:"summary"` // pre-cap package counts (revisions are attachments, never counted)
	Scanned   int                 `json:"scanned"`
	Truncated bool                `json:"truncated,omitempty"`
	Notes     []string            `json:"notes,omitempty"`
}

// listPackagesHandler builds the shared handler behind list_providers,
// list_functions, and list_configurations — the pkg.crossplane.io API is
// uniform across the three kinds, so only the kind pair differs.
func listPackagesHandler(cl *k8s.Client, pkgKind, revKind string) mcp.ToolHandlerFor[ListPackagesInput, *ListPackagesOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in ListPackagesInput) (*mcp.CallToolResult, *ListPackagesOutput, error) {
		kinds, notes, err := cl.DiscoverComposite(k8s.CategoryPackage, k8s.CategoryPackageRevision)
		if err != nil {
			return nil, nil, err
		}
		var pkgKinds, revKinds []k8s.CompositeKind
		for _, k := range kinds {
			if k.GVR.Group != packageGroup {
				continue
			}
			switch {
			case k.Kind == pkgKind && k.Category == k8s.CategoryPackage:
				pkgKinds = append(pkgKinds, k)
			case k.Kind == revKind && k.Category == k8s.CategoryPackageRevision:
				revKinds = append(revKinds, k)
			}
		}
		if len(pkgKinds) == 0 {
			notes = append(notes, missingPackageKindNote(pkgKind))
		}

		// Packages and revisions are listed separately so an RBAC denial on one
		// is distinguishable from the other: package rows still ship when only
		// revisions are forbidden, with revision-derived fields suppressed.
		pkgRes := cl.ListAll(ctx, pkgKinds, "")
		revRes := cl.ListAll(ctx, revKinds, "")
		notes = append(notes, pkgRes.Notes...)
		notes = append(notes, revRes.Notes...)

		built := xp.BuildPackages(pkgRes.Objects, revRes.Objects, xp.PackagesParams{
			Name:            in.Name,
			UnhealthyOnly:   in.UnhealthyOnly,
			Limit:           clampLimit(in.Limit),
			RevisionsListed: len(revKinds) > 0 && revisionsListed(revRes.Notes, revKinds),
		})
		notes = append(notes, built.Notes...)
		notes = append(notes, xp.DecoratePackageEvents(ctx, cl, built.Items)...)

		return nil, &ListPackagesOutput{
			Items:     built.Items,
			Summary:   built.Summary,
			Scanned:   built.Scanned,
			Truncated: built.Truncated,
			Notes:     notes,
		}, nil
	}
}

// revisionsListed reports whether the revision list actually succeeded: a skip
// note naming the revision GroupResource means RBAC/availability denied it,
// and an aborted listing (context cancellation) means we cannot know — either
// way revision-derived output must be suppressed rather than misread as "no
// revisions exist".
func revisionsListed(notes []string, revKinds []k8s.CompositeKind) bool {
	for _, n := range notes {
		if strings.Contains(n, "listing aborted") {
			return false
		}
		for _, k := range revKinds {
			if strings.Contains(n, k.GVR.GroupResource().String()) {
				return false
			}
		}
	}
	return true
}

// missingPackageKindNote explains an absent package kind honestly. Functions
// get a version-aware note: they simply do not exist before Crossplane 1.14.
func missingPackageKindNote(kind string) string {
	if kind == "Function" {
		return "no Function package types are served by this API server — composition Functions require Crossplane >= 1.14 " +
			"(pkg.crossplane.io/v1beta1 on 1.14-1.16, v1 from 1.17) — or discovery RBAC denies them"
	}
	return fmt.Sprintf("no %s package types found (is Crossplane installed, and do you have discovery access?)", kind)
}
