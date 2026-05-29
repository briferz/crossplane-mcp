package xp

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func obj(spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
}

func resourceRef(kind, name string) map[string]any {
	return map[string]any{"apiVersion": "example.org/v1", "kind": kind, "name": name}
}

// TestChildRefs guards the version-specific ref locations: v1 XRs use top-level
// spec.resourceRefs, v2 namespaced XRs use spec.crossplane.resourceRefs, and v1
// Claims use the single spec.resourceRef. The v2 case is the regression that
// previously made diagnose find no children for namespaced XRs.
func TestChildRefs(t *testing.T) {
	tests := []struct {
		name      string
		spec      map[string]any
		wantKinds []string
	}{
		{
			name:      "v1 composite (spec.resourceRefs)",
			spec:      map[string]any{"resourceRefs": []any{resourceRef("Bucket", "b1")}},
			wantKinds: []string{"Bucket"},
		},
		{
			name: "v2 namespaced composite (spec.crossplane.resourceRefs)",
			spec: map[string]any{
				"crossplane": map[string]any{"resourceRefs": []any{
					resourceRef("Bucket", "b1"), resourceRef("Queue", "q1"),
				}},
			},
			wantKinds: []string{"Bucket", "Queue"},
		},
		{
			name:      "v1 claim (spec.resourceRef)",
			spec:      map[string]any{"resourceRef": resourceRef("XApp", "app")},
			wantKinds: []string{"XApp"},
		},
		{
			name: "both v1 and v2 ref locations present",
			spec: map[string]any{
				"resourceRefs": []any{resourceRef("V1Composed", "a")},
				"crossplane":   map[string]any{"resourceRefs": []any{resourceRef("V2Composed", "b")}},
			},
			wantKinds: []string{"V1Composed", "V2Composed"},
		},
		{
			name:      "no refs",
			spec:      map[string]any{"message": "nothing composed"},
			wantKinds: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := childRefs(obj(tt.spec))
			if len(got) != len(tt.wantKinds) {
				t.Fatalf("got %d refs, want %d: %+v", len(got), len(tt.wantKinds), got)
			}
			gotKinds := map[string]bool{}
			for _, r := range got {
				gotKinds[r.kind] = true
			}
			for _, k := range tt.wantKinds {
				if !gotKinds[k] {
					t.Errorf("missing expected ref kind %q in %+v", k, got)
				}
			}
		})
	}
}
