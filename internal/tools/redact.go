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
// The redaction is structural, not a guess — it matches object shape and schema
// conventions, never the content of a value. Three things are redacted:
//
//   - Embedded Secret manifests. Any object anywhere inside spec that is a core
//     Secret (isSecretManifest) keeps its data/stringData KEYS — presence, which
//     is what the rule asks to report — while every value is replaced.
//   - kubectl's last-applied-configuration annotation, on ANY object inside
//     spec. It is a JSON copy of that object: on a Secret it duplicates the data,
//     and on an object that embeds a Secret (a provider-kubernetes Object pasted
//     into a Composition base) it duplicates the embedded Secret. It adds nothing
//     diagnostic, since the object it copies is right there.
//   - Terraform write-only arguments. Terraform's marker is `WriteOnly: true` in
//     a provider schema — values it never persists to state or plan — and by
//     provider convention those arguments are named with a `_wo` suffix, which
//     upjet renders as a `Wo` suffix on a plain spec field. In practice they are
//     secrets: provider-upjet-azure v2.7.0 has four fields named `…Wo`, on five
//     resources — three admin/job passwords (administratorPasswordWo,
//     administratorLoginPasswordWo, passwordWo) and the Key Vault Secret's
//     valueWo — all secret. So under a forProvider / initProvider key, at any
//     depth and through lists or objects, the whole non-null value of a field
//     whose name ends in `Wo` is replaced. This is a naming convention
//     with one meaning, not a guess from a word like "password"; the numeric
//     `…WoVersion` companion does not end in `Wo` and is left alone. The rule is
//     keyed on location, not kind: a write-only value held anywhere else — the
//     XR/claim spec field a composition patches it from, or an inline template
//     string — is returned as written.
//
// Nothing else is touched: a ConfigMap's data is returned as written, and so are
// the *SecretRef / writeConnectionSecretToRef fields that NAME a secret, since
// naming it is exactly the presence information the rule wants surfaced.
//
// Deliberately NOT done: masking values by ordinary key names (password, token,
// …). Key names do not mark secret payloads — username, tls.key and
// .dockerconfigjson would all slip through — and scalar naming fields such as
// secretName would be blanked. It is also the heuristic live-output scrubbing
// this project decided against for provider error text (the --log-file recorder
// does mask by key name, scalars only, where false positives cost less).
//
// Residual channels, returned as written:
//   - any free-form or string-valued field: a Helm Release's values and set[]
//     pairs, a provider-terraform Workspace's vars and inline module, a Pod's
//     env values, and — the common case in Crossplane v2 — function-go-templating
//     and KCL inline templates, which are YAML STRINGS and are not parsed. So
//     patch-and-transform Composition bases are covered and templated ones are
//     not. Keeping secrets out of these is the author's job, and the machinery
//     exists (valuesFrom / secretRef, so spec carries a ref);
//   - provider error text in conditions, events and decodedErrors, surfaced
//     verbatim by an explicit project decision, because it is actionable.

// lastAppliedAnnotation holds kubectl's JSON copy of the whole object.
const lastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

// redactEmbeddedSecrets returns a redacted copy of v (see the package notes
// above). v is never mutated — not its maps, not its lists, not its
// annotations: it is the fetched object's own spec.
func redactEmbeddedSecrets(v any) any { return redactWalk(v, false) }

// redactWalk carries whether it is inside forProvider/initProvider, where
// Terraform write-only arguments live.
func redactWalk(v any, providerArgs bool) any {
	switch t := v.(type) {
	case map[string]any:
		secret := isSecretManifest(t)
		out := make(map[string]any, len(t))
		for k, val := range t {
			switch {
			case secret && (strings.EqualFold(k, "data") || strings.EqualFold(k, "stringData")):
				out[k] = redactSecretValues(val)
			case k == "metadata":
				out[k] = redactLastApplied(redactWalk(val, providerArgs))
			case providerArgs && isWriteOnlyArg(k) && val != nil:
				// Whole value, whatever its type: walking a list or map of
				// secrets would return its strings unchanged.
				out[k] = redactedMarker
			case k == "forProvider" || k == "initProvider":
				out[k] = redactWalk(val, true)
			default:
				out[k] = redactWalk(val, providerArgs)
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = redactWalk(e, providerArgs)
		}
		return out
	default:
		return v
	}
}

// isWriteOnlyArg reports whether name is upjet's rendering of a Terraform
// write-only argument: a name ending in the camelCase word "Wo", e.g. valueWo,
// administratorPasswordWo. A capital W always starts a new camelCase word, so no
// check on the character before it is needed — an earlier one only rejected
// names like fooIDWo. "…WoVersion" does not end in "Wo" and is never matched.
func isWriteOnlyArg(name string) bool {
	return len(name) > 2 && strings.HasSuffix(name, "Wo")
}

// isSecretManifest reports whether m is a core Secret manifest. Any
// apiVersion without a group ("v1", "V1", "v1beta1" — the core group has no
// name) or in the explicit "core/" group counts, as does a missing, null or
// non-string apiVersion, and kind is matched case-insensitively. The apiserver
// rejects most of those spellings, so no Secret is created from them — but the
// plaintext still sits in the embedding object's spec, and a failing object is
// exactly what someone debugs with this tool. A Secret-named kind in any OTHER
// group is left alone: that is a different API.
func isSecretManifest(m map[string]any) bool {
	kind, _ := m["kind"].(string)
	if !strings.EqualFold(strings.TrimSpace(kind), "Secret") {
		return false
	}
	// Missing, null and non-string apiVersions all land here: none is a valid
	// apiVersion, so err towards redacting.
	s, ok := m["apiVersion"].(string)
	if !ok {
		return true
	}
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "/") {
		return true // no group: the core group
	}
	group, _, _ := strings.Cut(s, "/")
	return group == "" || strings.EqualFold(group, "core")
}

// redactLastApplied replaces the last-applied annotation in md, in place, and
// leaves every other annotation and field readable: names and labels are
// presence information, not payload. In place is safe because its only caller
// passes redactWalk's output, which is already a fresh copy of the input — a
// second copy here was dead code, which mutation testing surfaced as a
// "survivor" no test could ever kill.
func redactLastApplied(md any) any {
	if m, ok := md.(map[string]any); ok {
		if ann, ok := m["annotations"].(map[string]any); ok {
			if _, has := ann[lastAppliedAnnotation]; has {
				ann[lastAppliedAnnotation] = redactedMarker
			}
		}
	}
	return md
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
