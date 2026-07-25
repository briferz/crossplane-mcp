package xp

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// obj builders for native kinds. Conditions go in status.conditions; everything
// else (phase, replica counts, spec.suspend) is set directly, since those are
// exactly the fields Classify cannot see.

func nativeObj(apiVersion, kind string, status map[string]any, spec map[string]any, conds ...map[string]any) *unstructured.Unstructured {
	o := map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   map[string]any{"name": strings.ToLower(kind) + "-1", "namespace": "ns"},
	}
	if status == nil {
		status = map[string]any{}
	}
	if len(conds) > 0 {
		cs := make([]any, len(conds))
		for i, c := range conds {
			cs[i] = c
		}
		status["conditions"] = cs
	}
	if len(status) > 0 {
		o["status"] = status
	}
	if spec != nil {
		o["spec"] = spec
	}
	return &unstructured.Unstructured{Object: o}
}

func c(typ, status, reason, msg string) map[string]any {
	m := map[string]any{"type": typ, "status": status}
	if reason != "" {
		m["reason"] = reason
	}
	if msg != "" {
		m["message"] = msg
	}
	return m
}

// TestPodSucceededNoLongerBlocked is the headline: this was a live false
// positive, not a missing feature. A finished init/migration Pod is
// Ready=False/PodCompleted forever, which Classify reads as Blocked — tier 0,
// the top suspect rank — so it was named "likely root cause" permanently.
func TestPodSucceededNoLongerBlocked(t *testing.T) {
	pod := nativeObj("v1", "Pod", map[string]any{"phase": "Succeeded"}, nil,
		c("Ready", "False", "PodCompleted", ""),
		c("ContainersReady", "False", "PodCompleted", ""))

	_, state, _ := ClassifyObject(pod)
	if state != StateReady {
		t.Errorf("a Succeeded Pod must be Ready, got %q — it would rank as the top suspect", state)
	}
}

// TestPodRunningNotReadyStillBlocked guards the other direction: the fix must
// not weaken a verdict that is already correct. A crashlooping pod is Blocked,
// and must not be softened to Pending.
func TestPodRunningNotReadyStillBlocked(t *testing.T) {
	pod := nativeObj("v1", "Pod", map[string]any{"phase": "Running"}, nil,
		c("Ready", "False", "ContainersNotReady", "containers with unready status: [api]"))

	_, state, _ := ClassifyObject(pod)
	if state != StateBlocked {
		t.Errorf("a crashlooping Pod must stay Blocked, got %q", state)
	}
}

