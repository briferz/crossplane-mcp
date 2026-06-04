package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	t.Setenv("XPMCP_TMP", "/tmp/xpmcp")

	cases := map[string]string{
		"~/log.jsonl":         filepath.Join(home, "log.jsonl"),
		"~":                   home,
		"$XPMCP_TMP/a.jsonl":  "/tmp/xpmcp/a.jsonl",
		"/abs/path.jsonl":     "/abs/path.jsonl",
		"relative/path.jsonl": "relative/path.jsonl",
	}
	for in, want := range cases {
		if got := expandPath(in); got != want {
			t.Errorf("expandPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRecorderRecord(t *testing.T) {
	var buf bytes.Buffer
	r := &Recorder{w: &buf}

	r.record("diagnose", 5*time.Millisecond,
		map[string]any{"kind": "XApp", "name": "demo"},
		map[string]any{"healthy": false, "summary": "1 blocking"}, nil)
	r.record("get_resource", time.Millisecond,
		map[string]any{"kind": "Bucket", "name": "b"}, nil, errors.New("not found"))

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("want 2 JSONL lines, got %d", len(lines))
	}

	var ok callRecord
	if err := json.Unmarshal(lines[0], &ok); err != nil {
		t.Fatalf("line 1 not valid JSON: %v", err)
	}
	if ok.Tool != "diagnose" || ok.Time == "" || ok.Error != "" || ok.Output == nil {
		t.Errorf("success record wrong: %+v", ok)
	}

	var failed callRecord
	if err := json.Unmarshal(lines[1], &failed); err != nil {
		t.Fatalf("line 2 not valid JSON: %v", err)
	}
	// On error, the error is recorded and output is omitted.
	if failed.Error != "not found" || failed.Output != nil {
		t.Errorf("error record wrong: %+v", failed)
	}
	if failed.Input == nil {
		t.Errorf("input should be recorded even on error: %+v", failed)
	}
}

// TestRecordedNilIsIdentity confirms the zero-overhead path: a nil recorder
// returns the original handler unchanged.
func TestRecordedNilIsIdentity(t *testing.T) {
	h := func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, *struct{}, error) {
		return nil, nil, nil
	}
	got := recorded[struct{}, *struct{}](nil, "x", h)
	if reflect.ValueOf(got).Pointer() != reflect.ValueOf(mcp.ToolHandlerFor[struct{}, *struct{}](h)).Pointer() {
		t.Error("nil recorder should return the handler unchanged")
	}
}

// TestRecordedWritesLine exercises the wrapper end-to-end: calling the wrapped
// handler appends one record and forwards the handler's return values.
func TestRecordedWritesLine(t *testing.T) {
	var buf bytes.Buffer
	r := &Recorder{w: &buf}
	type out struct {
		Healthy bool `json:"healthy"`
	}
	h := func(context.Context, *mcp.CallToolRequest, map[string]any) (*mcp.CallToolResult, *out, error) {
		return nil, &out{Healthy: false}, nil
	}
	wrapped := recorded(r, "diagnose", h)
	if _, o, err := wrapped(context.Background(), nil, map[string]any{"kind": "XApp"}); err != nil || o == nil {
		t.Fatalf("wrapped handler return not forwarded: o=%v err=%v", o, err)
	}
	var rec callRecord
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("no valid record written: %v", err)
	}
	if rec.Tool != "diagnose" {
		t.Errorf("want tool=diagnose, got %q", rec.Tool)
	}
}

