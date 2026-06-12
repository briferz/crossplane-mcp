package xp

// Mass-failure budget guard: a pathological cluster — hundreds of broken
// packages (e.g. a registry outage) with GC-failed revision piles — must stay
// inside every cap AND a sane token budget. This is what the enrichment cap
// in BuildPackages exists for.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/briferz/crossplane-mcp/internal/k8s"
)

func TestBuildPackagesMassFailureBudget(t *testing.T) {
	const nPkgs, nRevs = 300, 8
	longMsg := "cannot unpack package: GET https://registry.example.org/v2/x/manifests/v9: UNAUTHORIZED: " +
		strings.Repeat("auth required; ", 30) // ~500B condition message each

	var pkgs, revs []k8s.Listed
	for i := 0; i < nPkgs; i++ {
		name := fmt.Sprintf("provider-%03d", i)
		pkgs = append(pkgs, pkgObj("Provider", name, fmt.Sprintf("u%d", i),
			map[string]any{"package": "xpkg.example.org/" + name + ":v9", "revisionHistoryLimit": int64(1)},
			map[string]any{"currentRevision": fmt.Sprintf("%s-%d", name, nRevs), "currentIdentifier": "xpkg.example.org/" + name + ":v8"},
			cndR(TypeInstalled, "False", "UnpackingPackage", longMsg),
			cndR(TypeHealthy, "False", "UnhealthyPackageRevision", longMsg),
		))
		for j := 1; j <= nRevs; j++ {
			state := "Inactive"
			if j == nRevs-1 {
				state = "Active"
			}
			revs = append(revs, revObj("ProviderRevision", fmt.Sprintf("%s-%d", name, j), fmt.Sprintf("u%d-r%d", i, j),
				name, int64(j), state,
				cndR(TypeRevisionHealthy, "False", "UnhealthyPackageRevision", longMsg)))
		}
	}

	r := BuildPackages(pkgs, revs, PackagesParams{Limit: 100, RevisionsListed: true})
	if len(r.Items) != 100 || !r.Truncated || r.Scanned != nPkgs || r.Summary.Blocked != nPkgs {
		t.Fatalf("caps/honesty broke at scale: items=%d truncated=%v scanned=%d blocked=%d",
			len(r.Items), r.Truncated, r.Scanned, r.Summary.Blocked)
	}
	for i, it := range r.Items {
		if len(it.Revisions) > maxRevisionRows {
			t.Fatalf("revision cap broke: %d rows on %s", len(it.Revisions), it.Name)
		}
		if *it.RevisionCount != nRevs {
			t.Fatalf("revisionCount must stay honest on %s: %v", it.Name, it.RevisionCount)
		}
		if i < maxDetailedPackages && !it.RevisionsTruncated {
			t.Fatalf("detailed row %s should flag its capped revisions", it.Name)
		}
		if i >= maxDetailedPackages && (it.Reasons != nil || it.Revisions != nil) {
			t.Fatalf("row %d (%s) beyond the detail cap must be compact", i, it.Name)
		}
	}

	ev := &uidEvents{events: map[string][]k8s.Event{}}
	notes := DecoratePackageEvents(context.Background(), ev, r.Items)
	if len(ev.asked) > maxDetailedPackages*(1+maxRevisionRows) {
		t.Fatalf("event fetch budget broke: %d fetches", len(ev.asked))
	}

	out := struct {
		Items   []PackageRow     `json:"items"`
		Summary UnhealthySummary `json:"summary"`
		Scanned int              `json:"scanned"`
		Trunc   bool             `json:"truncated"`
		Notes   []string         `json:"notes"`
	}{r.Items, r.Summary, r.Scanned, r.Truncated, append(r.Notes, notes...)}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("worst-case output: %d items, %d event fetches, %d KiB JSON (~%dk tokens)",
		len(r.Items), len(ev.asked), len(b)/1024, len(b)/4/1000)
	// The budget itself: without the enrichment cap this was 542 KiB (~138k
	// tokens). Whole-field omission keeps the worst case bounded.
	if len(b) > 128*1024 {
		t.Fatalf("mass-failure output budget exceeded: %d KiB", len(b)/1024)
	}
}
