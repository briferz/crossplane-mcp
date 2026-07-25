package xp

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Readiness for native Kubernetes resources composed directly by a Crossplane v2
// XR. Classify alone cannot judge these: they carry none of Ready/Synced/Healthy,
// so it returns StateUnknown ("not assessed") — safe, but it means a Deployment
// dead past its progress deadline contributes nothing to a diagnosis. Worse, the
// one native kind that DOES carry a Ready condition reads it backwards: a Pod in
// phase Succeeded is Ready=False forever, which Classify calls StateBlocked, so
// every finished migration Pod became the top-ranked suspect permanently.
//
// The rules below are an exact (group, kind) table, never a polarity heuristic.
// That is deliberate: for core/Node (MemoryPressure, DiskPressure, PIDPressure)
// and policy/PodDisruptionBudget (DisruptionAllowed), False is the HEALTHY
// value, so any "native condition False -> Blocked" fold is wrong on contact.
// Kinds outside the table keep Classify's verdict untouched — this is strictly
// additive.
//
// Two boundaries the table holds to:
//   - Pending and Blocked stay distinct. A rollout in flight, a Job still
//     running, and a PVC awaiting its first consumer are Pending, never Blocked.
//     Conflating them is the failure this package spent a release removing.
//   - A Blocked or Pending verdict always carries a reason. A named suspect with
//     empty reasons is the original complaint that started this work.

// nativeVerdict is one rule's answer. ok=false means the rule declines and
// Classify's verdict stands.
type nativeVerdict struct {
	state   string
	reasons []string
	ok      bool
}

func declined() nativeVerdict { return nativeVerdict{} }

func verdict(state string, reasons ...string) nativeVerdict {
	return nativeVerdict{state: state, reasons: reasons, ok: true}
}

// ClassifyObject is Classify with access to the whole object, which the rules
// need: PVC and Pod readiness live in status.phase, StatefulSet readiness in
// replica counts — none of them conditions.
//
// Classify itself keeps its (apiVersion, conditions) signature. BuildUnhealthy
// deliberately stays on it: k8s.ProjectTriageFields drops spec and status.phase
// to bound memory when listing a whole estate, so object-aware classification is
// not available there — and list_unhealthy's category-scoped discovery means
// native kinds never reach it anyway.
func ClassifyObject(obj *unstructured.Unstructured) (Health, string, []string) {
	conds := Conditions(obj)
	h, state := Classify(obj.GetAPIVersion(), conds)
	if v := nativeReadiness(obj, conds); v.ok {
		return h, v.state, v.reasons
	}
	return h, state, nil
}

func nativeReadiness(obj *unstructured.Unstructured, conds []Condition) nativeVerdict {
	if obj == nil {
		return declined()
	}
	switch key := groupOf(obj.GetAPIVersion()) + "/" + obj.GetKind(); key {
	case "apps/Deployment":
		return deploymentReadiness(conds)
	case "batch/Job":
		return jobReadiness(obj, conds)
	case "/PersistentVolumeClaim":
		return pvcReadiness(obj)
	case "/Pod":
		return podReadiness(obj, conds)
	case "apps/StatefulSet":
		return statefulSetReadiness(obj)
	}
	return declined()
}

// deploymentReadiness keys on Progressing's REASON, not merely its status:
// spec.paused also yields Progressing=False (reason DeploymentPaused), and a
// paused Deployment is not a failure.
//
// There is deliberately no "only if Available != True" guard on the Blocked
// path. The canonical stuck rollout keeps Available=True the whole time — old
// pods keep serving under maxSurge while the new ReplicaSet crashloops past the
// deadline — so that guard would blind the rule to the exact failure it exists
// to catch.
func deploymentReadiness(conds []Condition) nativeVerdict {
	prog, hasProg := conditionByType(conds, "Progressing")
	avail, hasAvail := conditionByType(conds, "Available")
	repl, hasRepl := conditionByType(conds, "ReplicaFailure")

	var blocking []string
	if hasProg && prog.Status == "False" && prog.Reason == "ProgressDeadlineExceeded" {
		blocking = append(blocking, lineOr(prog, "Progressing",
			"Progressing: ProgressDeadlineExceeded — the rollout did not complete within spec.progressDeadlineSeconds"))
	}
	if hasRepl && repl.Status == "True" {
		blocking = append(blocking, lineOr(repl, "ReplicaFailure",
			"ReplicaFailure: the ReplicaSet could not create pods (quota, limits, or admission)"))
	}
	if len(blocking) > 0 {
		return verdict(StateBlocked, blocking...)
	}
	if hasAvail && avail.Status == "True" {
		return verdict(StateReady)
	}
	if hasAvail {
		return verdict(StatePending, lineOr(avail, "Available",
			"Available: not True — the rollout is in flight or minimum availability is unmet"))
	}
	// No Available condition at all: a freshly created Deployment with an empty
	// status. Nothing to assert.
	return declined()
}

