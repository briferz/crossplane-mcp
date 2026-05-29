// Package xp holds Crossplane-aware diagnostic logic: reading status
// conditions, walking the Composite Resource → Managed Resource tree, and
// ranking the resources most likely to be the root cause of a problem.
package xp

import (
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
func Classify(cs []Condition) (Health, string) {
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
	// No health conditions at all → we can't assert readiness.
	if !sawTrue && state == StateReady {
		return h, StatePending
	}
	return h, state
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
