package xp

import (
	"context"
	"fmt"
	"sort"

	"github.com/briferz/crossplane-mcp/internal/k8s"
)

// maxSuspects caps how many blocking resources we fetch events for and return.
const maxSuspects = 10

// allEvents requests every event for a suspect from the fetcher (no cap). The
// per-object set is small — the API server aggregates events by reason+message,
// and the fetcher already lists them all regardless of limit (the limit only
// trims the returned slice), so this adds no API cost. Fetching the full set is
// what lets attribution find a recurring high-count composition event even when
// a churn of newer one-shot transport flakes would otherwise evict it from a
// capped, newest-first window. The response is trimmed separately (trimEvents)
// to stay token-light.
const allEvents = 0

// EventFetcher supplies recent events for a resource by namespace+uid.
// *k8s.Client satisfies it; tests use a stub.
type EventFetcher interface {
	Events(ctx context.Context, namespace, uid string, limit int) ([]k8s.Event, error)
}

// Suspect is a resource flagged as a likely cause of a problem.
type Suspect struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace,omitempty"`
	Depth      int    `json:"depth"`
	Health     Health `json:"health"`
	// DeletionTimestamp is set (RFC3339) when the resource is being deleted, and
	// Lifecycle is a derived label — "Terminating (stuck 140d)" for a wedged
	// teardown vs "Creating (blocked, 5d)" for one failing to come up — so an
	// agent can tell "unblock the finalizer" from "fix the create" and see how
	// long it has been stuck.
	DeletionTimestamp string   `json:"deletionTimestamp,omitempty"`
	Lifecycle         string   `json:"lifecycle,omitempty"`
	Reasons           []string `json:"reasons,omitempty"`
	// DecodedErrors carries the actionable provider error decoded from a
	// provider-terraform/OpenTofu "… | base64 -d | gunzip" hint embedded in a
	// condition (or recurring event) message: the base64+gzip blob is decoded
	// and reduced to its Error:/Summary: lines (or the trailing non-log lines
	// when no such marker is present), kept whole — except that a single
	// pathological line is capped with an explicit marker pointing back to the
	// shell hint, to stay token-light. The decoded text is surfaced in the live
	// response as-is — identifiers like account IDs/ARNs are intentionally kept
	// (often the actionable detail); in the --log-file the recorder's best-effort
	// secret scrub applies but does not guarantee removal, so marking values
	// sensitive in the Terraform/OpenTofu config remains the source of truth. The
	// server never reads Secret objects. Empty when no hint is present or the
	// blob cannot be decoded.
	DecodedErrors []string    `json:"decodedErrors,omitempty"`
	Events        []k8s.Event `json:"events,omitempty"`
}

// Diagnosis is the result of analysing a Crossplane tree.
type Diagnosis struct {
	Summary  string     `json:"summary"`
	Healthy  bool       `json:"healthy"`
	Suspects []Suspect  `json:"suspects,omitempty"`
	Stats    Stats      `json:"stats"`
	Tree     []FlatNode `json:"tree,omitempty"`
}