// jobReadiness treats a completed Job as Ready. A Job that has run to completion
// is terminal success, not something to keep flagging.
func jobReadiness(obj *unstructured.Unstructured, conds []Condition) nativeVerdict {
	if c, ok := conditionByType(conds, "Complete"); ok && c.Status == "True" {
		return verdict(StateReady)
	}
	// Failed is a True-means-bad condition, so blockingMessages never sees it —
	// the reason has to come from here or the suspect would be unexplained.
	if c, ok := conditionByType(conds, "Failed"); ok && c.Status == "True" {
		return verdict(StateBlocked, lineOr(c, "Failed", "Failed: the Job did not complete successfully"))
	}
	succeeded, _, _ := unstructured.NestedInt64(obj.Object, "status", "succeeded")
	completions, found, _ := unstructured.NestedInt64(obj.Object, "spec", "completions")
	if !found {
		completions = 1 // the API default
	}
	reason := fmt.Sprintf("job has not completed: %d/%d succeeded (status.succeeded vs spec.completions)", succeeded, completions)
	if suspended, _, _ := unstructured.NestedBool(obj.Object, "spec", "suspend"); suspended {
		reason += "; spec.suspend=true, so it will not run until unsuspended"
	}
	return verdict(StatePending, reason)
}

// pvcReadiness reads status.phase, never conditions: Resizing and
// FileSystemResizePending are not readiness.
//
// Pending is never Blocked. A PVC on a WaitForFirstConsumer StorageClass sits
// Pending indefinitely by design, and the walk follows composed refs only — it
// does not fetch the StorageClass, so it cannot tell that case apart.
func pvcReadiness(obj *unstructured.Unstructured) nativeVerdict {
	phase, found, _ := unstructured.NestedString(obj.Object, "status", "phase")
	if !found {
		return declined()
	}
	switch phase {
	case "Bound":
		return verdict(StateReady)
	case "Lost":
		return verdict(StateBlocked,
			"phase: Lost — the PersistentVolume backing this claim no longer exists (status.phase)")
	case "Pending":
		return verdict(StatePending,
			"phase: Pending — not bound to a PersistentVolume (status.phase); "+
				"a WaitForFirstConsumer StorageClass stays here by design until a Pod schedules")
	}
	return declined()
}

// podReadiness is phase-only, and deliberately so. Judging a Running pod by its
// Ready condition would DOWNGRADE a crashlooping pod from Blocked to Pending,
// weakening an answer that is already correct. Declining on every other phase
// leaves Classify's verdict — including Ready=False -> Blocked with
// blockingMessages' verbatim text — exactly as it is.
func podReadiness(obj *unstructured.Unstructured, conds []Condition) nativeVerdict {
	phase, found, _ := unstructured.NestedString(obj.Object, "status", "phase")
	if !found {
		return declined()
	}
	switch phase {
	case "Succeeded":
		// The fix: a terminated pod is Ready=False/PodCompleted forever, which
		// Classify reads as Blocked — making every finished init or migration
		// Pod a permanent top-ranked suspect.
		return verdict(StateReady)
	case "Failed":
		reason, _, _ := unstructured.NestedString(obj.Object, "status", "reason")
		message, _, _ := unstructured.NestedString(obj.Object, "status", "message")
		switch {
		case reason != "" && message != "":
			return verdict(StateBlocked, "pod failed: "+reason+" — "+message)
		case reason != "":
			return verdict(StateBlocked, "pod failed: "+reason)
		}
		if c, ok := conditionByType(conds, TypeReady); ok {
			if line := conditionLine(TypeReady, c); line != "" {
				return verdict(StateBlocked, line)
			}
		}
		return verdict(StateBlocked, "phase: Failed (status.phase)")
	}
	return declined()
}

// statefulSetReadiness compares ready replicas against desired. It never returns
// Blocked: an STS at 0/3 because its PVC cannot bind and one 0/3 five seconds
// after creation are byte-identical from here, and calling the second a failure
// is precisely the not-ready-yet/failed conflation this package removed.
func statefulSetReadiness(obj *unstructured.Unstructured) nativeVerdict {
	if _, found, _ := unstructured.NestedMap(obj.Object, "status"); !found {
		return declined()
	}
	ready, _, _ := unstructured.NestedInt64(obj.Object, "status", "readyReplicas")
	want, found, _ := unstructured.NestedInt64(obj.Object, "spec", "replicas")
	if !found {
		want = 1 // the API default
	}
	if ready >= want {
		return verdict(StateReady) // includes a deliberate 0/0 scale-to-zero
	}
	return verdict(StatePending, fmt.Sprintf(
		"readiness: %d/%d replicas ready (status.readyReplicas vs spec.replicas)", ready, want))
}

func conditionByType(conds []Condition, typ string) (Condition, bool) {
	for _, c := range conds {
		if c.Type == typ {
			return c, true
		}
	}
	return Condition{}, false
}

// lineOr renders a condition, falling back to fixed text when it carries neither
// reason nor message — so a rule can never produce an unexplained verdict.
func lineOr(c Condition, label, fallback string) string {
	if line := conditionLine(label, c); line != "" {
		return line
	}
	return fallback
}