// TestRecorderRedaction confirms scalar values under sensitive keys are masked
// while reference structures (a secretRef's name) and innocuous fields survive,
// and that --log-redact=false (redact:false) leaves everything intact.
func TestRecorderRedaction(t *testing.T) {
	out := map[string]any{
		"spec": map[string]any{
			"forProvider": map[string]any{
				"password":  "p@ssw0rd",
				"region":    "us-east-1",
				"apiToken":  "tok-123",
				"secretRef": map[string]any{"name": "db-conn", "namespace": "team-a"},
			},
		},
	}

	// redaction on
	var on bytes.Buffer
	(&Recorder{w: &on, redact: true}).record("get_resource", 0, nil, out, nil)
	s := on.String()
	if !contains(s, redactedMarker) {
		t.Fatalf("expected redaction marker, got: %s", s)
	}
	if contains(s, "p@ssw0rd") || contains(s, "tok-123") {
		t.Errorf("sensitive scalar leaked: %s", s)
	}
	if !contains(s, "us-east-1") || !contains(s, "db-conn") {
		t.Errorf("non-sensitive value (region) or ref name (db-conn) was over-redacted: %s", s)
	}

	// redaction off
	var off bytes.Buffer
	(&Recorder{w: &off, redact: false}).record("get_resource", 0, nil, out, nil)
	if !contains(off.String(), "p@ssw0rd") {
		t.Errorf("redact:false should keep values verbatim: %s", off.String())
	}
}

func contains(s, sub string) bool { return bytes.Contains([]byte(s), []byte(sub)) }

// TestScrubSecrets covers the high-precision content scrub: it masks credential
// material embedded in a string value while leaving innocuous identifiers
// (ARNs, resource IDs, request UUIDs, account numbers) intact.
func TestScrubSecrets(t *testing.T) {
	pem := "-----BEGIN RSA PRIVATE KEY-----\nMIIEvQIBADANBgkqh\n-----END RSA PRIVATE KEY-----"         //nolint:gosec // G101: synthetic test fixture, not a real key
	pgp := "-----BEGIN PGP PRIVATE KEY BLOCK-----\nlQVYBGZ0dummy\n-----END PGP PRIVATE KEY BLOCK-----" //nolint:gosec // G101: synthetic test fixture, not a real key
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
	jwtMin := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.e30.JIWT1_Db9Wiqgn-jwmNNDRBe2m25imsMo-ZqYaHBgK0" // minimal "{}" payload
	jwtNone := "eyJhbGciOiJub25lIn0.eyJzdWIiOiJhZG1pbiJ9."                                           // alg=none, empty signature

	masked := map[string]string{
		"pem":      pem,
		"pgp":      pgp,
		"awskey":   "credentials AKIAIOSFODNN7EXAMPLE rejected",
		"jwt":      "id_token=" + jwt,
		"jwt-min":  "token " + jwtMin,
		"jwt-none": "unsigned " + jwtNone,
		"bearer":   "Authorization: Bearer abcdef0123456789ghij", // ≥16-char token
	}
	for name, in := range masked {
		if got := scrubSecrets(in); !contains(got, redactedMarker) {
			t.Errorf("%s: expected redaction, got %q", name, got)
		}
	}
	if contains(scrubSecrets(pem), "MIIEvQ") {
		t.Error("PEM key body leaked through scrub")
	}
	if contains(scrubSecrets(pgp), "lQVYBGZ0dummy") {
		t.Error("PGP key body leaked through scrub")
	}
	if got := scrubSecrets("Authorization: Bearer abcdef0123456789ghij"); !contains(got, "Bearer "+redactedMarker) {
		t.Errorf("bearer scheme word should survive, token masked: %q", got)
	}
	// A structured-header VALUE (no "Authorization:" prefix in the string) with a
	// realistic-length token must still be masked.
	if got := scrubSecrets("Bearer abcdef0123456789ghij"); !contains(got, redactedMarker) {
		t.Errorf("bare structured-header bearer value should be masked: %q", got)
	}
	// The scheme→token separator is spaces/tabs only: a dangling "Bearer" at a
	// line end must NOT consume the next line's first token.
	if got := scrubSecrets("Authorization: Bearer\nGET-123 /v1/x"); !contains(got, "GET-123") {
		t.Errorf("bearer match must not span a newline onto the next line: %q", got)
	}

	// False-positive guards: these are NOT secrets and must pass through verbatim.
	keep := []string{
		"arn:aws:iam::123456789012:role/MyRole",
		"i-0abc123def4567890",
		"a1b2c3d4-e5f6-7890-abcd-ef1234567890",
		"Error: failed to create bucket: AccessDenied",
		"account 123456789012 is denied",
		"on main.tf line 42, in resource \"aws_s3_bucket\" \"this\"",
		"Bearer token is expired",                             // short word → not a token
		"missing Bearer token in the request header",          // prose, not a credential
		"Bearer 2.0 is the supported scheme",                  // version string, <16 chars → not a token
		"WWW-Authenticate: Bearer realm=\"api\", error=\"x\"", // RFC 6750 challenge params must stay intact
		"eyJshort.x.y",        // header <8 chars after eyJ → not a JWT
		"com.example.service", // dotted, but no eyJ prefix
		"-----BEGIN PGP PUBLIC KEY BLOCK-----\nabc\n-----END PGP PUBLIC KEY BLOCK-----", // PUBLIC, not PRIVATE
	}
	for _, in := range keep {
		if got := scrubSecrets(in); got != in {
			t.Errorf("over-redacted identifier %q -> %q", in, got)
		}
	}
}

