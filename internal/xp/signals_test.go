package xp

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/briferz/crossplane-mcp/internal/k8s"
)

// Four signals used to drop silently at an output boundary: a fetch error never
// reached Suspect, Stats.Capped never reached the summary or the healthy
// verdict, a genuine condition could be outranked by a transport flake merely
// listed above it, and a wedged teardown with frozen Ready=True vanished from
// triage. Each is pinned here.

// unreachableNode mirrors exactly what fetchChild builds when a ref cannot be
// resolved or fetched: no uid, no conditions, no events — only Error.
func unreachableNode(depth int, kind, name, errMsg string) *Node {
	return &Node{
		APIVersion: "s3.aws.upbound.io/v1beta1",
		Kind:       kind,
		Name:       name,
		State:      StatePending,
		Error:      errMsg,
		depth:      depth,
	}
}

func TestDiagnoseUnreachableSuspectExplained(t *testing.T) {
	const forbidden = `buckets.s3.aws.upbound.io "b" is forbidden: User cannot get resource`
	child := unreachableNode(1, "Bucket", "b", forbidden)
	root := node(0, "App", "a", []Condition{cond("Ready", "Unknown", "Provisioning", "waiting")}, child)

	d := Diagnose(context.Background(), &stubEvents{}, root, Stats{Nodes: 2}, false)

	// Both are tier 1 (Pending), so the deeper unreachable child ranks first —
	// which is precisely why it must be able to explain itself.
	if len(d.Suspects) == 0 || d.Suspects[0].Name != "b" {
		t.Fatalf("expected the deeper unreachable node as top suspect, got %+v", d.Suspects)
	}
	top := d.Suspects[0]
	if top.Error != forbidden {
		t.Errorf("Suspect.Error = %q, want the fetch error", top.Error)
	}
	if len(top.Reasons) == 0 || !strings.HasPrefix(top.Reasons[0], unreachablePrefix) {
		t.Fatalf("an unreachable suspect must lead with why it could not be read, got %v", top.Reasons)
	}
	if !strings.Contains(top.Reasons[0], "is forbidden") {
		t.Errorf("lead reason should carry the RBAC text, got %q", top.Reasons[0])
	}
	// The headline must not name a suspect and then explain nothing.
	if !strings.Contains(d.Summary, "forbidden") {
		t.Errorf("summary should explain the unreachable root cause, got %q", d.Summary)
	}
}

func TestDiagnoseCappedTraversalNotHealthy(t *testing.T) {
	root := node(0, "App", "a", []Condition{cond("Ready", "True", "", ""), cond("Synced", "True", "", "")})

	capped := Diagnose(context.Background(), &stubEvents{}, root, Stats{Nodes: 200, Capped: true}, false)
	if capped.Healthy {
		t.Error("a capped walk cannot support a healthy verdict — the unvisited resources are where a failure would hide")
	}
	if len(capped.Suspects) != 0 {
		t.Errorf("no suspects were found, so none should be reported: %+v", capped.Suspects)
	}
	if !strings.Contains(capped.Summary, "capped") {
		t.Errorf("summary must say the traversal was incomplete, got %q", capped.Summary)
	}

	// The same tree, fully walked, is still healthy and says nothing about caps.
	full := Diagnose(context.Background(), &stubEvents{}, root, Stats{Nodes: 1}, false)
	if !full.Healthy {
		t.Errorf("an uncapped all-Ready tree must stay healthy, got %q", full.Summary)
	}
	if strings.Contains(full.Summary, "capped") {
		t.Errorf("an uncapped summary must not mention capping, got %q", full.Summary)
	}
}

func TestDiagnoseCappedCaveatOnSuspects(t *testing.T) {
	root := node(0, "App", "a", []Condition{cond("Ready", "False", "Boom", "it broke")})

	d := Diagnose(context.Background(), &stubEvents{}, root, Stats{Nodes: 200, Capped: true}, false)

	if !strings.Contains(d.Summary, "likely root cause") {
		t.Errorf("the root-cause headline must survive, got %q", d.Summary)
	}
	// The ranking ran over a partial tree, so the named cause may not be the real
	// one — the caveat has to be present alongside the headline, not instead.
	if !strings.Contains(d.Summary, "capped") {
		t.Errorf("a capped ranking must be caveated, got %q", d.Summary)
	}
}

