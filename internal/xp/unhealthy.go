package xp

import (
	"sort"
	"strings"

	"github.com/briferz/crossplane-mcp/internal/k8s"
)

// UnhealthyItem is a single triaged resource — a tiny row meant to be fed
// straight into diagnose. It deliberately carries no conditions, events, or
// spec (fetch those with diagnose/get_resource); only condition statuses, never
// messages, so nothing sensitive leaks.
type UnhealthyItem struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace,omitempty"`
	Category   string `json:"category,omitempty"`
	State      string `json:"state"`
	Ready      string `json:"ready,omitempty"`
	Synced     string `json:"synced,omitempty"`
	// Paused is true when the resource carries crossplane.io/paused="true" —
	// surfaced at triage time because a paused resource never reconciles, so
	// its row's conditions may be stale and diagnose will route differently.
	Paused bool `json:"paused,omitempty"`
	// DeletionTimestamp is set (RFC3339) while the resource is being deleted. A
	// row can carry it while still reporting Ready: a dead or blocked reconciler
	// leaves conditions frozen at their last value. Such rows are kept even in
	// the default not-Ready-only view, because a teardown wedged for months is
	// exactly what triage must not hide. Same reasoning as Paused.
	DeletionTimestamp string `json:"deletionTimestamp,omitempty"`
}

// UnhealthySummary is the pre-cap tally across everything scanned, so the counts
// stay honest even when Items is truncated.
type UnhealthySummary struct {
	Blocked int `json:"blocked"`
	Pending int `json:"pending"`
	Ready   int `json:"ready"`
	// Terminating counts resources with a deletionTimestamp whatever their
	// conditions say, mirroring diagnose's headline counts. Disjoint from the
	// other three — the four still sum to Scanned — and omitted entirely when
	// nothing is deleting, so existing output stays byte-identical.
	Terminating int `json:"terminating,omitempty"`
}

// UnhealthyResult is the classified, filtered, sorted, capped triage output.
type UnhealthyResult struct {
	Items     []UnhealthyItem  `json:"items,omitempty"`
	Summary   UnhealthySummary `json:"summary"`
	Scanned   int              `json:"scanned"`
	Truncated bool             `json:"truncated,omitempty"`
}

// UnhealthyParams tunes BuildUnhealthy.
type UnhealthyParams struct {
	Kind           string // case-insensitive kind filter; "" matches any
	IncludeHealthy bool   // include Ready resources too (default: only not-Ready)
	Limit          int    // max Items; <=0 means no cap
}

// BuildUnhealthy classifies each listed object via the same logic diagnose uses,
// keeps those matching the filters, orders them most-actionable first (Blocked
// before Pending, then namespace/kind/name for determinism), and caps Items
// while keeping honest pre-cap Summary and Scanned totals. Pure: no cluster
// access, fully unit-testable.
func BuildUnhealthy(listed []k8s.Listed, p UnhealthyParams) *UnhealthyResult {
	kindFilter := strings.ToLower(strings.TrimSpace(p.Kind))
	res := &UnhealthyResult{}

	var items []UnhealthyItem
	for i := range listed {
		item := &listed[i]
		obj := &item.Object
		if kindFilter != "" && strings.ToLower(obj.GetKind()) != kindFilter {
			continue
		}
		health, state := Classify(obj.GetAPIVersion(), Conditions(obj))
		dt := deletionTime(obj)
		res.Scanned++
		// Terminating is checked first and counted separately: a resource whose
		// reconciler died mid-teardown keeps Ready=True forever, so classifying
		// it by conditions alone would tally it as healthy and drop it from
		// triage — the exact state diagnose refuses to call healthy.
		switch {
		case dt != "":
			res.Summary.Terminating++
		case state == StateBlocked:
			res.Summary.Blocked++
		case state == StatePending:
			res.Summary.Pending++
		default:
			// Ready, plus StateUnknown — not-failing either way.
			res.Summary.Ready++
		}
		// Hide StateUnknown alongside Ready: it is "not assessed", not a problem
		// to triage. Defensive rather than load-bearing — list_unhealthy's
		// discovery is category-scoped (composite/claim/managed) and native types
		// are never stamped with those Crossplane categories, so this is currently
		// unreachable here.
		// A terminating row survives the healthy filter whatever its conditions
		// say — that is the whole point of reading deletionTimestamp here.
		if (state == StateReady || state == StateUnknown) && dt == "" && !p.IncludeHealthy {
			continue
		}
		items = append(items, UnhealthyItem{
			APIVersion: obj.GetAPIVersion(),
			Kind:       obj.GetKind(),
			Name:       obj.GetName(),
			Namespace:  obj.GetNamespace(),
			Category:   item.Category,
			State:      state,
			Ready:      health.Ready,
			Synced:     health.Synced,
			Paused:     IsPaused(obj),

			DeletionTimestamp: dt,
		})
	}

	sort.SliceStable(items, func(a, b int) bool {
		x, y := items[a], items[b]
		if x.State != y.State {
			return stateRank(x.State) < stateRank(y.State)
		}
		if x.Namespace != y.Namespace {
			return x.Namespace < y.Namespace
		}
		if x.Kind != y.Kind {
			return x.Kind < y.Kind
		}
		return x.Name < y.Name
	})

	if p.Limit > 0 && len(items) > p.Limit {
		items = items[:p.Limit]
		res.Truncated = true
	}
	res.Items = items
	return res
}

// stateRank orders states most-actionable first, matching diagnose's
// blocking-before-pending philosophy.
func stateRank(s string) int {
	switch s {
	case StateBlocked:
		return 0
	case StatePending:
		return 1
	default:
		return 2
	}
}
