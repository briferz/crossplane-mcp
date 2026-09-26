package tools

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// secretManifest is a core/v1 Secret as it appears embedded in another object's
// spec — a provider-kubernetes Object's manifest, or a Composition base.
func secretManifest() map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": "db-creds"},
		"data":       map[string]any{"password": fixtureDataValue},
		"stringData": map[string]any{"username": fixtureStringValue},
	}
}

func mustNotLeak(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, needle := range []string{fixtureDataValue, fixtureStringValue} {
		if strings.Contains(string(b), needle) {
			t.Errorf("secret value %q survived redaction: %s", needle, b)
		}
	}
	return string(b)
}

func TestRedactEmbeddedSecretKeepsKeysDropsValues(t *testing.T) {
	spec := map[string]any{"forProvider": map[string]any{"manifest": secretManifest()}}

	out := mustNotLeak(t, redactEmbeddedSecrets(spec))

	// Presence is what hard rule 3 asks to report, so the keys must survive.
	for _, key := range []string{`"password"`, `"username"`, redactedMarker} {
		if !strings.Contains(out, key) {
			t.Errorf("expected %s in the redacted output: %s", key, out)
		}
	}
	// Non-payload fields of the Secret are not secret and stay readable.
	if !strings.Contains(out, "db-creds") {
		t.Errorf("the Secret's name is presence information and must survive: %s", out)
	}
}

// Precision: only core/v1 Secrets. A ConfigMap's data is ordinary config and
// is often exactly what someone debugging needs to see.
func TestRedactLeavesConfigMapsAndRefsAlone(t *testing.T) {
	spec := map[string]any{
		"forProvider": map[string]any{"manifest": map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"data": map[string]any{"log-level": "debug"},
		}},
		// A ref NAMES a secret — that is the presence information the rule wants.
		"writeConnectionSecretToRef": map[string]any{"name": "app-conn", "namespace": "team-a"},
		// Same kind name, different API group: not a core Secret.
		"other": map[string]any{"apiVersion": "example.org/v1", "kind": "Secret",
			"data": map[string]any{"k": "v"}},
	}

	got := redactEmbeddedSecrets(spec)

	if !reflect.DeepEqual(got, spec) {
		t.Errorf("nothing here is a core/v1 Secret, so nothing may change\n got: %v\nwant: %v", got, spec)
	}
}

// A provider-kubernetes Object can manage a List, and a Composition holds its
// bases in a list: the walk must descend through slices.
func TestRedactDescendsThroughLists(t *testing.T) {
	spec := map[string]any{"resources": []any{
		map[string]any{"base": map[string]any{"apiVersion": "v1", "kind": "List",
			"items": []any{secretManifest()}}},
	}}
	mustNotLeak(t, redactEmbeddedSecrets(spec))
}

// A malformed payload (not a map) is replaced whole rather than passed through.
func TestRedactReplacesNonMapPayloadWhole(t *testing.T) {
	m := secretManifest()
	m["data"] = fixtureDataValue
	out := redactEmbeddedSecrets(map[string]any{"manifest": m}).(map[string]any)
	if got := out["manifest"].(map[string]any)["data"]; got != redactedMarker {
		t.Errorf("non-map data = %v, want the redaction marker", got)
	}
}

// The input is the fetched object's own spec map; redaction must not write
// through to it.
func TestRedactDoesNotMutateItsInput(t *testing.T) {
	spec := map[string]any{"forProvider": map[string]any{"manifest": secretManifest()}}
	_ = redactEmbeddedSecrets(spec)

	data := spec["forProvider"].(map[string]any)["manifest"].(map[string]any)["data"].(map[string]any)
	if data["password"] != fixtureDataValue {
		t.Error("redaction mutated its input: the fetched object's spec was rewritten")
	}
}

// A manifest pasted from `kubectl get secret -o yaml` carries
// kubectl.kubernetes.io/last-applied-configuration: JSON of the whole Secret,
// data included. Redacting data while passing metadata through verbatim would
// leak the same values one field over.
func TestRedactCoversLastAppliedConfiguration(t *testing.T) {
	m := secretManifest()
	m["metadata"] = map[string]any{
		"name": "db-creds",
		"annotations": map[string]any{
			"kubectl.kubernetes.io/last-applied-configuration": `{"apiVersion":"v1","kind":"Secret","data":{"password":"` + fixtureDataValue + `"}}`,
			"team": "payments",
		},
	}
	out := mustNotLeak(t, redactEmbeddedSecrets(map[string]any{"manifest": m}))
	if !strings.Contains(out, "payments") {
		t.Errorf("ordinary annotations carry no payload and must survive: %s", out)
	}
}

// The apiserver rejects a Secret manifest spelled "core/v1", " v1", or with no
// apiVersion — so no Secret is ever created, but the plaintext still sits in
// the embedding object's spec, and a failing Object is exactly what someone
// reaches for this tool to debug.
func TestRedactCoversNonCanonicalSecretManifests(t *testing.T) {
	for _, av := range []any{"core/v1", " v1", "v1 ", nil} {
		m := secretManifest()
		if av == nil {
			delete(m, "apiVersion")
		} else {
			m["apiVersion"] = av
		}
		t.Run(fmt.Sprintf("apiVersion=%q", av), func(t *testing.T) {
			mustNotLeak(t, redactEmbeddedSecrets(map[string]any{"manifest": m}))
		})
	}
}

// data: null is not a payload; replacing it with the marker would invent one.
func TestRedactLeavesAbsentPayloadAbsent(t *testing.T) {
	m := secretManifest()
	m["data"] = nil
	out := redactEmbeddedSecrets(map[string]any{"manifest": m}).(map[string]any)
	if got := out["manifest"].(map[string]any)["data"]; got != nil {
		t.Errorf("data: null became %v; there was no payload to redact", got)
	}
}

// The slice branch must copy too. Writing redacted elements back into the
// input's list would rewrite the fetched object's own manifest list.
func TestRedactDoesNotMutateInputLists(t *testing.T) {
	items := []any{secretManifest()}
	_ = redactEmbeddedSecrets(map[string]any{"items": items})

	data := items[0].(map[string]any)["data"].(map[string]any)
	if data["password"] != fixtureDataValue {
		t.Error("redaction wrote through a list into its input")
	}
}
