package xp

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/briferz/crossplane-mcp/internal/k8s"
)

// Crossplane v2 XRs compose native Kubernetes resources directly, and those
// never carry Ready/Synced/Healthy. Reading their silence as StatePending made
// every one of them a permanent suspect with empty reasons — and, being usually
// the deepest node in the tree, frequently the named root cause. These pin the
// fix and, just as importantly, the constraint it must not break: a Crossplane
// resource that simply hasn't reported conditions yet still reads Pending.

// TestDiagnoseNativeComposedResourcesNotSuspects is the headline regression: a
// v2 XR composing native resources, all healthy, must diagnose clean.
func TestDiagnoseNativeComposedResourcesNotSuspects(t *testing.T) {
	cm := nodeAPI(1, "v1", "ConfigMap", "app-config", nil)
	cm.creationTime = "2020-01-01T00:00:00Z"
	deploy := nodeAPI(1, "apps/v1", "Deployment", "app",
		[]Condition{cond("Available", "True", "", ""), cond("Progressing", "True", "", "")})
	bucket := node(1, "Bucket", "assets",
		[]Condition{cond("Ready", "True", "", ""), cond("Synced", "True", "", "")})
	root := node(0, "XApp", "app",
		[]Condition{cond("Ready", "True", "", ""), cond("Synced", "True", "", "")},
		cm, deploy, bucket)

	d := Diagnose(context.Background(), &stubEvents{}, root, Stats{Nodes: 4}, false)

	if !d.Healthy {
		t.Fatalf("a healthy tree with native composed resources must diagnose healthy; suspects: %+v", d.Suspects)
	}
	if len(d.Suspects) != 0 {
		t.Fatalf("native resources must not be suspects, got %+v", d.Suspects)
	}
	// The summary must not claim readiness it cannot assert.
	if strings.Contains(d.Summary, "are Ready") {
		t.Errorf("summary must not claim all resources are Ready when 2 were not assessed: %q", d.Summary)
	}
	if !strings.Contains(d.Summary, "not assessed") || !strings.Contains(d.Summary, "2") {
		t.Errorf("summary should say how many were not assessed, got %q", d.Summary)
	}

	// Excluded from suspects, but never dropped from the tree.
	withTree := Diagnose(context.Background(), &stubEvents{}, root, Stats{Nodes: 4}, true)
	var found bool
	for _, n := range withTree.Tree {
		if n.Name == "app-config" {
			found = true
			if n.State != StateUnknown {
				t.Errorf("ConfigMap node State = %q, want %q", n.State, StateUnknown)
			}
		}
	}
	if !found {
		t.Error("the native node must still appear in the tree, just not as a suspect")
	}
}

// TestDiagnoseFreshManagedResourceStillPending is the companion constraint: the
// fix must not silence a Crossplane resource that has yet to report.
func TestDiagnoseFreshManagedResourceStillPending(t *testing.T) {
	mr := nodeAPI(0, "s3.aws.upbound.io/v1beta1", "Bucket", "fresh", nil)
	mr.creationTime = "2026-06-03T00:00:00Z"

	d := Diagnose(context.Background(), &stubEvents{}, mr, Stats{Nodes: 1}, false)

	if d.Healthy {
		t.Fatalf("a managed resource with no conditions yet must not read healthy; summary: %s", d.Summary)
	}
	if len(d.Suspects) != 1 || d.Suspects[0].Name != "fresh" {
		t.Fatalf("expected the fresh MR as the single suspect, got %+v", d.Suspects)
	}
}

// TestDiagnoseNativeTerminatingStillSurfaced: not being assessable for readiness
// does not exempt a resource from a wedged teardown.
func TestDiagnoseNativeTerminatingStillSurfaced(t *testing.T) {
	orig := nowFn
	nowFn = func() time.Time { return time.Date(2026, 6, 4, 0, 0, 0, 0, time.UTC) }
	defer func() { nowFn = orig }()

	cm := nodeAPI(1, "v1", "ConfigMap", "stuck-config", nil)
	cm.deletionTime = "2026-01-15T00:00:00Z"
	root := node(0, "XApp", "app",
		[]Condition{cond("Ready", "True", "", ""), cond("Synced", "True", "", "")}, cm)

	d := Diagnose(context.Background(), &stubEvents{}, root, Stats{Nodes: 2}, false)

	if d.Healthy {
		t.Fatal("a tree containing a wedged teardown must not read healthy")
	}
	if len(d.Suspects) != 1 || d.Suspects[0].Name != "stuck-config" {
		t.Fatalf("expected the terminating ConfigMap as the sole suspect, got %+v", d.Suspects)
	}
	if got := d.Suspects[0].Lifecycle; got != "Terminating (stuck 140d)" {
		t.Errorf("Lifecycle = %q, want %q", got, "Terminating (stuck 140d)")
	}
}