func TestDiagnoseLeadsWithNonNoiseCondition(t *testing.T) {
	// Controller order puts the flake first; nothing sorts status.conditions.
	root := node(0, "Bucket", "b", []Condition{
		cond("Ready", "False", "", "connection refused"),
		cond("Synced", "False", "AccessDenied", "invalid credentials"),
	})

	d := Diagnose(context.Background(), &stubEvents{}, root, Stats{Nodes: 1}, false)

	if !strings.Contains(d.Summary, "AccessDenied") {
		t.Errorf("summary should lead with the genuine failure, not the flake: %q", d.Summary)
	}
	if len(d.Suspects) == 0 {
		t.Fatal("expected a suspect")
	}
	if !strings.Contains(d.Suspects[0].Reasons[0], "AccessDenied") {
		t.Errorf("Reasons[0] = %q, want the genuine failure first", d.Suspects[0].Reasons[0])
	}
	// Demoted, never dropped — hard rule 4.
	joined := strings.Join(d.Suspects[0].Reasons, "\n")
	if !strings.Contains(joined, "connection refused") {
		t.Errorf("the flake must still be reported, just not first: %v", d.Suspects[0].Reasons)
	}
}

func TestDiagnoseAllNoiseConditionStillLeads(t *testing.T) {
	// For provider-http / provider-kubernetes endpoints a transport error IS the
	// root cause. With no genuine condition to promote, the lead must not move.
	root := node(0, "Request", "r", []Condition{
		cond("Ready", "False", "", "connection refused"),
		cond("Synced", "False", "", "i/o timeout"),
	})

	d := Diagnose(context.Background(), &stubEvents{}, root, Stats{Nodes: 1}, false)

	if !strings.Contains(d.Summary, "connection refused") {
		t.Errorf("an all-noise suspect must keep its lead, got %q", d.Summary)
	}
}

func TestLeadFirst(t *testing.T) {
	const flake = "Ready: connection refused"
	const flake2 = "Synced: i/o timeout"
	const genuine = "Synced: AccessDenied — invalid credentials"

	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, nil},
		{"already genuine first", []string{genuine, flake}, []string{genuine, flake}},
		{"promotes the genuine one", []string{flake, genuine, flake2}, []string{genuine, flake, flake2}},
		{"all noise is left alone", []string{flake, flake2}, []string{flake, flake2}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := leadFirst(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("leadFirst() = %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("leadFirst() = %v, want %v", got, c.want)
				}
			}
		})
	}

	t.Run("does not mutate the caller's slice", func(t *testing.T) {
		in := []string{flake, genuine}
		_ = leadFirst(in)
		if in[0] != flake || in[1] != genuine {
			t.Errorf("input was mutated: %v", in)
		}
	})
}

func TestBuildUnhealthyTerminatingKept(t *testing.T) {
	deleting := func(it k8s.Listed) k8s.Listed {
		it.Object.SetDeletionTimestamp(&metav1.Time{Time: time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)})
		return it
	}

	in := []k8s.Listed{
		// The dangerous shape: reconciler died mid-teardown, conditions frozen.
		deleting(listed("composite", "ex.org/v1", "XApp", "ns", "frozen-ready",
			cnd("Ready", "True"), cnd("Synced", "True"))),
		deleting(listed("managed", "ex.org/v1", "Bucket", "ns", "deleting-broken", cnd("Ready", "False"))),
		listed("composite", "ex.org/v1", "XApp", "ns", "plain-ready", cnd("Ready", "True"), cnd("Synced", "True")),
	}

	res := BuildUnhealthy(in, UnhealthyParams{})

	names := map[string]string{}
	for _, it := range res.Items {
		names[it.Name] = it.DeletionTimestamp
	}
	if _, ok := names["frozen-ready"]; !ok {
		t.Error("a terminating resource whose conditions still say Ready must not vanish from triage")
	}
	if _, ok := names["deleting-broken"]; !ok {
		t.Error("a terminating, failing resource must be listed")
	}
	if _, ok := names["plain-ready"]; ok {
		t.Error("a genuinely healthy resource must still be filtered out")
	}
	if got := names["frozen-ready"]; got != "2026-01-15T00:00:00Z" {
		t.Errorf("DeletionTimestamp = %q, want 2026-01-15T00:00:00Z", got)
	}
	if res.Summary.Terminating != 2 {
		t.Errorf("Summary.Terminating = %d, want 2", res.Summary.Terminating)
	}
	if res.Summary.Ready != 1 {
		t.Errorf("Summary.Ready = %d, want 1 (terminating is disjoint)", res.Summary.Ready)
	}
	// The four buckets must still account for everything scanned.
	s := res.Summary
	if total := s.Blocked + s.Pending + s.Ready + s.Terminating; total != res.Scanned {
		t.Errorf("summary buckets sum to %d, want Scanned=%d", total, res.Scanned)
	}
}

