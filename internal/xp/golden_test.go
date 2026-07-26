package xp

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Embedded rather than read from disk: the fixtures are then independent of the
// test's working directory, and there is no runtime file read taking a computed
// path for gosec to object to.
//
//go:embed testdata/golden/*.json
var goldenFS embed.FS

// Golden objects captured verbatim from a real cluster by the kind + Crossplane
// tier (see .github/workflows/e2e.yml, "export golden fixtures").
//
// Why they exist: every other fixture in this package was written by someone
// reasoning about what a provider emits, and that gap has already cost one
// shipped bug. Real provider-nop writes
//
//	Ready=False reason="" message=""
//
// beside a fully-populated Synced=True reason="ReconcileSuccess". Nobody writing
// a fixture by hand produces that asymmetry — they give every condition a
// reason, because most conditions have one. conditionLine renders the bare one
// as "", blockingMessages dropped it, and the top-ranked suspect came back with
// nothing to say. No unit test in this package could have caught it, because
// they all shared the author's blind spot.
//
// These tests are cheap and run on every PR, with no cluster. They cannot detect
// upstream CHANGING its shape — that is the live tier's job — but they stop the
// fast suite from being systematically more optimistic than reality.

const goldenDir = "testdata/golden"

// loadGolden reads every captured JSON file and flattens Lists into individual
// objects, so a caller sees one slice of real resources regardless of whether
// the capture used `get <name>` or `get <kind> -A`.
func loadGolden(t *testing.T) []*unstructured.Unstructured {
	t.Helper()
	paths, err := fs.Glob(goldenFS, goldenDir+"/*.json")
	if err != nil {
		t.Fatalf("globbing golden fixtures: %v", err)
	}
	if len(paths) == 0 {
		t.Fatalf("no golden fixtures in %s — they are captured by the kind + Crossplane "+
			"tier's 'export golden fixtures' step; an empty directory means this test "+
			"silently asserts nothing", goldenDir)
	}

	var out []*unstructured.Unstructured
	for _, p := range paths {
		raw, err := goldenFS.ReadFile(p)
		if err != nil {
			t.Fatalf("reading %s: %v", p, err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("parsing %s: %v", p, err)
		}
		obj := &unstructured.Unstructured{Object: m}
		if obj.IsList() {
			items, err := obj.ToList()
			if err != nil {
				t.Fatalf("listing %s: %v", p, err)
			}
			for i := range items.Items {
				out = append(out, &items.Items[i])
			}
			continue
		}
		// Non-resource captures (kubectl version) have no kind/name; skip them.
		if obj.GetKind() == "" || obj.GetName() == "" {
			continue
		}
		out = append(out, obj)
	}
	return out
}

// TestGoldenSuspectsAlwaysExplained is the invariant, checked against real data:
// a resource that diagnose names as a suspect must say why. This is the exact
// assertion that failed against a live cluster while the entire hand-written
// unit suite passed.
func TestGoldenSuspectsAlwaysExplained(t *testing.T) {
	objs := loadGolden(t)
	var checked int

	for _, obj := range objs {
		conds := Conditions(obj)
		h, state := Classify(obj.GetAPIVersion(), conds)
		n := &Node{
			APIVersion: obj.GetAPIVersion(),
			Kind:       obj.GetKind(),
			Name:       obj.GetName(),
			Namespace:  obj.GetNamespace(),
			State:      state,
			Health:     h,
			Conditions: conds,
		}

		d := Diagnose(context.Background(), &stubEvents{}, n, Stats{Nodes: 1}, false)
		if len(d.Suspects) == 0 {
			continue
		}
		checked++
		s := d.Suspects[0]
		if len(s.Reasons) == 0 {
			t.Errorf("%s/%s (%s) is a suspect with no explanation — the shipped defect. "+
				"conditions as captured: %s",
				s.Kind, s.Name, state, renderConds(conds))
		}
		// The headline must not name a root cause and then say nothing either.
		if d.Summary == "" {
			t.Errorf("%s/%s produced an empty summary", s.Kind, s.Name)
		}
	}

	if checked == 0 {
		t.Fatal("no captured object classified as a suspect, so this test asserted nothing — " +
			"the fixtures were captured from a cluster where the deliberately-stuck " +
			"resources were healthy, which means the capture is wrong")
	}
	t.Logf("checked %d suspect-producing objects out of %d captured", checked, len(objs))
}

// TestGoldenStillCoversBareConditions guards the fixtures themselves. The whole
// value of this data is that it contains a shape nobody would have written by
// hand — a condition with no reason and no message. If a future re-capture comes
// from a provider version that populates every reason, these tests keep passing
// while no longer covering the regression at all, and the coverage would be lost
// silently. That is worth failing loudly for: it means the fixtures need a
// deliberate decision, not that the code is broken.
func TestGoldenStillCoversBareConditions(t *testing.T) {
	for _, obj := range loadGolden(t) {
		for _, c := range Conditions(obj) {
			if (c.Status == "False" || c.Status == "Unknown") && c.Reason == "" && c.Message == "" {
				t.Logf("bare condition retained: %s/%s %s=%s",
					obj.GetKind(), obj.GetName(), c.Type, c.Status)
				return
			}
		}
	}
	t.Fatal("no captured condition is False/Unknown with neither reason nor message — " +
		"the golden data no longer covers the shape it was captured for, so " +
		"TestGoldenSuspectsAlwaysExplained is now weaker than it looks. Re-capture " +
		"from a cluster that reproduces it, or retire these tests deliberately.")
}

func renderConds(cs []Condition) string {
	if len(cs) == 0 {
		return "<none>"
	}
	out := ""
	for _, c := range cs {
		out += "[" + c.Type + "=" + c.Status + " reason=" + q(c.Reason) + " message=" + q(c.Message) + "] "
	}
	return out
}

func q(s string) string { return `"` + s + `"` }