func TestNativeReadinessRules(t *testing.T) {
	cases := []struct {
		name      string
		obj       *unstructured.Unstructured
		want      string
		wantWhy   string // substring the reason must contain ("" = no reason required)
		wantEmpty bool   // reasons must be empty (Ready verdicts)
	}{
		// --- apps/Deployment ---
		{
			name: "deployment past its progress deadline is blocked",
			obj: nativeObj("apps/v1", "Deployment", nil, nil,
				c("Available", "True", "MinimumReplicasAvailable", ""),
				c("Progressing", "False", "ProgressDeadlineExceeded", `ReplicaSet "api-7d" has timed out progressing.`)),
			want:    StateBlocked,
			wantWhy: "ProgressDeadlineExceeded",
		},
		{
			// The reason this rule has no "only if Available != True" guard: the
			// canonical stuck rollout keeps Available=True the whole time.
			name: "stuck rollout with old pods still serving is still blocked",
			obj: nativeObj("apps/v1", "Deployment", nil, nil,
				c("Available", "True", "", ""),
				c("Progressing", "False", "ProgressDeadlineExceeded", "timed out")),
			want:    StateBlocked,
			wantWhy: "timed out",
		},
		{
			// TRAP: spec.paused also yields Progressing=False. Keying on status
			// alone would call every paused Deployment broken.
			name: "paused deployment is not blocked",
			obj: nativeObj("apps/v1", "Deployment", nil, map[string]any{"paused": true},
				c("Available", "True", "", ""),
				c("Progressing", "False", "DeploymentPaused", "Deployment is paused")),
			want:      StateReady,
			wantEmpty: true,
		},
		{
			// TRAP: mid-rollout is not a failure.
			name: "deployment mid-rollout is pending, never blocked",
			obj: nativeObj("apps/v1", "Deployment", nil, nil,
				c("Available", "False", "MinimumReplicasUnavailable", "Deployment does not have minimum availability."),
				c("Progressing", "True", "ReplicaSetUpdated", "")),
			want:    StatePending,
			wantWhy: "minimum availability",
		},
		{
			name: "healthy deployment is ready",
			obj: nativeObj("apps/v1", "Deployment", nil, nil,
				c("Available", "True", "MinimumReplicasAvailable", "")),
			want:      StateReady,
			wantEmpty: true,
		},
		{
			// ReplicaFailure is True-means-bad, so blockingMessages never sees
			// it — the reason has to come from the rule or the suspect is
			// unexplained.
			name: "replica failure is blocked and explained",
			obj: nativeObj("apps/v1", "Deployment", nil, nil,
				c("Available", "True", "", ""),
				c("ReplicaFailure", "True", "FailedCreate", "exceeded quota: pods=10")),
			want:    StateBlocked,
			wantWhy: "exceeded quota",
		},

		// --- batch/Job ---
		{
			// TRAP: a completed Job is terminal success, not something to flag.
			name: "completed job is ready",
			obj: nativeObj("batch/v1", "Job", map[string]any{"succeeded": int64(1)}, nil,
				c("Complete", "True", "", "")),
			want:      StateReady,
			wantEmpty: true,
		},
		{
			name: "failed job is blocked and explained",
			obj: nativeObj("batch/v1", "Job", nil, nil,
				c("Failed", "True", "BackoffLimitExceeded", "Job has reached the specified backoff limit")),
			want:    StateBlocked,
			wantWhy: "BackoffLimitExceeded",
		},
		{
			name:    "running job is pending with progress",
			obj:     nativeObj("batch/v1", "Job", map[string]any{"succeeded": int64(2)}, map[string]any{"completions": int64(5)}),
			want:    StatePending,
			wantWhy: "2/5 succeeded",
		},
		{
			name:    "suspended job says so",
			obj:     nativeObj("batch/v1", "Job", nil, map[string]any{"suspend": true}),
			want:    StatePending,
			wantWhy: "spec.suspend=true",
		},

		// --- core/PersistentVolumeClaim ---
		{
			name: "bound pvc is ready",
			obj:  nativeObj("v1", "PersistentVolumeClaim", map[string]any{"phase": "Bound"}, nil),
			want: StateReady, wantEmpty: true,
		},
		{
			// TRAP: WaitForFirstConsumer sits Pending by design, possibly for
			// days. It must never read as Blocked.
			name:    "pending pvc is pending and names WaitForFirstConsumer",
			obj:     nativeObj("v1", "PersistentVolumeClaim", map[string]any{"phase": "Pending"}, nil),
			want:    StatePending,
			wantWhy: "WaitForFirstConsumer",
		},
		{
			name:    "lost pvc is blocked",
			obj:     nativeObj("v1", "PersistentVolumeClaim", map[string]any{"phase": "Lost"}, nil),
			want:    StateBlocked,
			wantWhy: "no longer exists",
		},

		// --- core/Pod ---
		{
			name:    "failed pod is blocked with its status reason",
			obj:     nativeObj("v1", "Pod", map[string]any{"phase": "Failed", "reason": "Evicted", "message": "The node was low on resource: memory."}, nil),
			want:    StateBlocked,
			wantWhy: "Evicted",
		},

		// --- apps/StatefulSet ---
		{
			name: "statefulset at full readiness is ready",
			obj: nativeObj("apps/v1", "StatefulSet", map[string]any{"readyReplicas": int64(3)},
				map[string]any{"replicas": int64(3)}),
			want: StateReady, wantEmpty: true,
		},
		{
			// Never Blocked: 0/3 five seconds old and 0/3 on an unbindable PVC
			// are byte-identical from here.
			name: "statefulset short of replicas is pending, never blocked",
			obj: nativeObj("apps/v1", "StatefulSet", map[string]any{"readyReplicas": int64(1)},
				map[string]any{"replicas": int64(3)}),
			want:    StatePending,
			wantWhy: "1/3 replicas ready",
		},
		{
			// A deliberate scale-to-zero: readyReplicas is omitempty so it is
			// absent, but the object has a real status (observedGeneration etc.).
			name: "statefulset scaled to zero is ready",
			obj: nativeObj("apps/v1", "StatefulSet", map[string]any{"observedGeneration": int64(4), "replicas": int64(0)},
				map[string]any{"replicas": int64(0)}),
			want: StateReady, wantEmpty: true,
		},
		{
			// A brand-new StatefulSet with no status yet must NOT be judged:
			// asserting readiness from absent data is what StateUnknown exists
			// to prevent.
			name: "statefulset with no status yet is not assessed",
			obj:  nativeObj("apps/v1", "StatefulSet", nil, map[string]any{"replicas": int64(3)}),
			want: StateUnknown, wantEmpty: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, state, reasons := ClassifyObject(tc.obj)
			if state != tc.want {
				t.Errorf("state = %q, want %q (reasons: %v)", state, tc.want, reasons)
			}
			if tc.wantEmpty && len(reasons) > 0 {
				t.Errorf("a Ready verdict should carry no reasons, got %v", reasons)
			}
			if tc.wantWhy != "" && !strings.Contains(strings.Join(reasons, "\n"), tc.wantWhy) {
				t.Errorf("reasons %v should mention %q", reasons, tc.wantWhy)
			}
		})
	}
}

