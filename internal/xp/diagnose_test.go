package xp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/briferz/crossplane-mcp/internal/k8s"
)

// stubEvents is an EventFetcher for tests. It records the uids and limit it was
// asked about, and returns a configurable event set: when events is nil it falls
// back to a single benign Warning with Count 0 (below recurrenceThreshold, so it
// never triggers event attribution — keeping pre-existing tests unchanged).
type stubEvents struct {
	asked     []string
	lastLimit int
	events    []k8s.Event
	err       error
}

func (s *stubEvents) Events(_ context.Context, _, uid string, limit int) ([]k8s.Event, error) {
	s.asked = append(s.asked, uid)
	s.lastLimit = limit
	if s.err != nil {
		return nil, s.err
	}
	if s.events != nil {
		return s.events, nil
	}
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

// TestDiagnosePendingNotHealthy guards the fix for the bug where a resource
// stuck Pending (Unknown/absent conditions) was reported as healthy because
// only Blocked nodes were collected.
func TestDiagnosePendingNotHealthy(t *testing.T) {
	root := node(0, "App", "app",
		[]Condition{cond("Ready", "Unknown", "Provisioning", "still reconciling")})
	d := Diagnose(context.Background(), &stubEvents{}, root, Stats{Nodes: 1}, false)

	if d.Healthy {
		t.Fatalf("pending resource must not be reported healthy; summary: %s", d.Summary)
	}
	if len(d.Suspects) != 1 || d.Suspects[0].Kind != "App" {
		t.Fatalf("expected the pending App as a suspect, got %+v", d.Suspects)
	}
	// The Unknown condition's detail should be surfaced.
	if len(d.Suspects[0].Reasons) == 0 || !strings.Contains(d.Suspects[0].Reasons[0], "still reconciling") {
		t.Errorf("expected Unknown condition message surfaced, got %v", d.Suspects[0].Reasons)
	}
}

// TestDiagnoseBlockedRanksAbovePending confirms a deeper Pending node still
// ranks below a shallower Blocked one.
func TestDiagnoseBlockedRanksAbovePending(t *testing.T) {
	deepPending := node(2, "Bucket", "b", []Condition{cond("Ready", "Unknown", "", "")})
	blocked := node(1, "XStorage", "s", []Condition{cond("Synced", "False", "ApplyErr", "denied")}, deepPending)
	root := node(0, "App", "a", []Condition{cond("Ready", "False", "Waiting", "")}, blocked)

	d := Diagnose(context.Background(), &stubEvents{}, root, Stats{Nodes: 3}, false)
	if d.Suspects[0].Kind != "XStorage" {
		t.Errorf("expected Blocked XStorage ranked first over deeper Pending Bucket, got %s", d.Suspects[0].Kind)
	}
}

// compEvent is the recurring composition/validation event from the issue #24
// real-world incident (count ~2666), used across the attribution tests.
func compEvent(count int64) k8s.Event {
	return k8s.Event{
		Type:    "Warning",
		Reason:  "ComposeResources",
		Count:   count,
		Message: "cannot compose resources: cannot apply composite resource status: status.conditions[1].lastTransitionTime: Required value",
	}
}

// TestDiagnoseAttributesRecurringEventOverFlake is the headline issue #24 P1
// regression: when the latest condition is a transient transport flake but a
// composition event recurs thousands of times, the recurring event — not the
// flake — must be reported as the cause.
func TestDiagnoseAttributesRecurringEventOverFlake(t *testing.T) {
	leaf := node(0, "XApp", "app",
		[]Condition{cond("Synced", "False", "ReconcileError", "rpc error: code = Unavailable desc = transport is closing")})
	stub := &stubEvents{events: []k8s.Event{compEvent(2666)}}
	d := Diagnose(context.Background(), stub, leaf, Stats{Nodes: 1}, false)

	if !strings.Contains(d.Summary, "ComposeResources (x2666)") {
		t.Errorf("summary should surface the recurring event; got: %s", d.Summary)
	}
	if !strings.Contains(d.Summary, "Required value") {
		t.Errorf("summary should carry the composition error message; got: %s", d.Summary)
	}
	if strings.Contains(d.Summary, "rpc error") {
		t.Errorf("summary should not lead with the transport flake; got: %s", d.Summary)
	}
	// The override path surfaces the event once and must NOT also append the
	// "Recurring event:" tail (that is for the condition-led path only).
	if strings.Contains(d.Summary, "Recurring event:") {
		t.Errorf("override summary must not also append the recurring-event tail; got: %s", d.Summary)
	}
	// The full composition message survives untruncated in the summary.
	if !strings.Contains(d.Summary, compEvent(2666).Message) {
		t.Errorf("summary should carry the untruncated event message; got: %s", d.Summary)
	}
	if len(d.Suspects[0].Reasons) == 0 || !strings.HasPrefix(d.Suspects[0].Reasons[0], "event: ComposeResources") {
		t.Errorf("recurring event should be prepended to reasons; got: %v", d.Suspects[0].Reasons)
	}
	// The transport condition is demoted, not hidden.
	if !strings.Contains(strings.Join(d.Suspects[0].Reasons, " | "), "rpc error: code = Unavailable") {
		t.Errorf("transport condition should still appear in reasons; got: %v", d.Suspects[0].Reasons)
	}
}

// TestDiagnoseSurfacesRecurringEventWhenConditionWins covers requirement 3: even
// when a genuine (non-flake) condition leads, a qualifying recurring event is
// still surfaced so a hot loop behind the symptom is never missed.
func TestDiagnoseSurfacesRecurringEventWhenConditionWins(t *testing.T) {
	leaf := node(0, "Bucket", "b",
		[]Condition{cond("Synced", "False", "ReconcileError", "AccessDenied: invalid credentials")})
	stub := &stubEvents{events: []k8s.Event{compEvent(42)}}
	d := Diagnose(context.Background(), stub, leaf, Stats{Nodes: 1}, false)

	if !strings.Contains(d.Summary, "AccessDenied: invalid credentials") {
		t.Errorf("genuine condition should lead the summary; got: %s", d.Summary)
	}
	if !strings.Contains(d.Summary, "Recurring event: ComposeResources (x42).") {
		t.Errorf("summary should append a recurring-event pointer; got: %s", d.Summary)
	}
	if !strings.HasPrefix(d.Suspects[0].Reasons[0], "Synced:") {
		t.Errorf("genuine condition should remain the first reason; got: %v", d.Suspects[0].Reasons)
	}
	if !strings.Contains(strings.Join(d.Suspects[0].Reasons, " | "), "event: ComposeResources (x42)") {
		t.Errorf("event line should be appended to reasons; got: %v", d.Suspects[0].Reasons)
	}
}

// TestDiagnoseNoRecurringEventUnchanged is the golden no-regression test: with
// no qualifying event the summary and reasons are byte-identical to before.
func TestDiagnoseNoRecurringEventUnchanged(t *testing.T) {
	leaf := node(0, "Bucket", "b",
		[]Condition{cond("Synced", "False", "ReconcileError", "AccessDenied: invalid credentials")})
	d := Diagnose(context.Background(), &stubEvents{}, leaf, Stats{Nodes: 1}, false)

	if !strings.HasSuffix(d.Summary, "AccessDenied: invalid credentials") {
		t.Errorf("summary should end with the condition message; got: %s", d.Summary)
	}
	if strings.Contains(d.Summary, "(x") || strings.Contains(d.Summary, "Recurring event") {
		t.Errorf("no event annotation expected without a qualifying event; got: %s", d.Summary)
	}
	if len(d.Suspects[0].Reasons) != 1 || d.Suspects[0].Reasons[0] != "Synced: ReconcileError — AccessDenied: invalid credentials" {
		t.Errorf("reasons should be the bare condition message; got: %v", d.Suspects[0].Reasons)
	}
}

// TestDiagnoseBelowThresholdIgnored confirms a low-count event never overrides
// or annotates the condition.
func TestDiagnoseBelowThresholdIgnored(t *testing.T) {
	leaf := node(0, "XApp", "app",
		[]Condition{cond("Synced", "False", "ReconcileError", "context deadline exceeded")})
	stub := &stubEvents{events: []k8s.Event{compEvent(3)}}
	d := Diagnose(context.Background(), stub, leaf, Stats{Nodes: 1}, false)

	if !strings.Contains(d.Summary, "context deadline exceeded") {
		t.Errorf("below-threshold event must not override the condition; got: %s", d.Summary)
	}
	if strings.Contains(d.Summary, "(x") || strings.Contains(d.Summary, "Recurring event") {
		t.Errorf("below-threshold event must not be annotated; got: %s", d.Summary)
	}
}

// TestDiagnoseFetchesAllEvents guards that diagnose requests the full event set
// (no cap) so attribution can't miss a recurring composition event evicted from
// a capped, newest-first window.
func TestDiagnoseFetchesAllEvents(t *testing.T) {
	leaf := node(0, "Bucket", "b", []Condition{cond("Synced", "False", "Err", "boom")})
	stub := &stubEvents{}
	Diagnose(context.Background(), stub, leaf, Stats{Nodes: 1}, false)
	if stub.lastLimit != allEvents {
		t.Errorf("expected diagnose to request all events (limit %d), got %d", allEvents, stub.lastLimit)
	}
}

// TestDiagnoseOrderingPreservedUnderRecurrence guards the hard constraint: event
// recurrence must not reorder the deepest-first node ranking.
func TestDiagnoseOrderingPreservedUnderRecurrence(t *testing.T) {
	leaf := node(2, "Bucket", "my-bucket",
		[]Condition{cond("Synced", "False", "ReconcileError", "AccessDenied")})
	mid := node(1, "XStorage", "s",
		[]Condition{cond("Ready", "False", "Waiting", "")}, leaf)
	root := node(0, "App", "a",
		[]Condition{cond("Ready", "False", "Waiting", "")}, mid)
	stub := &stubEvents{events: []k8s.Event{compEvent(999)}}
	d := Diagnose(context.Background(), stub, root, Stats{Nodes: 3}, false)
	if got := d.Suspects[0]; got.Kind != "Bucket" || got.Depth != 2 {
		t.Errorf("event recurrence must not reorder nodes; want depth-2 Bucket, got %s (depth %d)", got.Kind, got.Depth)
	}
}

// TestDiagnoseTrimsSurfacedEvents confirms the response surfaces only a
// token-light slice of events while always retaining the qualifying recurring
// event — even when it is the oldest and falls outside the newest window — and
// that it still drives attribution.
func TestDiagnoseTrimsSurfacedEvents(t *testing.T) {
	q := compEvent(2666)
	q.Last = "2026-01-01T00:00:00Z" // oldest, so outside the newest-displayEvents window
	many := []k8s.Event{
		q,
		{Type: "Normal", Reason: "S1", Count: 1, Last: "2026-01-01T01:01:00Z", Message: "ok"},
		{Type: "Normal", Reason: "S2", Count: 1, Last: "2026-01-01T01:02:00Z", Message: "ok"},
		{Type: "Normal", Reason: "S3", Count: 1, Last: "2026-01-01T01:03:00Z", Message: "ok"},
		{Type: "Normal", Reason: "S4", Count: 1, Last: "2026-01-01T01:04:00Z", Message: "ok"},
		{Type: "Normal", Reason: "S5", Count: 1, Last: "2026-01-01T01:05:00Z", Message: "ok"},
		{Type: "Normal", Reason: "S6", Count: 1, Last: "2026-01-01T01:06:00Z", Message: "ok"},
	}
	leaf := node(0, "XApp", "app",
		[]Condition{cond("Synced", "False", "ReconcileError", "context deadline exceeded")})
	stub := &stubEvents{events: many}
	d := Diagnose(context.Background(), stub, leaf, Stats{Nodes: 1}, false)

	if got := len(d.Suspects[0].Events); got > displayEvents+1 {
		t.Errorf("surfaced events should be trimmed to ~%d, got %d", displayEvents, got)
	}
	found := false
	for _, e := range d.Suspects[0].Events {
		if e.Reason == "ComposeResources" {
			found = true
		}
	}
	if !found {
		t.Error("qualifying event must be retained after trimming")
	}
	if !strings.Contains(d.Summary, "ComposeResources (x2666)") {
		t.Errorf("trimmed-out qualifying event must still drive attribution; got: %s", d.Summary)
	}
}

// TestDiagnoseEventsFetchError confirms a failed events fetch degrades cleanly
// to condition-only attribution.
func TestDiagnoseEventsFetchError(t *testing.T) {
	leaf := node(0, "Bucket", "b",
		[]Condition{cond("Synced", "False", "ReconcileError", "AccessDenied: invalid credentials")})
	stub := &stubEvents{err: errors.New("forbidden")}
	d := Diagnose(context.Background(), stub, leaf, Stats{Nodes: 1}, false)
	if len(d.Suspects[0].Events) != 0 {
		t.Errorf("expected no events on fetch error, got %d", len(d.Suspects[0].Events))
	}
	if !strings.HasSuffix(d.Summary, "AccessDenied: invalid credentials") {
		t.Errorf("summary should fall back to the condition on fetch error; got: %s", d.Summary)
	}
}

// TestDiagnoseDecodedErrorsOnDeepLeaf confirms a base64+gzip OpenTofu blob in a
// deep leaf's Synced condition is decoded into DecodedErrors while the verbatim
// condition (including the literal "… | base64 -d | gunzip" hint) stays in
// Reasons untouched.
func TestDiagnoseDecodedErrorsOnDeepLeaf(t *testing.T) {
	hint := tofuHint(gzipB64(t, "Error: failed to create\n  on main.tf line 42"))
	leaf := node(2, "Workspace", "ws",
		[]Condition{cond("Synced", "False", "ReconcileError", hint)})
	mid := node(1, "XThing", "x", []Condition{cond("Ready", "False", "Waiting", "")}, leaf)
	root := node(0, "App", "a", []Condition{cond("Ready", "False", "Waiting", "")}, mid)

	d := Diagnose(context.Background(), &stubEvents{}, root, Stats{Nodes: 3}, false)

	top := d.Suspects[0]
	if top.Kind != "Workspace" {
		t.Fatalf("expected deepest Workspace as top suspect, got %s", top.Kind)
	}
	if len(top.DecodedErrors) != 1 {
		t.Fatalf("expected 1 decoded error on the leaf, got %d: %v", len(top.DecodedErrors), top.DecodedErrors)
	}
	if !strings.Contains(top.DecodedErrors[0], "Error: failed to create") ||
		!strings.Contains(top.DecodedErrors[0], "on main.tf line 42") {
		t.Errorf("decoded error missing actionable content: %q", top.DecodedErrors[0])
	}
	// The verbatim hint must survive untouched in Reasons (never-truncate).
	if !strings.Contains(strings.Join(top.Reasons, " | "), `base64 -d | gunzip`) {
		t.Errorf("verbatim blob hint must remain in Reasons, got %v", top.Reasons)
	}
}

// TestDiagnoseDecodedErrorsCrossSuspectDedup confirms a blob mirrored up the
// composite chain is surfaced once — on the first (deepest) suspect — not
// repeated on every affected suspect.
func TestDiagnoseDecodedErrorsCrossSuspectDedup(t *testing.T) {
	hint := tofuHint(gzipB64(t, "Error: shared root\n  on main.tf line 7"))
	leaf := node(2, "Workspace", "ws",
		[]Condition{cond("Synced", "False", "ReconcileError", hint)})
	mid := node(1, "XThing", "x",
		[]Condition{cond("Synced", "False", "ReconcileError", hint)}, leaf)
	root := node(0, "App", "a", []Condition{cond("Ready", "False", "Waiting", "")}, mid)

	d := Diagnose(context.Background(), &stubEvents{}, root, Stats{Nodes: 3}, false)

	if len(d.Suspects[0].DecodedErrors) != 1 {
		t.Fatalf("deepest suspect should carry the decoded error, got %v", d.Suspects[0].DecodedErrors)
	}
	if len(d.Suspects[1].DecodedErrors) != 0 {
		t.Errorf("identical blob should be deduped across suspects, got %v", d.Suspects[1].DecodedErrors)
	}
}

// TestDiagnoseNoBlobDecodedErrorsNil confirms a suspect without a blob carries
// no DecodedErrors (omitempty keeps non-TF output byte-identical).
func TestDiagnoseNoBlobDecodedErrorsNil(t *testing.T) {
	leaf := node(0, "Bucket", "b",
		[]Condition{cond("Synced", "False", "ReconcileError", "AccessDenied: invalid credentials")})
	d := Diagnose(context.Background(), &stubEvents{}, leaf, Stats{Nodes: 1}, false)
	if d.Suspects[0].DecodedErrors != nil {
		t.Errorf("expected nil DecodedErrors without a blob, got %v", d.Suspects[0].DecodedErrors)
	}
}

// TestDiagnoseLifecycle confirms suspects carry the deletionTimestamp and a
// derived lifecycle label distinguishing a wedged teardown from a blocked create.
func TestDiagnoseLifecycle(t *testing.T) {
	orig := nowFn
	nowFn = func() time.Time { return time.Date(2026, 6, 4, 0, 0, 0, 0, time.UTC) }
	defer func() { nowFn = orig }()

	// A leaf stuck Terminating for months.
	leaf := node(0, "Workspace", "ws", []Condition{cond("Ready", "False", "Deleting", "finalizer running")})
	leaf.deletionTime = "2026-01-15T00:00:00Z"
	d := Diagnose(context.Background(), &stubEvents{}, leaf, Stats{Nodes: 1}, false)
	if got := d.Suspects[0].DeletionTimestamp; got != "2026-01-15T00:00:00Z" {
		t.Errorf("deletionTimestamp not surfaced: %q", got)
	}
	if got := d.Suspects[0].Lifecycle; got != "Terminating (stuck 140d)" {
		t.Errorf("expected stuck-terminating label, got %q", got)
	}

	// A resource failing to come up (no deletionTimestamp).
	mr := node(0, "Bucket", "b", []Condition{cond("Synced", "False", "ReconcileError", "AccessDenied")})
	mr.creationTime = "2026-05-30T00:00:00Z"
	d = Diagnose(context.Background(), &stubEvents{}, mr, Stats{Nodes: 1}, false)
	if d.Suspects[0].DeletionTimestamp != "" {
		t.Errorf("non-deleting resource must not surface a deletionTimestamp: %q", d.Suspects[0].DeletionTimestamp)
	}
	if got := d.Suspects[0].Lifecycle; got != "Creating (blocked, 5d)" {
		t.Errorf("expected blocked-creating label, got %q", got)
	}
}

// TestDiagnoseDeletingReadySurfaced confirms a resource being deleted is
// surfaced (and not reported healthy) even when its conditions still say Ready —
// a finalizer can wedge a teardown while the Ready condition lags.
func TestDiagnoseDeletingReadySurfaced(t *testing.T) {
	orig := nowFn
	nowFn = func() time.Time { return time.Date(2026, 6, 4, 0, 0, 0, 0, time.UTC) }
	defer func() { nowFn = orig }()

	ready := node(0, "Workspace", "ws",
		[]Condition{cond("Ready", "True", "", ""), cond("Synced", "True", "", "")})
	ready.deletionTime = "2026-01-15T00:00:00Z"
	d := Diagnose(context.Background(), &stubEvents{}, ready, Stats{Nodes: 1}, false)

	if d.Healthy {
		t.Fatal("a resource being deleted must not be reported healthy")
	}
	if len(d.Suspects) != 1 || d.Suspects[0].Lifecycle != "Terminating (stuck 140d)" {
		t.Fatalf("expected the deleting Ready resource surfaced as Terminating, got %+v", d.Suspects)
	}
	// The headline counts it as terminating, not pending — matching its label.
	if !strings.Contains(d.Summary, "1 terminating") || strings.Contains(d.Summary, "1 pending") {
		t.Errorf("summary should count the deleting resource as terminating: %s", d.Summary)
	}
}

// TestDiagnoseTerminatingReadyDoesNotDisplaceDeeperFailure guards that a node
// surfaced only because it is being deleted (still Ready, no failing condition)
// never outranks a genuine deeper failure as the likely root cause.
func TestDiagnoseTerminatingReadyDoesNotDisplaceDeeperFailure(t *testing.T) {
	orig := nowFn
	nowFn = func() time.Time { return time.Date(2026, 6, 4, 0, 0, 0, 0, time.UTC) }
	defer func() { nowFn = orig }()

	child := node(1, "XThing", "x", []Condition{cond("Ready", "Unknown", "Provisioning", "still reconciling")})
	root := node(0, "App", "a",
		[]Condition{cond("Ready", "True", "", ""), cond("Synced", "True", "", "")}, child)
	root.deletionTime = "2026-01-15T00:00:00Z"

	d := Diagnose(context.Background(), &stubEvents{}, root, Stats{Nodes: 2}, false)

	if d.Suspects[0].Kind != "XThing" {
		t.Fatalf("a deleting-Ready root must not displace the deeper genuine failure; got root cause %s", d.Suspects[0].Kind)
	}
	if !strings.Contains(d.Summary, `XThing`) || !strings.Contains(d.Summary, "1 terminating") {
		t.Errorf("summary should name the real failing child and count the terminating root: %s", d.Summary)
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
