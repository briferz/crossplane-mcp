package xp

import (
	"strings"
	"testing"

	"github.com/briferz/crossplane-mcp/internal/k8s"
)

func TestIsTransportNoise(t *testing.T) {
	yes := []string{
		"Unexpected EOF",
		"http2: client connection force closed via ClientConn.Close",
		"connection reset by peer",
		"rpc error: code = Unavailable desc = error reading from server",
		"context deadline exceeded",
	}
	for _, m := range yes {
		if !isTransportNoise(m) {
			t.Errorf("isTransportNoise(%q) = false, want true", m)
		}
	}
	no := []string{
		"AccessDenied: invalid credentials",
		"status.conditions[1].lastTransitionTime: Required value",
		"",
	}
	for _, m := range no {
		if isTransportNoise(m) {
			t.Errorf("isTransportNoise(%q) = true, want false", m)
		}
	}
}

func TestIsCompositionError(t *testing.T) {
	yes := []string{
		"cannot compose resources: cannot apply composite resource status: ... Required value",
		"cannot apply composite resource status",
		"status.conditions[1].lastTransitionTime: Required value",
		`admission webhook "x" denied the request`,
	}
	for _, m := range yes {
		if !isCompositionError(m) {
			t.Errorf("isCompositionError(%q) = false, want true", m)
		}
	}
	no := []string{"unexpected EOF", "connection reset by peer", ""}
	for _, m := range no {
		if isCompositionError(m) {
			t.Errorf("isCompositionError(%q) = true, want false", m)
		}
	}
}

func TestTopRecurringEvent(t *testing.T) {
	any := func(k8s.Event) bool { return true }
	if _, ok := topRecurringEvent(nil, any); ok {
		t.Error("empty events should return ok=false")
	}
	events := []k8s.Event{
		{Reason: "A", Count: 3, Last: "2026-01-01T00:00:00Z"},
		{Reason: "B", Count: 50, Last: "2026-01-01T00:01:00Z"},
		{Reason: "C", Count: 50, Last: "2026-01-01T00:02:00Z"}, // ties B on count, but newer
		{Reason: "D", Count: 9, Last: "2026-01-01T00:03:00Z"},
	}
	if got, ok := topRecurringEvent(events, any); !ok || got.Reason != "C" {
		t.Errorf("expected highest-count, newest-tie event C, got %+v (ok=%v)", got, ok)
	}

	// Full tie (equal Count AND equal Last): keep the earliest in slice order.
	tie := []k8s.Event{
		{Reason: "first", Count: 50, Last: "2026-01-01T00:00:00Z"},
		{Reason: "second", Count: 50, Last: "2026-01-01T00:00:00Z"},
	}
	if got, _ := topRecurringEvent(tie, any); got.Reason != "first" {
		t.Errorf("full tie should keep first by slice order, got %s", got.Reason)
	}

	// The predicate is honoured: a higher-count non-matching event does not mask
	// a lower-count matching one.
	matchOnly := func(e k8s.Event) bool { return e.Reason == "match" }
	mixed := []k8s.Event{
		{Reason: "noise", Count: 9999, Last: "2026-01-01T00:05:00Z"},
		{Reason: "match", Count: 12, Last: "2026-01-01T00:04:00Z"},
	}
	if got, ok := topRecurringEvent(mixed, matchOnly); !ok || got.Reason != "match" {
		t.Errorf("expected the matching event despite a higher-count non-match, got %+v (ok=%v)", got, ok)
	}
}

// TestQualifyingEventNotMaskedByHigherCountNoise is the unit-level guard for the
// issue #24 review finding: the gate must pick the highest-count *qualifying*
// event, not the global max, so noisier events cannot suppress the real one.
func TestQualifyingEventNotMaskedByHigherCountNoise(t *testing.T) {
	comp := k8s.Event{Type: "Warning", Reason: "ComposeResources", Count: 2666,
		Message: "cannot compose resources: required value"}

	withNormal := []k8s.Event{
		{Type: "Normal", Reason: "Pulled", Count: 9999, Last: "2026-01-01T01:00:00Z", Message: "pulled"},
		comp,
	}
	if e, ok := qualifyingEvent(withNormal); !ok || e.Reason != "ComposeResources" {
		t.Errorf("higher-count Normal must not mask the composition Warning, got %+v (ok=%v)", e, ok)
	}

	withWarning := []k8s.Event{
		{Type: "Warning", Reason: "CannotConnect", Count: 5000, Last: "2026-01-01T01:00:00Z", Message: "connection refused"},
		comp,
	}
	if e, ok := qualifyingEvent(withWarning); !ok || e.Reason != "ComposeResources" {
		t.Errorf("higher-count non-composition Warning must not mask it, got %+v (ok=%v)", e, ok)
	}
}

