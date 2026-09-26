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

// Precision over a realistic, secret-free spec: lists, nested lists, scalars in
// lists, refs. Absence-only checks cannot catch a walk that DROPS content — a
// mutation that emptied every list passed the whole suite until this existed.
func TestRedactKeepsSecretFreeSpecIdentical(t *testing.T) {
	spec := map[string]any{
		"forProvider": map[string]any{
			"region": "eu-west-1",
			"tags":   []any{"a", "b"},
			"rules":  []any{map[string]any{"port": int64(443), "cidrs": []any{"10.0.0.0/8"}}},
		},
		"references": []any{map[string]any{"patchesFrom": map[string]any{"name": "cfg"}}},
		"resources": []any{map[string]any{"base": map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "c", "labels": map[string]any{"app": "x"}},
			"data":     map[string]any{"k": "v"},
		}}},
		"writeConnectionSecretToRef": map[string]any{"name": "conn"},
	}
	if got := redactEmbeddedSecrets(spec); !reflect.DeepEqual(got, spec) {
		t.Errorf("a spec with nothing secret in it must come back unchanged\n got: %v\nwant: %v", got, spec)
	}
}

// Every spelling the core group can take, including the ones the apiserver
// rejects: plaintext in a failing object's spec is still plaintext.
func TestRedactCoreGroupSpellings(t *testing.T) {
	for _, av := range []any{"V1", "v1beta1", "", "Core/v1", nil, 1} {
		m := secretManifest()
		m["apiVersion"] = av // explicit null and a non-string included deliberately
		t.Run(fmt.Sprintf("apiVersion=%v", av), func(t *testing.T) {
			mustNotLeak(t, redactEmbeddedSecrets(map[string]any{"manifest": m}))
		})
	}
}

func TestRedactSecretKindAndPayloadKeyCase(t *testing.T) {
	for _, kind := range []string{"secret", " Secret ", "SECRET"} {
		m := secretManifest()
		m["kind"] = kind
		t.Run("kind="+kind, func(t *testing.T) {
			mustNotLeak(t, redactEmbeddedSecrets(map[string]any{"manifest": m}))
		})
	}
	t.Run("Data key", func(t *testing.T) {
		m := secretManifest()
		m["Data"] = m["data"]
		delete(m, "data")
		mustNotLeak(t, redactEmbeddedSecrets(map[string]any{"manifest": m}))
	})
}

// A provider-kubernetes Object pasted from `kubectl get -o yaml` into a
// Composition base: the OBJECT's last-applied annotation duplicates the Secret
// it embeds, one level above the Secret itself.
func TestRedactLastAppliedOnTheEmbeddingObject(t *testing.T) {
	spec := map[string]any{"resources": []any{map[string]any{"base": map[string]any{
		"apiVersion": "kubernetes.crossplane.io/v1alpha2",
		"kind":       "Object",
		"metadata": map[string]any{"annotations": map[string]any{
			lastAppliedAnnotation: `{"spec":{"forProvider":{"manifest":{"kind":"Secret","data":{"password":"` + fixtureDataValue + `"}}}}}`,
			"team":                "payments",
		}},
		"spec": map[string]any{"forProvider": map[string]any{"manifest": secretManifest()}},
	}}}}
	out := mustNotLeak(t, redactEmbeddedSecrets(spec))
	if !strings.Contains(out, "payments") {
		t.Errorf("unrelated annotations must survive: %s", out)
	}
}

func TestRedactDoesNotMutateInputAnnotations(t *testing.T) {
	ann := map[string]any{lastAppliedAnnotation: "orig"}
	m := secretManifest()
	m["metadata"] = map[string]any{"annotations": ann}
	_ = redactEmbeddedSecrets(map[string]any{"manifest": m})
	if ann[lastAppliedAnnotation] != "orig" {
		t.Error("redaction wrote the marker into the input's annotation map")
	}
}

// Terraform write-only arguments, as upjet renders them. provider-upjet-azure
// v2.7.0 ships a Key Vault Secret whose value is spec.forProvider.valueWo and a
// PostgreSQL server whose admin password is administratorPasswordWo — plain
// strings, secret by definition.
func TestRedactWriteOnlyArguments(t *testing.T) {
	spec := map[string]any{
		"forProvider": map[string]any{
			"valueWo":        fixtureDataValue,
			"valueWoVersion": int64(1), // a number: the trigger, not the secret
			"keyVaultId":     "/subscriptions/x",
		},
		"initProvider": map[string]any{
			"nested": map[string]any{"administratorPasswordWo": fixtureStringValue},
		},
		// Outside forProvider/initProvider there are no Terraform arguments.
		"notesWo": "left alone",
	}
	out := mustNotLeak(t, redactEmbeddedSecrets(spec))

	for _, keep := range []string{`"valueWoVersion":1`, "/subscriptions/x", "left alone"} {
		if !strings.Contains(out, keep) {
			t.Errorf("%s must survive: %s", keep, out)
		}
	}
	for _, name := range []string{"valueWo", "administratorPasswordWo", "p9Wo"} {
		if !isWriteOnlyArg(name) {
			t.Errorf("%s is a write-only argument name", name)
		}
	}
	for _, name := range []string{"Wo", "slotTwo", "valueWoVersion", "WORKLOAD"} {
		if isWriteOnlyArg(name) {
			t.Errorf("%s is not a write-only argument name", name)
		}
	}
}

// A nested Terraform block can render as a list of objects, so a write-only
// argument in a block may sit inside a list; and a write-only field whose value is not a plain
// string must be replaced whole, not walked (walking returns strings unchanged).
func TestRedactWriteOnlyArgumentsInBlocksAndNonStrings(t *testing.T) {
	spec := map[string]any{"forProvider": map[string]any{
		"block":     []any{map[string]any{"passwordWo": fixtureDataValue}},
		"secretsWo": []any{fixtureStringValue},
		"mapWo":     map[string]any{"k": fixtureDataValue},
		"fooIDWo":   fixtureDataValue, // an acronym before "Wo" is still a write-only name
		"unsetWo":   nil,              // unset stays unset, like data: null
	}}
	if out := mustNotLeak(t, redactEmbeddedSecrets(spec)); !strings.Contains(out, `"unsetWo":null`) {
		t.Errorf("an unset write-only argument must stay null: %s", out)
	}
}

// An apiVersion with an empty group is the core group too.
func TestRedactEmptyGroupIsCore(t *testing.T) {
	m := secretManifest()
	m["apiVersion"] = "/v1"
	mustNotLeak(t, redactEmbeddedSecrets(map[string]any{"manifest": m}))
}
