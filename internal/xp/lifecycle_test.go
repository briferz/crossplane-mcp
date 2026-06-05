package xp

import (
	"testing"
	"time"
)

func TestLifecycleLabel(t *testing.T) {
	now := time.Date(2026, 6, 4, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		node *Node
		want string
	}{
		{"terminating-stuck-days", &Node{State: StateBlocked, deletionTime: "2026-01-15T00:00:00Z"}, "Terminating (stuck 140d)"},
		{"terminating-stuck-hours", &Node{State: StateBlocked, deletionTime: "2026-06-03T20:00:00Z"}, "Terminating (stuck 4h)"},
		{"terminating-recent", &Node{State: StateBlocked, deletionTime: "2026-06-03T23:55:00Z"}, "Terminating (5m)"},
		{"creating-blocked", &Node{State: StateBlocked, creationTime: "2026-05-30T00:00:00Z"}, "Creating (blocked, 5d)"},
		{"creating-pending", &Node{State: StatePending, creationTime: "2026-06-04T00:00:00Z"}, "Creating (pending, 0s)"},
		{"no-timestamps-blocked", &Node{State: StateBlocked}, "Creating (blocked)"},
		{"future-deletion-clamped", &Node{State: StateBlocked, deletionTime: "2027-01-01T00:00:00Z"}, "Terminating (0s)"},
		{"unparseable-deletion", &Node{State: StateBlocked, deletionTime: "not-a-timestamp"}, "Terminating (0s)"},
		// Deletion takes precedence over creation: a terminating resource is
		// always labelled Terminating regardless of how old it is.
		{"deleting-old-resource", &Node{State: StateBlocked, creationTime: "2020-01-01T00:00:00Z", deletionTime: "2026-06-03T23:00:00Z"}, "Terminating (stuck 1h)"},
	}
	for _, c := range cases {
		if got := lifecycleLabel(c.node, now); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestHumanizeAge(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{59 * time.Second, "59s"},
		{60 * time.Second, "1m"},
		{90 * time.Minute, "1h"},
		{23 * time.Hour, "23h"},
		{25 * time.Hour, "1d"},
		{140 * 24 * time.Hour, "140d"},
	}
	for _, c := range cases {
		if got := humanizeAge(c.d); got != c.want {
			t.Errorf("humanizeAge(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}
