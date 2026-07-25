package xp

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/briferz/crossplane-mcp/internal/k8s"
)

// k8s.ProjectTriageFields hardcodes BuildUnhealthy's read-set, and it cannot
// live in this package (k8s cannot import xp). That coupling is the whole risk:
// add a field to BuildUnhealthy's reads without adding it to the projector and
// every list_unhealthy row silently loses it — no compile error, no failing
// assertion anywhere else. This test is the pin.
//
// The terminating fixture is deliberately the first one: deletionTimestamp was
// the most recent addition to the read-set, and dropping it would make every
// wedged teardown read as a healthy resource again.

// bulky returns the object plus the fields the projector exists to discard, so a
// projector that trimmed nothing would still be visibly wrong in the size check.
func bulky(o *unstructured.Unstructured) *unstructured.Unstructured {
	md, _ := o.Object["metadata"].(map[string]any)
	if md == nil {
		md = map[string]any{}
		o.Object["metadata"] = md
	}
	md["managedFields"] = []any{
		map[string]any{"manager": "crossplane", "operation": "Apply", "fieldsV1": map[string]any{
			"f:spec": map[string]any{"f:forProvider": map[string]any{"f:region": map[string]any{}}},
		}},
	}
	ann, _ := md["annotations"].(map[string]any)
	if ann == nil {
		ann = map[string]any{}
		md["annotations"] = ann
	}
	ann["kubectl.kubernetes.io/last-applied-configuration"] = `{"apiVersion":"ex.org/v1","kind":"XApp","spec":{"a":"b"}}`
	o.Object["spec"] = map[string]any{"forProvider": map[string]any{"region": "eu-west-1"}}
	return o
}

func terminating(it k8s.Listed) k8s.Listed {
	md, _ := it.Object.Object["metadata"].(map[string]any)
	md["deletionTimestamp"] = "2026-01-15T00:00:00Z"
	return it
}

func pausedItem(it k8s.Listed) k8s.Listed {
	md, _ := it.Object.Object["metadata"].(map[string]any)
	ann, _ := md["annotations"].(map[string]any)
	if ann == nil {
		ann = map[string]any{}
		md["annotations"] = ann
	}
	ann[pausedAnnotation] = "true"
	return it
}

func parityFixtures() []k8s.Listed {
	mk := func(f func(k8s.Listed) k8s.Listed, category, apiVersion, kind, ns, name string, conds ...map[string]any) k8s.Listed {
		it := listed(category, apiVersion, kind, ns, name, conds...)
		bulky(&it.Object)
		if f != nil {
			it = f(it)
		}
		return it
	}
	return []k8s.Listed{
		// The regression this test exists for: Ready conditions frozen by a dead
		// reconciler, only deletionTimestamp reveals the wedged teardown.
		mk(terminating, "composite", "ex.org/v1", "XApp", "ns", "frozen-terminating",
			cnd("Ready", "True"), cnd("Synced", "True")),
		mk(terminating, "managed", "ex.org/v1", "Bucket", "ns", "deleting-broken", cnd("Ready", "False")),
		mk(pausedItem, "composite", "ex.org/v1", "XApp", "ns", "paused-blocked", cnd("Ready", "False")),
		mk(nil, "composite", "ex.org/v1", "XApp", "ns", "blocked", cnd("Ready", "False")),
		mk(nil, "claim", "ex.org/v1", "AppClaim", "ns2", "pending"), // no conditions
		mk(nil, "composite", "ex.org/v1", "XApp", "ns", "ready", cnd("Ready", "True"), cnd("Synced", "True")),
		// Native group with no Crossplane vocabulary -> StateUnknown (see
		// Classify); pins that apiVersion survives projection.
		mk(nil, "composite", "v1", "ConfigMap", "ns", "native"),
	}
}

func TestBuildUnhealthyProjectionParity(t *testing.T) {
	for _, p := range []UnhealthyParams{{}, {IncludeHealthy: true}} {
		raw := parityFixtures()
		projected := parityFixtures()
		for i := range projected {
			k8s.ProjectTriageFields(&projected[i].Object)
		}

		want := BuildUnhealthy(raw, p)
		got := BuildUnhealthy(projected, p)

		if !reflect.DeepEqual(want, got) {
			t.Errorf("IncludeHealthy=%v: projection changed the triage result.\n raw: %+v\nproj: %+v",
				p.IncludeHealthy, want, got)
		}
	}
}

// TestProjectTriageFieldsDropsBulk proves the projector actually sheds the bulk
// it exists to shed — parity alone would pass a no-op projector.
func TestProjectTriageFieldsDropsBulk(t *testing.T) {
	it := parityFixtures()[0]
	k8s.ProjectTriageFields(&it.Object)

	md, ok := it.Object.Object["metadata"].(map[string]any)
	if !ok {
		t.Fatal("metadata must survive projection")
	}
	if _, present := md["managedFields"]; present {
		t.Error("managedFields must be dropped")
	}
	ann, _ := md["annotations"].(map[string]any)
	if _, present := ann["kubectl.kubernetes.io/last-applied-configuration"]; present {
		t.Error("last-applied-configuration must be dropped — it can rival the object it annotates")
	}
	if _, present := it.Object.Object["spec"]; present {
		t.Error("spec must be dropped: triage rows carry no spec")
	}
	// ...while everything triage reads survives.
	if md["deletionTimestamp"] != "2026-01-15T00:00:00Z" {
		t.Errorf("deletionTimestamp must survive, got %v", md["deletionTimestamp"])
	}
	if it.Object.GetName() != "frozen-terminating" || it.Object.GetNamespace() != "ns" {
		t.Errorf("name/namespace must survive, got %s/%s", it.Object.GetNamespace(), it.Object.GetName())
	}
	if it.Object.GetAPIVersion() != "ex.org/v1" || it.Object.GetKind() != "XApp" {
		t.Errorf("apiVersion/kind must survive, got %s %s", it.Object.GetAPIVersion(), it.Object.GetKind())
	}
	if len(Conditions(&it.Object)) != 2 {
		t.Errorf("status.conditions must survive, got %v", Conditions(&it.Object))
	}
}

// TestProjectTriageFieldsKeepsPause pins the one annotation that is retained.
func TestProjectTriageFieldsKeepsPause(t *testing.T) {
	it := pausedItem(listed("composite", "ex.org/v1", "XApp", "ns", "p", cnd("Ready", "False")))
	bulky(&it.Object)
	k8s.ProjectTriageFields(&it.Object)

	if !IsPaused(&it.Object) {
		t.Error("the pause annotation must survive projection — a paused resource's conditions are stale")
	}
}