// TestReasonsWithEventDedup covers the duplicate-suppression guard: if the event
// line is already present among the condition messages it is not added again.
func TestReasonsWithEventDedup(t *testing.T) {
	e := k8s.Event{Type: "Warning", Reason: "ComposeResources", Count: 2666,
		Message: "cannot compose resources: required value"}
	got := reasonsWithEvent([]string{eventLine(e)}, []k8s.Event{e})
	if len(got) != 1 {
		t.Errorf("event line already present must not be duplicated, got %v", got)
	}
}

func TestEventLine(t *testing.T) {
	tests := []struct {
		name string
		e    k8s.Event
		want string
	}{
		{"full", k8s.Event{Reason: "ComposeResources", Count: 2666, Message: "boom"}, "event: ComposeResources (x2666) — boom"},
		{"count one omits multiplier", k8s.Event{Reason: "Pulled", Count: 1, Message: "ok"}, "event: Pulled — ok"},
		{"no message", k8s.Event{Reason: "Scheduled", Count: 5}, "event: Scheduled (x5)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := eventLine(tt.e); got != tt.want {
				t.Errorf("eventLine() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTrimEvents(t *testing.T) {
	mk := func(reason string, last string) k8s.Event {
		return k8s.Event{Type: "Normal", Reason: reason, Count: 1, Last: last}
	}

	// Fewer than the cap: returned unchanged.
	few := []k8s.Event{mk("a", "t1"), mk("b", "t2")}
	if got := trimEvents(few); len(got) != 2 {
		t.Errorf("short list should be unchanged, got %d", len(got))
	}

	// More than the cap, no qualifying event: keep only the newest displayEvents.
	many := []k8s.Event{
		mk("e1", "2026-01-01T00:01:00Z"), mk("e2", "2026-01-01T00:02:00Z"),
		mk("e3", "2026-01-01T00:03:00Z"), mk("e4", "2026-01-01T00:04:00Z"),
		mk("e5", "2026-01-01T00:05:00Z"), mk("e6", "2026-01-01T00:06:00Z"),
		mk("e7", "2026-01-01T00:07:00Z"), mk("e8", "2026-01-01T00:08:00Z"),
	}
	if got := trimEvents(many); len(got) != displayEvents {
		t.Errorf("expected newest %d events, got %d", displayEvents, len(got))
	}

	// Qualifying event already inside the newest window: not duplicated.
	q := k8s.Event{Type: "Warning", Reason: "ComposeResources", Count: 50,
		Last: "2026-01-01T09:00:00Z", Message: "cannot compose resources: required value"}
	withQ := []k8s.Event{
		mk("a", "t1"), mk("b", "t2"), mk("c", "t3"), mk("d", "t4"), mk("e", "t5"), q,
	}
	count := 0
	for _, e := range trimEvents(withQ) {
		if e.Reason == "ComposeResources" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("qualifying event in window must appear exactly once, got %d", count)
	}
}

func TestAttribute(t *testing.T) {
	comp := func(count int64) k8s.Event {
		return k8s.Event{Type: "Warning", Reason: "ComposeResources", Count: count,
			Message: "cannot compose resources: required value"}
	}
	flake := []string{"Synced: ReconcileError — rpc error: code = Unavailable"}
	genuine := []string{"Synced: ReconcileError — AccessDenied: invalid credentials"}

	tests := []struct {
		name      string
		condMsgs  []string
		events    []k8s.Event
		wantEvent bool
		wantHas   string
	}{
		{"flake + recurring composition -> event", flake, []k8s.Event{comp(2666)}, true, "event: ComposeResources (x2666)"},
		{"genuine condition wins", genuine, []k8s.Event{comp(2666)}, false, "AccessDenied"},
		{"below threshold keeps condition", flake, []k8s.Event{comp(9)}, false, "rpc error"},
		{"boundary 10 promotes", flake, []k8s.Event{comp(10)}, true, "event: ComposeResources (x10)"},
		{"normal event not promoted", flake, []k8s.Event{{Type: "Normal", Reason: "Pulled", Count: 9999, Message: "cannot compose resources"}}, false, "rpc error"},
		{"non-composition warning not promoted", flake, []k8s.Event{{Type: "Warning", Reason: "Pulled", Count: 50, Message: "image pulled"}}, false, "rpc error"},
		{"no condition + qualifying event -> event", nil, []k8s.Event{comp(2666)}, true, "event: ComposeResources"},
		{"no events keeps condition", genuine, nil, false, "AccessDenied"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, fromEvent := attribute(tt.condMsgs, tt.events)
			if fromEvent != tt.wantEvent {
				t.Errorf("fromEvent = %v, want %v (msg=%q)", fromEvent, tt.wantEvent, msg)
			}
			if !strings.Contains(msg, tt.wantHas) {
				t.Errorf("msg %q should contain %q", msg, tt.wantHas)
			}
		})
	}
}
