package xp

import (
	"fmt"
	"strings"

	"github.com/briferz/crossplane-mcp/internal/k8s"
)

// recurrenceThreshold is the event Count at or above which a recurring Warning
// is treated as a persistent failure rather than transient churn. A genuine
// transport flake fires a handful of times; a stuck reconcile/compose loop
// re-fires every reconcile and climbs into the hundreds or thousands (the
// real-world incident this guards against showed ~2666). Ten cleanly separates
// normal startup retries from a hot loop while staying an order of magnitude
// below the observed count. Inclusive: Count >= recurrenceThreshold qualifies.
const recurrenceThreshold int64 = 10

// transportNoise are lowercase substrings of a condition message that indicate
// a transient network/transport failure from whichever reconcile happened to be
// in flight when the snapshot was taken — not a resource-specific root cause.
// A condition matching one of these is demoted in favour of a qualifying
// recurring event; it is never hidden (it stays in the suspect's Reasons).
var transportNoise = []string{
	"unexpected eof",
	"client connection force closed",
	"clientconn.close",
	"connection reset by peer",
	"connection refused",
	"broken pipe",
	"i/o timeout",
	"tls handshake timeout",
	"no route to host",
	"context deadline exceeded",
	"context canceled",
	"rpc error: code = unavailable",
	"transport is closing",
	"http2: client connection",
}

// compositionErrorParts are lowercase substrings that mark a message as a
// persistent Crossplane composition/validation failure — the signal worth
// promoting over a transport flake. A couple ("required value", "denied the
// request") are intentionally broad; that is safe because a match only ever
// promotes a real, verbatim Warning that already cleared the recurrence and
// Warning gate (see isQualifyingEvent) — it never fabricates or truncates a
// cause, and only ever runs when the leading condition is itself a flake.
var compositionErrorParts = []string{
	"cannot compose resources",
	"cannot apply composite resource",
	"cannot render composed",
	"required value",
	"admission webhook",
	"denied the request",
}

func containsAny(s string, parts []string) bool {
	ls := strings.ToLower(s)
	for _, p := range parts {
		if strings.Contains(ls, p) {
			return true
		}
	}
	return false
}

// isTransportNoise reports whether a condition message is a transient transport
// flake rather than a resource-specific cause.
func isTransportNoise(msg string) bool { return containsAny(msg, transportNoise) }

// isCompositionError reports whether a message looks like a persistent
// Crossplane reconcile/compose/validation failure.
func isCompositionError(msg string) bool { return containsAny(msg, compositionErrorParts) }

// topRecurringEvent returns the highest-Count event satisfying keep, breaking
// ties by the most recent timestamp and then by slice order so the result is
// deterministic. ok is false when no event matches. Scanning for the
// highest-count *matching* event — rather than picking the global max and then
// testing it — means a noisier non-matching event (a chatty Normal, or an
// unrelated high-count Warning) cannot mask a genuine recurring failure.
func topRecurringEvent(events []k8s.Event, keep func(k8s.Event) bool) (k8s.Event, bool) {
	var best k8s.Event
	found := false
	for _, e := range events {
		if !keep(e) {
			continue
		}
		if !found || e.Count > best.Count || (e.Count == best.Count && e.Last > best.Last) {
			best = e
			found = true
		}
	}
	return best, found
}

// isQualifyingEvent reports whether an event is a persistent Crossplane
// composition/validation failure worth surfacing over a transport flake: a
// Warning that recurs at least recurrenceThreshold times with a
// composition-error message.
func isQualifyingEvent(e k8s.Event) bool {
	return e.Type == "Warning" && e.Count >= recurrenceThreshold &&
		isCompositionError(e.Reason+" "+e.Message)
}

// qualifyingEvent returns the highest-count qualifying event (see
// isQualifyingEvent), or ok=false if none qualify. This is the single gate
// shared by attribution, the per-suspect reasons, and the summary. It picks the
// highest-count event *among those that qualify*, so a higher-count Normal or
// unrelated Warning cannot suppress a real recurring composition failure.
func qualifyingEvent(events []k8s.Event) (k8s.Event, bool) {
	return topRecurringEvent(events, isQualifyingEvent)
}

// eventLine renders an event as one cause line shared by Reasons and the
// summary so they read identically. The message is never truncated (hard rule:
// full condition/event detail is the whole point over a flat trace).
func eventLine(e k8s.Event) string {
	s := "event: " + e.Reason
	if e.Count > 1 {
		s += fmt.Sprintf(" (x%d)", e.Count)
	}
	if e.Message != "" {
		s += " — " + e.Message
	}
	return s
}

// attribute decides the single cause line for a suspect. It defaults to the
// first blocking-condition message — byte-identical to the previous behaviour —
// and overrides to the recurring event only when that condition is absent or a
// transport flake AND a qualifying composition event exists. fromEvent reports
// whether the event won, so callers can order Reasons accordingly.
func attribute(condMsgs []string, events []k8s.Event) (msg string, fromEvent bool) {
	lead := ""
	if len(condMsgs) > 0 {
		lead = condMsgs[0]
	}
	if e, ok := qualifyingEvent(events); ok && (lead == "" || isTransportNoise(lead)) {
		return eventLine(e), true
	}
	return lead, false
}

// reasonsWithEvent augments condition-derived reasons with the dominant
// recurring composition event (if any): prepended when it overrides an
// absent/transport-flake condition so it reads first, appended otherwise so a
// genuine condition message stays primary. Without a qualifying event the
// reasons are returned unchanged.
func reasonsWithEvent(condMsgs []string, events []k8s.Event) []string {
	e, ok := qualifyingEvent(events)
	if !ok {
		return condMsgs
	}
	line := eventLine(e)
	for _, m := range condMsgs {
		if m == line { // already present; don't duplicate
			return condMsgs
		}
	}
	if _, fromEvent := attribute(condMsgs, events); fromEvent {
		return append([]string{line}, condMsgs...)
	}
	return append(condMsgs, line)
}

// displayEvents caps how many events are surfaced per suspect in the response.
// The fetch window (eventLimit) is deliberately wider so attribution can find a
// recurring event that a burst of one-shot events would otherwise evict; the
// response itself stays token-light by surfacing only the newest few — plus the
// qualifying event, which is the root-cause signal and must never be dropped.
const displayEvents = 5

// trimEvents reduces a suspect's events to a token-light set for output: the
// newest displayEvents, plus the qualifying recurring event when it would
// otherwise fall outside that window. events must be ordered oldest→newest (as
// the events fetcher returns them).
func trimEvents(events []k8s.Event) []k8s.Event {
	if len(events) <= displayEvents {
		return events
	}
	kept := events[len(events)-displayEvents:]
	q, ok := qualifyingEvent(events)
	if !ok {
		return kept
	}
	for _, e := range kept {
		if e == q {
			return kept
		}
	}
	return append([]k8s.Event{q}, kept...)
}