// TestRecorderContentScrub confirms the content scrub reaches nested string
// values (e.g. decodedErrors/reasons slices), preserves the surrounding
// actionable text, and is gated by --log-redact.
func TestRecorderContentScrub(t *testing.T) {
	out := map[string]any{
		"suspects": []any{
			map[string]any{
				"decodedErrors": []any{"Error: bad creds AKIAIOSFODNN7EXAMPLE on main.tf line 3"},
				"reasons":       []any{"Synced: ReconcileError — Authorization: Bearer tok.en-1234567890ab"},
			},
		},
	}
	var on bytes.Buffer
	(&Recorder{w: &on, redact: true}).record("diagnose", 0, nil, out, nil)
	s := on.String()
	if contains(s, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("AWS key in decodedErrors not scrubbed: %s", s)
	}
	if !contains(s, "on main.tf line 3") {
		t.Errorf("surrounding actionable text over-redacted: %s", s)
	}
	if contains(s, "tok.en-1234567890ab") {
		t.Errorf("bearer token in reasons not scrubbed: %s", s)
	}

	var off bytes.Buffer
	(&Recorder{w: &off, redact: false}).record("diagnose", 0, nil, out, nil)
	if !contains(off.String(), "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("redact:false must keep content verbatim: %s", off.String())
	}
}

// TestRecorderErrorScrub confirms a secret embedded in a failed tool call's
// error string is scrubbed (the error field bypasses prepare/redactValue, so
// record() scrubs it directly), and that --log-redact=false keeps it verbatim.
func TestRecorderErrorScrub(t *testing.T) {
	secretErr := errors.New("auth failed: Authorization: Bearer sekrit.tok-1234567890 using AKIAIOSFODNN7EXAMPLE")
	var on bytes.Buffer
	(&Recorder{w: &on, redact: true}).record("get_resource", 0, map[string]any{"kind": "X"}, nil, secretErr)
	s := on.String()
	if contains(s, "AKIAIOSFODNN7EXAMPLE") || contains(s, "sekrit.tok-1234567890") {
		t.Errorf("secret in error string not scrubbed: %s", s)
	}
	if !contains(s, "auth failed") {
		t.Errorf("error context over-redacted: %s", s)
	}

	var off bytes.Buffer
	(&Recorder{w: &off, redact: false}).record("get_resource", 0, nil, nil, secretErr)
	if !contains(off.String(), "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("redact:false must keep the error verbatim: %s", off.String())
	}
}

// TestRecorderConcurrent guards the mutex: concurrent records (and a racing
// Close) must not interleave or race. Run with -race.
func TestRecorderConcurrent(t *testing.T) {
	var buf bytes.Buffer
	r := &Recorder{w: &buf}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.record("t", 0, map[string]any{"n": 1}, map[string]any{"ok": true}, nil)
		}()
	}
	wg.Wait()
	if n := bytes.Count(buf.Bytes(), []byte("\n")); n != 50 {
		t.Errorf("want 50 JSONL lines, got %d", n)
	}
	// Close redirects to io.Discard; a late record must not panic or write.
	_ = r.Close()
	r.record("late", 0, nil, nil, nil)
	if n := bytes.Count(buf.Bytes(), []byte("\n")); n != 50 {
		t.Errorf("record after Close should be discarded; got %d lines", n)
	}
}