// TestNativeRulesNeverReachInvertedConditions is the trap the audit named: for
// these kinds False is the HEALTHY value, so any polarity heuristic would call
// a healthy cluster broken. The table is keyed on exact (group, kind), so they
// are unreachable by construction — this pins that.
func TestNativeRulesNeverReachInvertedConditions(t *testing.T) {
	cases := []struct {
		name string
		obj  *unstructured.Unstructured
	}{
		{"node with no pressure", nativeObj("v1", "Node", nil, nil,
			c("MemoryPressure", "False", "KubeletHasSufficientMemory", ""),
			c("DiskPressure", "False", "KubeletHasNoDiskPressure", ""),
			c("PIDPressure", "False", "KubeletHasSufficientPID", ""),
			c("Ready", "True", "KubeletReady", ""))},
		{"pdb at minimum availability", nativeObj("policy/v1", "PodDisruptionBudget", nil, nil,
			c("DisruptionAllowed", "False", "InsufficientPods", ""))},
		{"hpa quiescent", nativeObj("autoscaling/v2", "HorizontalPodAutoscaler", nil, nil,
			c("ScalingActive", "False", "ScalingDisabled", ""))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, reasons := ClassifyObject(tc.obj)
			if len(reasons) != 0 {
				t.Errorf("no native rule may fire for this kind, got reasons %v", reasons)
			}
		})
	}
	// A Node with Ready=True classifies on its own vocabulary, unchanged.
	_, state, _ := ClassifyObject(cases[0].obj)
	if state != StateReady {
		t.Errorf("a healthy Node should read Ready via its own Ready condition, got %q", state)
	}
}

// TestUntabledKindsStayUnknown pins that the change is strictly additive.
func TestUntabledKindsStayUnknown(t *testing.T) {
	for _, o := range []*unstructured.Unstructured{
		nativeObj("v1", "ConfigMap", nil, nil),
		nativeObj("v1", "Secret", nil, nil),
		nativeObj("v1", "Service", map[string]any{"loadBalancer": map[string]any{}}, map[string]any{"type": "LoadBalancer"}),
		nativeObj("networking.k8s.io/v1", "Ingress", map[string]any{"loadBalancer": map[string]any{}}, nil),
	} {
		_, state, reasons := ClassifyObject(o)
		if state != StateUnknown || len(reasons) != 0 {
			t.Errorf("%s should stay %s with no reasons, got %s %v", o.GetKind(), StateUnknown, state, reasons)
		}
	}
}

// TestNativeVerdictsAlwaysExplained is the invariant that keeps this change from
// recreating the complaint it descends from: a named suspect with empty reasons.
func TestNativeVerdictsAlwaysExplained(t *testing.T) {
	objs := []*unstructured.Unstructured{
		nativeObj("apps/v1", "Deployment", nil, nil, c("Progressing", "False", "ProgressDeadlineExceeded", "")),
		nativeObj("apps/v1", "Deployment", nil, nil, c("Available", "False", "", "")),
		nativeObj("batch/v1", "Job", nil, nil, c("Failed", "True", "", "")),
		nativeObj("batch/v1", "Job", nil, nil),
		nativeObj("v1", "PersistentVolumeClaim", map[string]any{"phase": "Pending"}, nil),
		nativeObj("v1", "PersistentVolumeClaim", map[string]any{"phase": "Lost"}, nil),
		nativeObj("v1", "Pod", map[string]any{"phase": "Failed"}, nil),
		nativeObj("apps/v1", "StatefulSet", map[string]any{}, map[string]any{"replicas": int64(3)}),
	}
	for _, o := range objs {
		_, state, reasons := ClassifyObject(o)
		if state != StateBlocked && state != StatePending {
			continue
		}
		if len(reasons) == 0 {
			t.Errorf("%s reached %s with no explanation — a named suspect must say why", o.GetKind(), state)
			continue
		}
		for _, r := range reasons {
			if strings.TrimSpace(r) == "" {
				t.Errorf("%s produced an empty reason line", o.GetKind())
			}
		}
	}
}

// TestDiagnoseSurfacesNativeFailure is the end-to-end payoff: a broken composed
// Deployment is now a suspect, with an explanation, instead of silence.
func TestDiagnoseSurfacesNativeFailure(t *testing.T) {
	dep := nodeAPI(1, "apps/v1", "Deployment", "api", nil)
	// Reproduce what build() would have stored for a deadline-exceeded rollout.
	_, state, reasons := ClassifyObject(nativeObj("apps/v1", "Deployment", nil, nil,
		c("Available", "True", "", ""),
		c("Progressing", "False", "ProgressDeadlineExceeded", "ReplicaSet has timed out progressing")))
	dep.State, dep.nativeReasons = state, reasons

	root := node(0, "XApp", "app", []Condition{cond("Ready", "True", "", ""), cond("Synced", "True", "", "")}, dep)
	d := Diagnose(context.Background(), &stubEvents{}, root, Stats{Nodes: 2}, false)

	if d.Healthy {
		t.Fatalf("an XR whose composed Deployment is past its deadline is not healthy: %s", d.Summary)
	}
	if len(d.Suspects) != 1 || d.Suspects[0].Kind != "Deployment" {
		t.Fatalf("expected the Deployment as the suspect, got %+v", d.Suspects)
	}
	if len(d.Suspects[0].Reasons) == 0 {
		t.Fatal("the suspect must explain itself — empty reasons is the original complaint")
	}
	if !strings.Contains(strings.Join(d.Suspects[0].Reasons, "\n"), "timed out") {
		t.Errorf("reasons should carry the rollout failure, got %v", d.Suspects[0].Reasons)
	}
}
