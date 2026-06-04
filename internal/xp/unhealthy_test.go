package xp

import (
	"strconv"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/briferz/crossplane-mcp/internal/k8s"
)

// listed builds a k8s.Listed test fixture. Passing no conditions leaves
// status.conditions absent, which Classify treats as Pending.
func listed(category, apiVersion, kind, ns, name string, conds ...map[string]any) k8s.Listed {
	meta := map[string]any{"name": name}
	if ns != "" {
		meta["namespace"] = ns
	}
	o := map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": meta}
	if conds != nil {
		cs := make([]any, len(conds))
		for i, c := range conds {
			cs[i] = c
		}
		o["status"] = map[string]any{"conditions": cs}
	}
	return k8s.Listed{Category: category, Object: unstructured.Unstructured{Object: o}}
}

func cnd(typ, status string) map[string]any {
	return map[string]any{"type": typ, "status": status}
}

func TestBuildUnhealthyFilters(t *testing.T) {
	in := []k8s.Listed{
		listed("composite", "ex.org/v1", "XApp", "ns1", "ready1", cnd("Ready", "True"), cnd("Synced", "True")),
		listed("composite", "ex.org/v1", "XApp", "ns1", "blocked1", cnd("Ready", "False")),
		listed("claim", "ex.org/v1", "AppClaim", "ns2", "pending1"), // no conditions -> Pending
	}

	r := BuildUnhealthy(in, UnhealthyParams{})
	if r.Scanned != 3 {
		t.Errorf("Scanned = %d, want 3", r.Scanned)
	}
	if r.Summary.Ready != 1 || r.Summary.Blocked != 1 || r.Summary.Pending != 1 {
		t.Errorf("summary = %+v, want blocked/pending/ready 1/1/1", r.Summary)
	}
	if len(r.Items) != 2 {
		t.Fatalf("default should return 2 unhealthy items, got %d", len(r.Items))
	}
	if r.Items[0].State != StateBlocked || r.Items[1].State != StatePending {
		t.Errorf("ordering wrong: %s then %s", r.Items[0].State, r.Items[1].State)
	}
	// Category is carried through.
	if r.Items[1].Category != "claim" {
		t.Errorf("expected category claim on the pending item, got %q", r.Items[1].Category)
	}

	r = BuildUnhealthy(in, UnhealthyParams{IncludeHealthy: true})
	if len(r.Items) != 3 {
		t.Errorf("IncludeHealthy should return all 3, got %d", len(r.Items))
	}
}

func TestBuildUnhealthyKindFilter(t *testing.T) {
	in := []k8s.Listed{
		listed("composite", "ex.org/v1", "XApp", "ns1", "a", cnd("Ready", "False")),
		listed("composite", "ex.org/v1", "XDatabase", "ns1", "b", cnd("Ready", "False")),
	}
	r := BuildUnhealthy(in, UnhealthyParams{Kind: "xapp"}) // case-insensitive
	if len(r.Items) != 1 || r.Items[0].Kind != "XApp" {
		t.Errorf("kind filter failed: %+v", r.Items)
	}
	if r.Scanned != 1 {
		t.Errorf("Scanned should count only kind-matched resources, got %d", r.Scanned)
	}
}

func TestBuildUnhealthyCapKeepsHonestTotals(t *testing.T) {
	var in []k8s.Listed
	for i := 0; i < 5; i++ {
		in = append(in, listed("composite", "ex.org/v1", "XApp", "ns", "b"+strconv.Itoa(i), cnd("Synced", "False")))
	}
	r := BuildUnhealthy(in, UnhealthyParams{Limit: 2})
	if len(r.Items) != 2 {
		t.Errorf("expected 2 capped items, got %d", len(r.Items))
	}
	if !r.Truncated {
		t.Error("expected Truncated=true when matches exceed the limit")
	}
	if r.Scanned != 5 || r.Summary.Blocked != 5 {
		t.Errorf("pre-cap totals must stay honest: scanned=%d blocked=%d, want 5/5", r.Scanned, r.Summary.Blocked)
	}
}

func TestBuildUnhealthyOrdering(t *testing.T) {
	in := []k8s.Listed{
		listed("composite", "ex.org/v1", "XApp", "ns-b", "x", cnd("Ready", "False")),
		listed("composite", "ex.org/v1", "XApp", "ns-a", "y", cnd("Ready", "False")),
		listed("claim", "ex.org/v1", "AppClaim", "ns-a", "z"), // pending
	}
	r := BuildUnhealthy(in, UnhealthyParams{})
	if r.Items[0].Namespace != "ns-a" || r.Items[0].State != StateBlocked {
		t.Errorf("first should be blocked in ns-a, got %+v", r.Items[0])
	}
	if r.Items[1].Namespace != "ns-b" || r.Items[1].State != StateBlocked {
		t.Errorf("second should be blocked in ns-b, got %+v", r.Items[1])
	}
	if r.Items[2].State != StatePending {
		t.Errorf("pending should rank last, got %+v", r.Items[2])
	}
}

func TestBuildUnhealthyKindTiebreak(t *testing.T) {
	in := []k8s.Listed{
		listed("composite", "ex.org/v1", "XDatabase", "ns-a", "d", cnd("Ready", "False")),
		listed("composite", "ex.org/v1", "XApp", "ns-a", "a", cnd("Ready", "False")),
	}
	r := BuildUnhealthy(in, UnhealthyParams{})
	// Same state and namespace: kind decides — XApp before XDatabase.
	if r.Items[0].Kind != "XApp" || r.Items[1].Kind != "XDatabase" {
		t.Errorf("kind tiebreaker failed: %s then %s", r.Items[0].Kind, r.Items[1].Kind)
	}
}

func TestBuildUnhealthyEmpty(t *testing.T) {
	r := BuildUnhealthy(nil, UnhealthyParams{})
	if r.Scanned != 0 || len(r.Items) != 0 || r.Truncated {
		t.Errorf("empty input should yield empty result, got %+v", r)
	}
}