// TestBuildTreeCappedOnlyWhenSomethingSkipped is the behavioural half of the
// Capped fix, driven through the real walk rather than a hand-built Stats.
func TestBuildTreeCappedOnlyWhenSomethingSkipped(t *testing.T) {
	ctx := context.Background()

	t.Run("exactly at the node budget, nothing skipped", func(t *testing.T) {
		// root + 199 childless leaves = exactly maxNodes visited, no ref skipped.
		cl, root := manyChildrenClient(t, maxNodes-1)
		_, stats := BuildTree(ctx, cl, root)
		if stats.Nodes != maxNodes {
			t.Fatalf("Nodes = %d, want %d", stats.Nodes, maxNodes)
		}
		if stats.Capped {
			t.Error("reaching the budget without skipping anything must not report Capped")
		}
	})

	t.Run("over the node budget", func(t *testing.T) {
		cl, root := manyChildrenClient(t, maxNodes+50)
		_, stats := BuildTree(ctx, cl, root)
		if stats.Nodes != maxNodes || !stats.Capped {
			t.Errorf("Nodes/Capped = %d/%v, want %d/true", stats.Nodes, stats.Capped, maxNodes)
		}
	})

	t.Run("exactly at max depth, final leaf childless", func(t *testing.T) {
		// The deepest node sits at maxDepth and has no children to skip.
		cl, root := chainClient(t, maxDepth+1)
		_, stats := BuildTree(ctx, cl, root)
		if stats.Nodes != maxDepth+1 {
			t.Fatalf("Nodes = %d, want %d", stats.Nodes, maxDepth+1)
		}
		if stats.Capped {
			t.Error("a final leaf at the depth limit skipped nothing; Capped must be false")
		}
	})

	t.Run("depth limit actually truncates", func(t *testing.T) {
		cl, root := chainClient(t, maxDepth+2)
		_, stats := BuildTree(ctx, cl, root)
		if stats.Nodes != maxDepth+1 || !stats.Capped {
			t.Errorf("Nodes/Capped = %d/%v, want %d/true", stats.Nodes, stats.Capped, maxDepth+1)
		}
	})
}

// A fifth signal, and the one only a real cluster surfaced: a suspect that names
// itself and then explains nothing. Both shapes below reach diagnose from real
// providers, and neither is reachable from the fixtures elsewhere in this
// package, which always write a reason or a message.

// TestSuspectWithBareConditionExplained covers a condition that is False but
// carries neither reason nor message. conditionLine renders that as "", so
// blockingMessages drops it entirely — leaving a Blocked, top-ranked suspect
// with an empty Reasons list.
//
// provider-nop constructs conditions with no message at all, and a reason only
// when the fixture supplies conditionReason; the e2e fixture supplies neither.
// So this is very likely the shape the kind + Crossplane tier hit — though the
// run itself could not prove it, since the composed object was not captured.
// That is an inference, not a measurement; the workflow now collects the object
// so a recurrence settles it.
func TestSuspectWithBareConditionExplained(t *testing.T) {
	root := node(0, "App", "a",
		[]Condition{cond("Synced", "True", "", ""), cond("Ready", "False", "", "")})

	d := Diagnose(context.Background(), &stubEvents{}, root, Stats{Nodes: 1}, false)

	if len(d.Suspects) == 0 {
		t.Fatal("a resource with Ready=False must be a suspect")
	}
	top := d.Suspects[0]
	if len(top.Reasons) == 0 {
		t.Fatal("a suspect must explain itself even when the condition carries no text")
	}
	if !strings.Contains(top.Reasons[0], "Ready") {
		t.Errorf("the explanation must name the failing condition, got %q", top.Reasons[0])
	}
	// The headline is the other output boundary the same gap reached.
	if !strings.Contains(d.Summary, "Ready") {
		t.Errorf("summary names a root cause, so it must say what is wrong with it: %q", d.Summary)
	}
	// Synced=True is not a problem and must not be reported as one.
	for _, r := range top.Reasons {
		if strings.Contains(r, "Synced") {
			t.Errorf("a True condition is not a reason: %q", r)
		}
	}
}

