package tools

import "strings"

// CLAUDE.md hard rule 3: no secret contents in output — connection-secret
// presence/status only, never values.
//
// A core/v1 Secret fetched directly already satisfies that structurally:
// ResourceView is a closed projection, and a Secret keeps data/stringData at the
// top level, outside the spec ResourceView returns. But spec itself is returned
// verbatim, and for some kinds spec EMBEDS a whole Secret manifest — most
// commonly a provider-kubernetes Object, whose spec.forProvider.manifest is the
// object it manages, and a Composition, whose resource bases can be Secrets.
// Without this, get_resource on such an object returned the Secret's values.
//
// The redaction is structural, not a guess. Any object anywhere inside spec that
// is a core Secret manifest (see isSecretManifest — lenient about spelling, strict
// about group) keeps its data/stringData KEYS, so presence stays visible, which
// is what the rule asks to report, while every value is replaced, as is its
// kubectl last-applied annotation, which holds a JSON copy of the same data.
// Nothing else is touched: a ConfigMap's data is returned as written, and so are
// the *SecretRef / writeConnectionSecretToRef fields that NAME a secret, since
// naming it is exactly the presence information the rule wants surfaced.
//
// Deliberately NOT done: masking values by key name (password, token, …). Key
// names do not mark secret payloads — username, tls.key and .dockerconfigjson
// would all slip through — and scalar naming fields such as secretName would be
// blanked. It is also the heuristic live-output scrubbing this project decided
// against for provider error text (the --log-file recorder does mask by key name,
// scalars only, where false positives cost less).
//
// Residual channels, returned as written:
//   - any free-form or string-valued field: a Helm Release's values and set[]
//     pairs, a provider-terraform Workspace's vars and inline module, a Pod's
//     env values, and — the common case in Crossplane v2 — function-go-templating
//     and KCL inline templates, which are YAML STRINGS and are not parsed. So
//     "a Composition's resource bases" is covered for patch-and-transform bases,
//     not for templated ones. Keeping secrets out of these is the author's job,
//     and the machinery exists (valuesFrom / secretRef, so spec carries a ref);
//   - provider error text in conditions, events and decodedErrors, surfaced
//     verbatim by an explicit project decision, because it is actionable.

// lastAppliedAnnotation holds kubectl's JSON copy of the whole object — for a
// Secret manifest pasted from `kubectl get secret -o yaml`, that includes data.
const lastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

// redactEmbeddedSecrets returns a copy of v in which every embedded core/v1
// Secret manifest has its data/stringData values, and its last-applied
// annotation, replaced. v is never mutated — not its maps, not its lists: it is
// the fetched object's own spec.
func redactEmbeddedSecrets(v any) any {
	switch t := v.(type) {
	case map[string]any:
		secret := isSecretManifest(t)
		out := make(map[string]any, len(t))
		for k, val := range t {
			switch {
			case secret && (k == "data" || k == "stringData"):
				out[k] = redactSecretValues(val)
			case secret && k == "metadata":
				out[k] = redactLastApplied(val)
			default:
				out[k] = redactEmbeddedSecrets(val)
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = redactEmbeddedSecrets(e)
		}
		return out
	default:
		return v
	}
}

// isSecretManifest reports whether m is a core Secret manifest. It is lenient
// about spelling on purpose: the apiserver rejects "core/v1", a padded "v1" or a
// missing apiVersion, so no Secret is ever created from them — but the
// plaintext still sits in the embedding object's spec, and a failing object is
// exactly what someone debugs with this tool. A Secret-named kind in any OTHER
// group is left alone: that is a different API, and the Secret-shaped CRDs in
// the Crossplane ecosystem carry values through *SecretRef fields instead.
func isSecretManifest(m map[string]any) bool {
	kind, _ := m["kind"].(string)
	if !strings.EqualFold(strings.TrimSpace(kind), "Secret") {
		return false
	}
	av, present := m["apiVersion"]
	if !present || av == nil {
		return true
	}
	s, ok := av.(string)
	if !ok {
		return true // not a valid apiVersion at all; err towards redacting
	}
	switch strings.TrimSpace(s) {
	case "", "v1", "core/v1":
		return true
	}
	return false
}

// redactLastApplied copies a Secret manifest's metadata, replacing only the
// last-applied annotation. Every other annotation and field is left readable:
// names and labels are presence information, not payload.
func redactLastApplied(v any) any {
	md, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := make(map[string]any, len(md))
	for k, val := range md {
		out[k] = val
	}
	ann, ok := md["annotations"].(map[string]any)
	if !ok {
		return out
	}
	if _, has := ann[lastAppliedAnnotation]; has {
		copied := make(map[string]any, len(ann))
		for k, val := range ann {
			copied[k] = val
		}
		copied[lastAppliedAnnotation] = redactedMarker
		out["annotations"] = copied
	}
	return out
}

// redactSecretValues keeps a Secret payload's keys and replaces every value with
// redactedMarker — the same fixed marker the --log-file recorder uses, so it
// reveals nothing, not even a length, and reads the same in both places. An
// absent payload (null) stays absent: replacing it would invent one. A payload
// that is not a map — malformed, but possible in a hand-written manifest — is
// replaced whole rather than passed through.
func redactSecretValues(v any) any {
	if v == nil {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return redactedMarker
	}
	out := make(map[string]any, len(m))
	for k := range m {
		out[k] = redactedMarker
	}
	return out
}
