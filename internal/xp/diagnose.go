package xp

import (
	"context"
	"fmt"
	"sort"

	"github.com/briferz/crossplane-mcp/internal/k8s"
)

// maxSuspects caps how many blocking resources we fetch events for and return.
const maxSuspects = 10

// EventFetcher supplies recent events for a resource by namespace+uid.
// *k8s.Client satisfies it; tests use a stub.
type EventFetcher interface {
	Events(ctx context.Context, namespace, uid string, limit int) ([]k8s.Event, error)
}

// Suspect is a resource flagged as a likely cause of a problem.
type Suspect struct {
	APIVersion string      `json:"apiVersion"`
	Kind       string      `json:"kind"`
	Name       string      `json:"name"`
	Namespace  string      `json:"namespace,omitempty"`
	Depth      int         `json:"depth"`
	Health     Health      `json:"health"`
	Reasons    []string    `json:"reasons,omitempty"`
	Events     []k8s.Event `json:"events,omitempty"`
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
	// reported as healthy.
	var suspects []*Node
	walk(tree, func(n *Node) {
		if n.State != StateReady {
			suspects = append(suspects, n)
		}
	})

	// Rank: Blocked (a failing condition) before Pending, then deepest first —
	// a leaf managed resource failing is a more actionable root cause than the
	// composite that merely propagates the problem upward.
	sort.SliceStable(suspects, func(i, j int) bool {
		if suspects[i].State != suspects[j].State {
			return suspects[i].State == StateBlocked
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

	var blocked, pending int
	for _, n := range suspects {
		if n.State == StateBlocked {
			blocked++
		} else {
			pending++
		}
	}

	for i, n := range suspects {
		if i >= maxSuspects {
			break
		}
		s := Suspect{
			APIVersion: n.APIVersion,
			Kind:       n.Kind,
			Name:       n.Name,
			Namespace:  n.Namespace,
			Depth:      n.depth,
			Health:     n.Health,
			Reasons:    blockingMessages(n.Conditions),
		}
		if ev != nil {
			if events, err := ev.Events(ctx, n.Namespace, n.uid, 5); err == nil {
				s.Events = events
			}
		}
		d.Suspects = append(d.Suspects, s)
	}

	root := suspects[0]
	d.Summary = fmt.Sprintf("%d blocking, %d pending resource(s); likely root cause: %s %q (%s, depth %d).",
		blocked, pending, root.Kind, root.Name, root.APIVersion, root.depth)
	if msgs := blockingMessages(root.Conditions); len(msgs) > 0 {
		d.Summary += " " + msgs[0]
	}
	return d
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