// Diagnose walks the tree, finds blocking resources, and ranks them so the
// deepest (most likely root-cause) resource comes first — the key value over a
// flat trace, which leaves the user to spot the blocker themselves. Events are
// fetched only for the top suspects, keeping the response token-light.
func Diagnose(ctx context.Context, ev EventFetcher, tree *Node, stats Stats, includeTree bool) *Diagnosis {
	// Collect every non-Ready node, not just Blocked ones: a resource stuck
	// Pending (Unknown/absent conditions) is still a problem and must not be
	// reported as healthy. Also collect any resource being deleted even if its
	// conditions still report Ready — a finalizer can wedge a teardown while the
	// Ready condition lags, and a stuck termination must never be called healthy.
	var suspects []*Node
	walk(tree, func(n *Node) {
		if n.State != StateReady || n.deletionTime != "" {
			suspects = append(suspects, n)
		}
	})

	// Rank by how actionable a suspect is, then deepest first — a leaf managed
	// resource failing is a more actionable root cause than the composite that
	// merely propagates the problem upward. A node surfaced only because it is
	// being deleted while its conditions still report Ready has no failing
	// condition to explain, so it ranks below any genuine Blocked/Pending failure
	// and never displaces the real root cause.
	sort.SliceStable(suspects, func(i, j int) bool {
		if ti, tj := rankTier(suspects[i]), rankTier(suspects[j]); ti != tj {
			return ti < tj
		}
		return suspects[i].depth > suspects[j].depth
	})

	d := &Diagnosis{Healthy: len(suspects) == 0, Stats: stats}
	if includeTree {
		d.Tree = tree.Flatten()
	}

	if d.Healthy {
		d.Summary = fmt.Sprintf("All %d resource(s) in the tree are Ready; no blocking or pending conditions found.", stats.Nodes)
		return d
	}

	// Count by lifecycle so the headline matches each suspect's Lifecycle label: a
	// resource being deleted is "terminating", not a pending create.
	var blocked, pending, terminating int
	for _, n := range suspects {
		switch {
		case n.deletionTime != "":
			terminating++
		case n.State == StateBlocked:
			blocked++
		default:
			pending++
		}
	}

	var rootEvents []k8s.Event // the root suspect's full (untrimmed) events
	// seen dedups byte-identical decoded provider errors across suspects in this
	// call: the same recurring TF blob mirrors up the composite chain, so it is
	// surfaced once (on the first/deepest suspect that carries it).
	seen := map[string]bool{}
	for i, n := range suspects {
		if i >= maxSuspects {
			break
		}
		s := Suspect{
			APIVersion:        n.APIVersion,
			Kind:              n.Kind,
			Name:              n.Name,
			Namespace:         n.Namespace,
			Depth:             n.depth,
			Health:            n.Health,
			DeletionTimestamp: n.deletionTime,
			Lifecycle:         lifecycleLabel(n, nowFn()),
		}
		var events []k8s.Event
		if ev != nil {
			if got, err := ev.Events(ctx, n.Namespace, n.uid, allEvents); err == nil {
				events = got
			}
		}
		if i == 0 {
			rootEvents = events // the root's full events, for the summary below
		}
		// Build reasons from the full fetched set so a recurring composition
		// event is found even when a burst of one-shot events fills the newest
		// window — and, when the condition is just a transport flake, lead the
		// reasons (see reasonsWithEvent). Surface only a trimmed set to stay
		// token-light.
		condMsgs := blockingMessages(n.Conditions)
		s.Reasons = reasonsWithEvent(condMsgs, events)
		s.Events = trimEvents(events)
		// Additively surface any decoded provider-terraform/OpenTofu error blob.
		// The verbatim condition message stays untouched in Reasons (the literal
		// "… | base64 -d | gunzip" hint included); this only adds the decoded,
		// actionable form alongside it.
		s.DecodedErrors = decodeTFErrors(condMsgs, events, seen)
		d.Suspects = append(d.Suspects, s)
	}

	root := suspects[0]
	counts := fmt.Sprintf("%d blocking, %d pending", blocked, pending)
	if terminating > 0 {
		counts += fmt.Sprintf(", %d terminating", terminating)
	}
	d.Summary = fmt.Sprintf("%s resource(s); likely root cause: %s %q (%s, depth %d).",
		counts, root.Kind, root.Name, root.APIVersion, root.depth)

	// Attribute over the root's full events (not the trimmed s.Events) so the
	// summary's cause matches the reasons even when the qualifying event fell
	// outside the surfaced window. attribute prefers a recurring composition
	// event over a transport-flake condition; otherwise it returns the condition
	// message, preserving the previous behaviour exactly.
	msg, fromEvent := attribute(blockingMessages(root.Conditions), rootEvents)
	if msg != "" {
		d.Summary += " " + msg
	}
	// When a genuine condition led but a hot loop is also recurring, point at it
	// so the agent never misses the persistent failure behind the symptom.
	if e, ok := qualifyingEvent(rootEvents); ok && !fromEvent {
		d.Summary += fmt.Sprintf(" Recurring event: %s (x%d).", e.Reason, e.Count)
	}
	return d
}

// rankTier orders a suspect by how actionable it is for root-cause ranking: a
// failing condition (Blocked) first, then Pending, then a node surfaced only
// because it is being deleted while still reporting Ready (StateReady) — that
// last one has no failing condition to explain and must not displace a genuine
// failure as the likely root cause.
func rankTier(n *Node) int {
	switch n.State {
	case StateBlocked:
		return 0
	case StatePending:
		return 1
	default: // StateReady — only collected because it is being deleted
		return 2
	}
}

func walk(n *Node, fn func(*Node)) {
	if n == nil {
		return
	}
	fn(n)
	for _, c := range n.Children {
		walk(c, fn)
	}
}
