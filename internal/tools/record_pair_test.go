package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

// The key-based mask inspects a scalar's OWN key, so the {name, value} shape
// that Kubernetes env and provider-terraform vars both use slipped through
// entirely: the keys it sees are "name"/"key"/"value", none of which is
// sensitive. That is the commonest inline-credential shape in the specs this
// server reads, and it contradicted the Recorder's own claim that inline
// credentials are not written verbatim.

// Named so gosec's G101 does not read it as a hardcoded credential.
const fixtureDSNSecret = "s3cr3t-value"

func redactJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(redactValue(v))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestRedactPairEncodedCredentials(t *testing.T) {
	cases := []struct {
		name    string
		in      any
		leaked  string // must NOT appear
		kept    string // must still appear (the identifying half)
		wantRed bool
	}{
		{
			name: "provider-terraform vars",
			in: map[string]any{"spec": map[string]any{"vars": []any{
				map[string]any{"key": "db_password", "value": "S3cr3t!"},
			}}},
			leaked:  "S3cr3t!",
			kept:    "db_password",
			wantRed: true,
		},
		{
			name: "pod-style env",
			in: map[string]any{"env": []any{
				map[string]any{"name": "DB_PASSWORD", "value": "hunter2"},
			}},
			leaked:  "hunter2",
			kept:    "DB_PASSWORD",
			wantRed: true,
		},
		{
			name: "val spelling",
			in: map[string]any{"settings": []any{
				map[string]any{"name": "api_key", "val": "ak-12345"},
			}},
			leaked:  "ak-12345",
			kept:    "api_key",
			wantRed: true,
		},
		{
			// DSN shape rather than a credential-bearing URL: gosec's G101
			// flags "user:pass@host" literals even in test data, and a DSN is
			// what provider-sql actually emits anyway.
			name: "nested under a deeper path",
			in: map[string]any{"a": map[string]any{"b": []any{
				map[string]any{"key": "connectionString", "value": "host=db.internal user=app pw=" + fixtureDSNSecret},
			}}},
			leaked:  fixtureDSNSecret,
			kept:    "connectionString",
			wantRed: true,
		},
		{
			// The false-positive guard: an ordinary {name, value} pair whose name
			// says nothing sensitive must survive untouched. Masking it would
			// blank exactly the diagnostic detail this tool exists to surface.
			name: "non-sensitive name is left alone",
			in: map[string]any{"env": []any{
				map[string]any{"name": "LOG_LEVEL", "value": "debug"},
			}},
			leaked:  "",
			kept:    "debug",
			wantRed: false,
		},
		{
			name: "region is not a credential",
			in: map[string]any{"vars": []any{
				map[string]any{"key": "region", "value": "eu-west-1"},
			}},
			leaked:  "",
			kept:    "eu-west-1",
			wantRed: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := redactJSON(t, c.in)
			if c.leaked != "" && strings.Contains(got, c.leaked) {
				t.Errorf("credential %q written verbatim: %s", c.leaked, got)
			}
			if !strings.Contains(got, c.kept) {
				t.Errorf("expected %q to survive redaction, got %s", c.kept, got)
			}
			if red := strings.Contains(got, redactedMarker); red != c.wantRed {
				t.Errorf("redacted=%v, want %v: %s", red, c.wantRed, got)
			}
		})
	}
}

// TestRedactPairEncodedLeavesKeyBasedIntact guards that the new structural rule
// composes with the existing key-based one rather than replacing it.
func TestRedactPairEncodedLeavesKeyBasedIntact(t *testing.T) {
	got := redactJSON(t, map[string]any{
		"password": "direct-secret",
		"env":      []any{map[string]any{"name": "TOKEN", "value": "pair-secret"}},
		"region":   "eu-west-1",
	})
	for _, leaked := range []string{"direct-secret", "pair-secret"} {
		if strings.Contains(got, leaked) {
			t.Errorf("%q leaked: %s", leaked, got)
		}
	}
	if !strings.Contains(got, "eu-west-1") {
		t.Errorf("non-sensitive values must survive: %s", got)
	}
}
