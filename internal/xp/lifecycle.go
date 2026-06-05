package xp

import (
	"fmt"
	"time"
)

// nowFn is the clock used to age deletion/creation timestamps into the lifecycle
// label. A package-level var so tests can pin it; production uses the wall clock.
var nowFn = time.Now

// stuckThreshold is how long a resource must have been Terminating before the
// lifecycle label calls it "stuck": a normal finalizer completes in seconds, so
// a delete still lingering past this is a wedged-teardown signal worth flagging.
const stuckThreshold = 15 * time.Minute

// lifecycleLabel describes where a non-Ready node is in its lifecycle, turning
// the bare "Ready: Deleting" into "Terminating (stuck 140d)" — so an agent can
// route a wedged teardown (unblock the finalizer) differently from a resource
// failing to come up ("Creating (blocked, 5d)"). The age is what distinguishes a
// months-stuck delete from a transient in-progress one. now is injected for
// deterministic tests; callers pass nowFn().
func lifecycleLabel(n *Node, now time.Time) string {
	if n == nil {
		return ""
	}
	if n.deletionTime != "" {
		d := ageSince(n.deletionTime, now)
		if d >= stuckThreshold {
			return "Terminating (stuck " + humanizeAge(d) + ")"
		}
		return "Terminating (" + humanizeAge(d) + ")"
	}
	// Not deleting: a Ready node has no "Creating" story (it isn't a suspect
	// today, but guard so a future caller can't get a bogus label), and a node
	// that only failed to resolve/fetch lets its Error field tell the story.
	if n.State == StateReady || (n.creationTime == "" && n.Error != "") {
		return ""
	}
	phase := "blocked"
	if n.State == StatePending {
		phase = "pending"
	}
	// Age the "blocked" duration from when the resource entered its failing state
	// (a condition's LastTransitionTime), not from creation — otherwise a
	// long-lived resource that broke today reads as a months-old create failure.
	// Fall back to creation time when no transition time is known.
	since := blockedSince(n)
	if since == "" {
		since = n.creationTime
	}
	if since != "" {
		return "Creating (" + phase + ", " + humanizeAge(ageSince(since, now)) + ")"
	}
	return "Creating (" + phase + ")"
}

// blockedSince returns the timestamp the resource entered its current failing
// state — the LastTransitionTime of its Ready (else Synced, else Healthy, else
// any) False/Unknown condition. "" when no such timestamp is recorded.
func blockedSince(n *Node) string {
	for _, typ := range []string{TypeReady, TypeSynced, TypeHealthy} {
		for _, c := range n.Conditions {
			if c.Type == typ && (c.Status == "False" || c.Status == "Unknown") && c.LastTransitionTime != "" {
				return c.LastTransitionTime
			}
		}
	}
	// No canonical health condition carried a time; take the most recent
	// transition among any failing condition (RFC3339 sorts lexicographically).
	var latest string
	for _, c := range n.Conditions {
		if (c.Status == "False" || c.Status == "Unknown") && c.LastTransitionTime > latest {
			latest = c.LastTransitionTime
		}
	}
	return latest
}

// ageSince returns now minus the RFC3339 timestamp ts, clamped at 0 — an
// unparseable or future timestamp (e.g. clock skew) yields 0, never a negative.
func ageSince(ts string, now time.Time) time.Duration {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return 0
	}
	if d := now.Sub(t); d > 0 {
		return d
	}
	return 0
}

// humanizeAge renders a duration as its largest single unit (45s, 12m, 3h, 140d)
// — coarse on purpose, the magnitude is what matters for "how long stuck".
func humanizeAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
}
