// Package xp holds Crossplane-aware diagnostic logic: reading status
// conditions, walking the Composite Resource → Managed Resource tree, and
// ranking the resources most likely to be the root cause of a problem.
package xp

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Condition is a pruned Kubernetes/Crossplane status condition.
type Condition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
}

// The condition types Crossplane uses to express health. Ready: availability
// (almost everything). Synced: reconciliation with the external API (managed
// resources). Healthy: package revisions (Provider/Function/Configuration).
const (
	TypeReady   = "Ready"
	TypeSynced  = "Synced"
	TypeHealthy = "Healthy"
)

// Resource state, ordered by severity.
const (
	StateReady   = "Ready"   // all present health conditions are True
	StatePending = "Pending" // some condition is Unknown/absent but none False
	StateBlocked = "Blocked" // a health condition is False
	// StateUnknown: the object does not speak Crossplane's health vocabulary at
	// all — a native Kubernetes resource composed by a v2 XR (ConfigMap, Service,
	// PVC; or a Deployment, whose vocabulary is Available/Progressing). Its
	// readiness is not knowable from Ready/Synced/Healthy, so it is neither Ready
	// nor a failure: "not assessed", not "fine".
	StateUnknown = "Unknown"
)

// Conditions extracts the status conditions from an unstructured object.
func Conditions(obj *unstructured.Unstructured) []Condition {
	if obj == nil {
		return nil
	}
	raw, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if !found || err != nil {
		return nil
	}
	out := make([]Condition, 0, len(raw))
	for _, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, Condition{
			Type:               str(m, "type"),
			Status:             str(m, "status"),
			Reason:             str(m, "reason"),
			Message:            str(m, "message"),
			LastTransitionTime: str(m, "lastTransitionTime"),
		})
	}
	return out
}

// byType returns the status of a condition ("True"/"False"/"Unknown"), or ""
// if the condition is absent.
func byType(cs []Condition, t string) string {
	for _, c := range cs {
		if c.Type == t {
			return c.Status
		}
	}
	return ""
}

// Health summarises the three Crossplane health conditions.
type Health struct {
	Ready   string `json:"ready,omitempty"`
	Synced  string `json:"synced,omitempty"`
	Healthy string `json:"healthy,omitempty"`
}

// Classify reduces a condition set to a Health summary and an overall state.
//
// apiVersion is consulted ONLY in the no-vocabulary branch — when none of
// Ready/Synced/Healthy is present at all — to tell a native Kubernetes resource
// (which will never carry them) from a Crossplane resource that has not reported
// them yet. Anything that does carry the vocabulary classifies on its conditions
// alone, whatever its group. An empty apiVersion means unknown provenance and
// stays conservatively Pending.
func Classify(apiVersion string, cs []Condition) (Health, string) {
	h := Health{
		Ready:   byType(cs, TypeReady),
		Synced:  byType(cs, TypeSynced),
		Healthy: byType(cs, TypeHealthy),
	}

	present := []string{h.Ready, h.Synced, h.Healthy}
	state := StateReady
	sawTrue := false
	for _, s := range present {
		switch s {
		case "False":
			return h, StateBlocked
		case "Unknown":
			state = StatePending
		case "True":
			sawTrue = true
		}
	}
	// No health conditions at all → we can't assert readiness. Whether that means
	// "not yet" or "never will" depends on what kind of object this is.
	if !sawTrue && state == StateReady {
		if nativeAPIGroup(apiVersion) {
			return h, StateUnknown
		}
		return h, StatePending
	}
	return h, state
}

// builtinGroups are the non-core Kubernetes API groups that do not end in
// ".k8s.io". Every other built-in group is either the core group ("") or carries
// that suffix, which Kubernetes reserves — so this short exact-match list plus
// the suffix check covers the built-ins without a prefix rule that would swallow
// look-alike third-party groups.
var builtinGroups = map[string]bool{
	"apps":        true,
	"batch":       true,
	"autoscaling": true,
	"policy":      true,
	"extensions":  true,
}

// nativeAPIGroup reports whether apiVersion names a built-in Kubernetes API
// group — an object that will never carry Crossplane's health conditions.
//
// Matching is exact for the listed groups and suffix-based for ".k8s.io", both
// deliberately: "apps.example.org" is a provider group, not the "apps" built-in,
// and the leading dot in the suffix keeps "cluster.x-k8s.io" out.
func nativeAPIGroup(apiVersion string) bool {
	if apiVersion == "" {
		return false
	}
	g := groupOf(apiVersion)
	return g == "" || builtinGroups[g] || strings.HasSuffix(g, ".k8s.io")
}

// blockingMessages returns the reason/message text of any condition that is
// False (or Unknown with detail) — the lines a human would read first. Unknown
// conditions are included so resources stuck Pending still report why.
func blockingMessages(cs []Condition) []string {
	var msgs []string
	for _, c := range cs {
		if c.Status != "False" && c.Status != "Unknown" {
			continue
		}
		if c.Status == "Unknown" && c.Message == "" && c.Reason == "" {
			continue
		}
		label := c.Type
		if c.Status == "Unknown" {
			label += " [Unknown]"
		}
		switch {
		case c.Message != "" && c.Reason != "":
			msgs = append(msgs, label+": "+c.Reason+" — "+c.Message)
		case c.Message != "":
			msgs = append(msgs, label+": "+c.Message)
		case c.Reason != "":
			msgs = append(msgs, label+": "+c.Reason)
		}
	}
	return msgs
}

func str(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}
