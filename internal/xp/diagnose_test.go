package xp

import (
	"context"
	"strings"
	"testing"

	"github.com/briferz/crossplane-mcp/internal/k8s"
)

// stubEvents is an EventFetcher that records uids it was asked about.
type stubEvents struct{ asked []string }

func (s *stubEvents) Events(_ context.Context, uid string, _ int) ([]k8s.Event, error) {
	s.asked = append(s.asked, uid)
	return []k8s.Event{{Type: "Warning", Reason: "Stub", Message: "for " + uid}}, nil
}

func cond(t, status, reason, msg string) Condition {
	return Condition{Type: t, Status: status, Reason: reason, Message: msg}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name  string
		conds []Condition
		want  string
	}{
		{"ready+synced true", []Condition{cond("Ready", "True", "", ""), cond("Synced", "True", "", "")}, StateReady},
		{"ready false blocks", []Condition{cond("Ready", "False", "Waiting", "not yet"), cond("Synced", "True", "", "")}, StateBlocked},
		{"synced false blocks", []Condition{cond("Ready", "True", "", ""), cond("Synced", "False", "ApplyErr", "boom")}, StateBlocked},
		{"unknown is pending", []Condition{cond("Ready", "Unknown", "", "")}, StatePending},
		{"no conditions is pending", nil, StatePending},
		{"healthy false blocks", []Condition{cond("Healthy", "False", "Unpacking", "bad pkg")}, StateBlocked},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, got := Classify(tt.conds); got != tt.want {
				t.Errorf("Classify() = %q, want %q", got, tt.want)
			}
		})
	}
}

// buildNode is a test helper to construct a tree node with state derived from
// its conditions.
func node(depth int, kind, name string, conds []Condition, children ...*Node) *Node {
	h, state := Classify(conds)
	return &Node{
		APIVersion: "example.org/v1",
		Kind:       kind,
		Name:       name,
		State:      state,
		Health:     h,
		Conditions: conds,
		Children:   children,
		uid:        name + "-uid",
		depth:      depth,
	}
}

// TestDiagnoseRanksDeepestFirst is the core differentiator: when a composite is
// Ready:False only because a leaf managed resource is failing, the leaf — not
// the top-level symptom — must be reported as the likely root cause.
func TestDiagnoseRanksDeepestFirst(t *testing.T) {
	leaf := node(2, "Bucket", "my-bucket",
		[]Condition{cond("Ready", "False", "Creating", ""), cond("Synced", "False", "ReconcileError", "AccessDenied: invalid credentials")})
	mid := node(1, "XStorage", "storage-xyz",
		[]Condition{cond("Ready", "False", "Waiting", "waiting for composed resources")}, leaf)
	root := node(0, "App", "app-xyz",
		[]Condition{cond("Ready", "False", "Waiting", "waiting for composite")}, mid)

	stub := &stubEvents{}
	d := Diagnose(context.Background(), stub, root, Stats{Nodes: 3}, false)

	if d.Healthy {
		t.Fatal("expected unhealthy diagnosis")
	}
	if len(d.Suspects) != 3 {
		t.Fatalf("expected 3 suspects, got %d", len(d.Suspects))
	}
	// Deepest (the leaf Bucket) must rank first.
	if got := d.Suspects[0]; got.Kind != "Bucket" || got.Depth != 2 {
		t.Errorf("expected leaf Bucket (depth 2) as top suspect, got %s (depth %d)", got.Kind, got.Depth)
	}
	// The root-cause condition message must survive untruncated (it appears
	// among the leaf's reasons, alongside the less-informative Ready reason).
	if !strings.Contains(strings.Join(d.Suspects[0].Reasons, " | "), "AccessDenied: invalid credentials") {
		t.Errorf("expected untruncated AccessDenied message, got %v", d.Suspects[0].Reasons)
	}
	// Events should be fetched for the top suspect.
	if len(d.Suspects[0].Events) == 0 {
		t.Error("expected events fetched for top suspect")
	}
}

func TestDiagnoseHealthy(t *testing.T) {
	root := node(0, "App", "ok",
		[]Condition{cond("Ready", "True", "", "")},
		node(1, "Bucket", "b", []Condition{cond("Ready", "True", "", ""), cond("Synced", "True", "", "")}),
	)
	d := Diagnose(context.Background(), &stubEvents{}, root, Stats{Nodes: 2}, false)
	if !d.Healthy {
		t.Errorf("expected healthy, got: %s", d.Summary)
	}
	if len(d.Suspects) != 0 {
		t.Errorf("expected no suspects, got %d", len(d.Suspects))
	}
}

func TestFlatten(t *testing.T) {
	root := node(0, "App", "app",
		[]Condition{cond("Ready", "True", "", "")},
		node(1, "XStorage", "s", []Condition{cond("Ready", "False", "", "")},
			node(2, "Bucket", "b", []Condition{cond("Synced", "False", "", "")})),
	)
	flat := root.Flatten()
	if len(flat) != 3 {
		t.Fatalf("expected 3 flat nodes, got %d", len(flat))
	}
	if flat[0].Parent != -1 {
		t.Errorf("root parent should be -1, got %d", flat[0].Parent)
	}
	if flat[1].Parent != 0 || flat[2].Parent != 1 {
		t.Errorf("parent indices wrong: %d, %d", flat[1].Parent, flat[2].Parent)
	}
}
