package tools

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Recorder appends a JSONL record per tool call — its input, output, duration,
// and any error — so a session against a real cluster can be inspected later.
// Enabled via --log-file / CROSSPLANE_MCP_LOG_FILE.
//
// It records the full tool input and output. By default (redact=true) two
// masks run: (1) key-based — scalar values under sensitive keys
// (password/token/secret/credential/…) are masked, so inline credentials in a
// resource spec are not written verbatim; (2) content-based — every logged
// string value is scrubbed for a few high-precision secret shapes (PEM private
// keys, AWS access-key IDs, JWTs, Authorization: Bearer tokens), which catches
// credential material the key-based mask misses, including in provider error
// text and the decoded provider-terraform/OpenTofu blob (decodedErrors).
//
// Both masks are BEST-EFFORT, not a guarantee: the content scrub is deliberately
// high-precision and will not catch an arbitrary or unusually-shaped secret, and
// it intentionally does NOT mask identifiers like account IDs or ARNs (they are
// often the actionable detail). Redaction applies only to the log; the live tool
// response is never altered. The server never reads Kubernetes Secret objects.
// Treat the log as potentially sensitive and review it before sharing off a
// machine that touches production.
type Recorder struct {
	mu     sync.Mutex
	w      io.Writer
	c      io.Closer
	redact bool
}

// NewRecorder opens dest for appending. dest "-" or "stderr" writes to stderr;
// anything else is treated as a file path (created if absent, mode 0600). When
// redact is true, scalar values under sensitive keys are masked before writing.
func NewRecorder(dest string, redact bool) (*Recorder, error) {
	if dest == "-" || dest == "stderr" {
		return &Recorder{w: os.Stderr, redact: redact}, nil
	}
	dest = expandPath(dest)
	// #nosec G304 -- dest is an operator-provided log path (flag/env), not attacker-controlled.
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // operator-provided log path
	if err != nil {
		return nil, err
	}
	return &Recorder{w: f, c: f, redact: redact}, nil
}

// Close releases the underlying file (no-op for stderr / nil). It takes the
// same lock as record so it can't race with an in-flight write, and redirects
// further writes to io.Discard so a late record() after Close is harmless.
func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.w = io.Discard
	if r.c != nil {
		c := r.c
		r.c = nil
		return c.Close()
	}
	return nil
}

// expandPath resolves $VARS and a leading ~ in the log path, so it works the
// same whether set via a shell (which would expand them itself) or via an MCP
// client's JSON config (which has no shell, so the raw value reaches us). An
// absolute path is always safe.
func expandPath(p string) string {
	p = os.ExpandEnv(p)
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

type callRecord struct {
	Time       string `json:"time"`
	Tool       string `json:"tool"`
	DurationMs int64  `json:"durationMs"`
	Input      any    `json:"input,omitempty"`
	Output     any    `json:"output,omitempty"`
	Error      string `json:"error,omitempty"`
}

func (r *Recorder) record(name string, dur time.Duration, in, out any, callErr error) {
	rec := callRecord{
		Time:       time.Now().UTC().Format(time.RFC3339Nano),
		Tool:       name,
		DurationMs: dur.Milliseconds(),
		Input:      r.prepare(in),
	}
	if callErr != nil {
		rec.Error = callErr.Error()
	} else {
		rec.Output = r.prepare(out)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, _ = r.w.Write(append(b, '\n'))
}

const redactedMarker = "[redacted]"

// sensitiveKey reports whether a field name suggests an inline secret value.
// Matched case-insensitively as substrings. Kept narrow enough to avoid masking
// innocuous fields (e.g. bare "key") while catching the dangerous ones.
var sensitiveKeyParts = []string{
	"password", "passwd", "secret", "token", "credential",
	"apikey", "api_key", "api-key", "accesskey", "access_key",
	"privatekey", "private_key", "private-key", "connectionstring",
	"connection", "dsn", "sasl",
}

func sensitiveKey(k string) bool {
	lk := strings.ToLower(k)
	for _, p := range sensitiveKeyParts {
		if strings.Contains(lk, p) {
			return true
		}
	}
	return false
}

// secretPatterns are high-precision matchers for credential material that can
// appear inline in a string VALUE (not just under a sensitive key) — e.g. a
// private key or token rendered into a provider error or a decoded OpenTofu
// blob, which the key-based mask never sees. Deliberately high-precision
// (distinctive prefixes/structure) so innocuous identifiers like ARNs, account
// IDs, resource IDs, and request UUIDs are NOT masked. Best-effort, not a
// guarantee: marking values sensitive remains the source system's job.
var secretPatterns = []*regexp.Regexp{
	// PEM private key blocks (RSA/EC/OPENSSH/PGP/…).
	regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
	// AWS access key IDs: AKIA/ASIA + 16 upper-alnum (20 chars total).
	regexp.MustCompile(`(?:AKIA|ASIA)[0-9A-Z]{16}`),
	// JSON Web Tokens (header.payload.signature; header always starts "eyJ").
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`),
}

// bearerRe masks the token after an Authorization "Bearer" scheme, keeping the
// scheme word so the surrounding text still reads sensibly.
var bearerRe = regexp.MustCompile(`(?i)(bearer\s+)[\w.~+/=-]+`)

// scrubSecrets replaces high-precision secret patterns in a string with the
// redaction marker. Applied to every logged string value when redaction is on.
func scrubSecrets(s string) string {
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, redactedMarker)
	}
	return bearerRe.ReplaceAllString(s, "${1}"+redactedMarker)
}

// prepare returns v normalised for logging, with sensitive scalar values masked
// when redaction is enabled. Returns v unchanged when redaction is off or v is
// nil. Best-effort: if v can't be round-tripped through JSON, it's logged as-is.
func (r *Recorder) prepare(v any) any {
	if v == nil || !r.redact {
		return v
	}
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var g any
	if err := json.Unmarshal(b, &g); err != nil {
		return v
	}
	return redactValue(g)
}

// redactValue masks scalar values under sensitive keys but recurses into maps
// and slices, so reference structures (e.g. a secretRef's name/namespace) are
// preserved while inline credential *values* are masked. Every string value not
// already whole-masked by a sensitive key is additionally run through
// scrubSecrets to catch high-precision secret shapes embedded in error text.
func redactValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if sensitiveKey(k) && isScalar(val) {
				t[k] = redactedMarker
			} else {
				t[k] = redactValue(val)
			}
		}
		return t
	case []any:
		for i := range t {
			t[i] = redactValue(t[i])
		}
		return t
	case string:
		return scrubSecrets(t)
	default:
		return v
	}
}

func isScalar(v any) bool {
	switch v.(type) {
	case map[string]any, []any:
		return false
	default:
		return true
	}
}

// recorded wraps a typed tool handler so its input/output is appended to the
// recorder. A nil recorder returns the handler unchanged (zero overhead).
func recorded[In, Out any](r *Recorder, name string, h mcp.ToolHandlerFor[In, Out]) mcp.ToolHandlerFor[In, Out] {
	if r == nil {
		return h
	}
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		start := time.Now()
		res, out, err := h(ctx, req, in)
		r.record(name, time.Since(start), in, out, err)
		return res, out, err
	}
}