// TestDiagnoseNativeUnknownNeverOutranksRealFailure guards the precise failure
// in the audit: a deep native resource must not displace the real root cause.
func TestDiagnoseNativeUnknownNeverOutranksRealFailure(t *testing.T) {
	orig := nowFn
	nowFn = func() time.Time { return time.Date(2026, 6, 4, 0, 0, 0, 0, time.UTC) }
	defer func() { nowFn = orig }()

	// The ConfigMap is deeper, so under the old deepest-first-within-tier rule it
	// would win if it shared a tier with the genuinely stuck XR.
	cm := nodeAPI(2, "v1", "ConfigMap", "deep-config", nil)
	cm.deletionTime = "2026-06-01T00:00:00Z"
	stuck := node(1, "XDatabase", "db",
		[]Condition{cond("Ready", "Unknown", "Provisioning", "waiting for the provider")}, cm)
	root := node(0, "XApp", "app",
		[]Condition{cond("Ready", "True", "", ""), cond("Synced", "True", "", "")}, stuck)

	d := Diagnose(context.Background(), &stubEvents{}, root, Stats{Nodes: 3}, false)

	if len(d.Suspects) == 0 {
		t.Fatal("expected suspects")
	}
	if d.Suspects[0].Kind != "XDatabase" {
		t.Errorf("top suspect = %s/%s (depth %d), want the Pending XDatabase — a deeper "+
			"unassessable resource must not be named root cause",
			d.Suspects[0].Kind, d.Suspects[0].Name, d.Suspects[0].Depth)
	}
}

// TestLifecycleLabelUnknownState: "Creating (pending, 340d)" on a ConfigMap that
// has been fine for a year is nonsense, but a terminating one keeps its label.
func TestLifecycleLabelUnknownState(t *testing.T) {
	now := time.Date(2026, 6, 4, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		node *Node
		want string
	}{
		{"unknown-no-label", &Node{State: StateUnknown, creationTime: "2020-01-01T00:00:00Z"}, ""},
		{"unknown-paused-no-label", &Node{State: StateUnknown, paused: true, creationTime: "2026-05-30T00:00:00Z"}, ""},
		{"unknown-terminating-still-labelled", &Node{State: StateUnknown, deletionTime: "2026-01-15T00:00:00Z"}, "Terminating (stuck 140d)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := lifecycleLabel(c.node, now); got != c.want {
				t.Errorf("lifecycleLabel() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestBuildTreeNativeRootUnknown pins that build() forwards the object's
// apiVersion to Classify — the plumbing the rest of this fix rests on.
func TestBuildTreeNativeRootUnknown(t *testing.T) {
	root := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": "cm", "namespace": "ns"},
	}}

	// A childless root never touches the client, so a nil one is safe here.
	node, _ := BuildTree(context.Background(), nil, root)

	if node.State != StateUnknown {
		t.Errorf("State = %q, want %q — build() must pass the apiVersion to Classify", node.State, StateUnknown)
	}
	if flat := node.Flatten(); len(flat) != 1 || flat[0].State != StateUnknown {
		t.Errorf("flattened state = %+v, want a single %s node", flat, StateUnknown)
	}
}

// TestBuildUnhealthyVocabularyLessObjectNotUnhealthy documents the defensive
// path: list_unhealthy's discovery is category-scoped, so native types are not
// reachable here today — but if that ever changes they must not read as broken.
func TestBuildUnhealthyVocabularyLessObjectNotUnhealthy(t *testing.T) {
	in := []k8s.Listed{listed("composite", "v1", "ConfigMap", "ns", "cm")}

	res := BuildUnhealthy(in, UnhealthyParams{})
	if len(res.Items) != 0 {
		t.Errorf("a vocabulary-less object must not be listed as unhealthy, got %+v", res.Items)
	}
	if res.Scanned != 1 || res.Summary.Ready != 1 {
		t.Errorf("scanned/ready = %d/%d, want 1/1", res.Scanned, res.Summary.Ready)
	}

	res = BuildUnhealthy(in, UnhealthyParams{IncludeHealthy: true})
	if len(res.Items) != 1 || res.Items[0].State != StateUnknown {
		t.Errorf("with IncludeHealthy the row should appear as %s, got %+v", StateUnknown, res.Items)
	}
}