// TestSuspectWithNoConditionsExplained covers a managed resource the provider
// has not reconciled yet. Classify calls it Pending precisely so a fresh MR
// cannot read as healthy — but Pending with no conditions produced no reasons.
func TestSuspectWithNoConditionsExplained(t *testing.T) {
	root := node(0, "Bucket", "b", nil)

	d := Diagnose(context.Background(), &stubEvents{}, root, Stats{Nodes: 1}, false)

	if len(d.Suspects) == 0 {
		t.Fatal("a resource with no conditions is Pending, and Pending resources are suspects")
	}
	if len(d.Suspects[0].Reasons) == 0 {
		t.Fatal("a suspect must explain itself; 'no conditions yet' is an explanation")
	}
}

// TestTerminatingReadySuspectNotMisexplained is the guard on the Pending gate.
// A resource wedged mid-teardown with frozen Ready=True is a suspect, and
// claiming "nothing has written status" about it would be plainly false — its
// story is the lifecycle label and its finalizers.
func TestTerminatingReadySuspectNotMisexplained(t *testing.T) {
	root := node(0, "Bucket", "b",
		[]Condition{cond("Ready", "True", "", ""), cond("Synced", "True", "", "")})
	root.deletionTime = "2026-01-15T00:00:00Z"
	root.finalizers = []string{"finalizer.managedresource.crossplane.io"}

	d := Diagnose(context.Background(), &stubEvents{}, root, Stats{Nodes: 1}, false)

	if len(d.Suspects) == 0 {
		t.Fatal("a terminating resource must stay in triage even with Ready=True")
	}
	for _, r := range d.Suspects[0].Reasons {
		if strings.Contains(r, "no status conditions") {
			t.Errorf("this resource HAS conditions; the fallback must not claim otherwise: %q", r)
		}
	}
}

// TestBareStateDoesNotDisplaceRecurringEvent is the guard on WHERE the bare-state
// fallback is applied. The lines it produces state the ABSENCE of an
// explanation, so they must behave like absence for attribution: attribute
// overrides to a recurring composition event only when the lead is empty or a
// transport flake (issue #24 P1). Putting the fallback inside causeMessages —
// the obvious place — gives attribute a non-empty, non-noise lead, silently
// defeating that override and demoting a real recurring event with its full
// message to a bare "Recurring event: R (xN)" suffix.
//
// This is precisely the population the fallback targets (bare or absent
// conditions), so the two features collide by construction rather than by
// coincidence.
func TestBareStateDoesNotDisplaceRecurringEvent(t *testing.T) {
	const detail = "cannot compose resources: required value missing at spec.forProvider.region"
	ev := &stubEvents{events: []k8s.Event{
		{Reason: "ComposeResources", Type: "Warning", Count: 42, Message: detail, Last: "2026-01-01T00:00:00Z"},
	}}

	for _, tc := range []struct {
		name  string
		conds []Condition
	}{
		{"bare-condition", []Condition{cond("Ready", "False", "", "")}},
		{"no-conditions", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := node(0, "App", "a", tc.conds)

			d := Diagnose(context.Background(), ev, root, Stats{Nodes: 1}, false)

			if len(d.Suspects) == 0 {
				t.Fatal("expected a suspect")
			}
			// The event must still LEAD the reasons, not trail them.
			lead := d.Suspects[0].Reasons[0]
			if !strings.Contains(lead, "ComposeResources") {
				t.Errorf("the recurring composition event must still lead the reasons, got %q", lead)
			}
			// And the summary must carry its full message, not just reason+count.
			if !strings.Contains(d.Summary, detail) {
				t.Errorf("summary lost the recurring event's message: %q", d.Summary)
			}
		})
	}
}

// TestBareStateNotAppliedToUnassessedNative guards the state gate. A native kind
// outside native.go's table carries no Ready/Synced/Healthy, so Classify calls
// it StateUnknown — never assessed. Some native vocabularies are False when
// HEALTHY (a PodDisruptionBudget's DisruptionAllowed, a Node's MemoryPressure),
// so naming a False condition on one would invent a problem that does not exist.
func TestBareStateNotAppliedToUnassessedNative(t *testing.T) {
	root := nodeAPI(0, "policy/v1", "PodDisruptionBudget", "pdb",
		[]Condition{cond("DisruptionAllowed", "False", "", "")})
	root.deletionTime = "2026-01-15T00:00:00Z" // terminating, so it is a suspect

	d := Diagnose(context.Background(), &stubEvents{}, root, Stats{Nodes: 1}, false)

	if len(d.Suspects) == 0 {
		t.Fatal("a terminating resource must stay in triage")
	}
	for _, r := range d.Suspects[0].Reasons {
		if strings.Contains(r, "DisruptionAllowed") {
			t.Errorf("False is the HEALTHY value for this condition and the resource was "+
				"never assessed; reporting it as a cause invents a problem: %q", r)
		}
	}
}
