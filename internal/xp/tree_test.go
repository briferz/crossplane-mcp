package xp

import (
	"context"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func obj(spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
}

// refMap builds a ref entry as it appears in unstructured spec data.
func refMap(kind, name, namespace string) map[string]any {
	m := map[string]any{"apiVersion": "example.org/v1", "kind": kind, "name": name}
	if namespace != "" {
		m["namespace"] = namespace
	}
	return m
}

func wantRef(kind, name, namespace string) ref {
	return ref{apiVersion: "example.org/v1", kind: kind, name: name, namespace: namespace}
}

// TestChildRefs guards the version-specific ref locations and the parsing
// robustness. The v2 case (spec.crossplane.resourceRefs) is the regression that
// previously made diagnose find no children for namespaced XRs.
func TestChildRefs(t *testing.T) {
	tests := []struct {
		name string
		spec map[string]any
		want []ref
	}{
		{
			name: "v1 composite (spec.resourceRefs)",
			spec: map[string]any{"resourceRefs": []any{refMap("Bucket", "b1", "")}},
			want: []ref{wantRef("Bucket", "b1", "")},
		},
		{
			name: "v2 namespaced composite (spec.crossplane.resourceRefs)",
			spec: map[string]any{
				"crossplane": map[string]any{"resourceRefs": []any{
					refMap("Bucket", "b1", ""), refMap("Queue", "q1", ""),
				}},
			},
			want: []ref{wantRef("Bucket", "b1", ""), wantRef("Queue", "q1", "")},
		},
		{
			name: "v2 composed ref carries a namespace",
			spec: map[string]any{
				"crossplane": map[string]any{"resourceRefs": []any{refMap("Bucket", "b1", "team-a")}},
			},
			want: []ref{wantRef("Bucket", "b1", "team-a")},
		},
		{
			name: "v1 claim (spec.resourceRef)",
			spec: map[string]any{"resourceRef": refMap("XApp", "app", "")},
			want: []ref{wantRef("XApp", "app", "")},
		},
		{
			name: "both v1 and v2 ref locations present (v1 first)",
			spec: map[string]any{
				"resourceRefs": []any{refMap("V1Composed", "a", "")},
				"crossplane":   map[string]any{"resourceRefs": []any{refMap("V2Composed", "b", "")}},
			},
			want: []ref{wantRef("V1Composed", "a", ""), wantRef("V2Composed", "b", "")},
		},
		{
			name: "non-map slice entries are skipped",
			spec: map[string]any{"resourceRefs": []any{"not-a-map", refMap("Bucket", "b1", "")}},
			want: []ref{wantRef("Bucket", "b1", "")},
		},
		{
			name: "empty resourceRefs slice",
			spec: map[string]any{"resourceRefs": []any{}},
			want: nil,
		},
		{
			name: "no refs",
			spec: map[string]any{"message": "nothing composed"},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := childRefs(obj(tt.spec))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("childRefs()\n got:  %+v\n want: %+v", got, tt.want)
			}
		})
	}
}

func TestIsPaused(t *testing.T) {
	cases := []struct {
		name        string
		annotations map[string]any
		want        bool
	}{
		{"paused true", map[string]any{"crossplane.io/paused": "true"}, true},
		// Crossplane only honours the literal "true" — anything else reconciles.
		{"paused false", map[string]any{"crossplane.io/paused": "false"}, false},
		{"paused other value", map[string]any{"crossplane.io/paused": "yes"}, false},
		{"unrelated annotations", map[string]any{"example.org/team": "a"}, false},
		{"no annotations", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			meta := map[string]any{"name": "x"}
			if c.annotations != nil {
				meta["annotations"] = c.annotations
			}
			o := &unstructured.Unstructured{Object: map[string]any{"metadata": meta}}
			if got := IsPaused(o); got != c.want {
				t.Errorf("IsPaused() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestBuildTreeCapturesMetadataSignals guards the build() wiring: the pause
// annotation, finalizers, and deletionTimestamp land on the Node and project
// into the FlatNode. A childless root never touches the client, so nil is safe.
func TestBuildTreeCapturesMetadataSignals(t *testing.T) {
	root := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "example.org/v1",
		"kind":       "XApp",
		"metadata": map[string]any{
			"name":              "app",
			"annotations":       map[string]any{"crossplane.io/paused": "true"},
			"finalizers":        []any{"composite.apiextensions.crossplane.io"},
			"deletionTimestamp": "2026-01-15T00:00:00Z",
		},
	}}
	node, stats := BuildTree(context.Background(), nil, root)
	if stats.Nodes != 1 {
		t.Fatalf("expected 1 node, got %d", stats.Nodes)
	}
	if !node.paused {
		t.Error("expected paused captured from the annotation")
	}
	if len(node.finalizers) != 1 || node.finalizers[0] != "composite.apiextensions.crossplane.io" {
		t.Errorf("expected finalizers captured, got %v", node.finalizers)
	}
	if node.deletionTime != "2026-01-15T00:00:00Z" {
		t.Errorf("expected deletionTimestamp captured, got %q", node.deletionTime)
	}

	flat := node.Flatten()
	if len(flat) != 1 || !flat[0].Paused || flat[0].DeletionTimestamp == "" {
		t.Errorf("FlatNode should surface paused + deletionTimestamp, got %+v", flat)
	}
}
