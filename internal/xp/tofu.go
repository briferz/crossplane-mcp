package xp

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode"

	"github.com/briferz/crossplane-mcp/internal/k8s"
)

// provider-terraform / OpenTofu hides the actionable error behind a base64+gzip
// blob, surfaced inside a Crossplane condition (or recurring event) message as a
// literal shell hint, e.g.:
//
//	... To see the full error run: echo "H4sIAAAA…" | base64 -d | gunzip
//
// decodeTFErrors detects that hint, decodes the blob, and reduces it to its
// actionable lines (the Error:/Summary: block, else the last few non-log lines)
// so an agent sees `Error: … on main.tf line NN` without shelling out. It is a
// pure, total transform: any malformed/oversized input degrades to "nothing
// decoded" rather than erroring the diagnose call.

const (
	// maxCompressedBytes caps the base64-decoded (still gzipped) input. The blob
	// rides inside a Kubernetes condition/event message, which is itself size
	// bounded, so the compressed payload is small; this is defence-in-depth.
	maxCompressedBytes = 256 * 1024
	// maxDecompressedBytes bounds the gunzipped output (decompression-bomb
	// defence). Generous on purpose: the real Error: is frequently the LAST thing
	// emitted after pages of provider boilerplate, so a small head cap would risk
	// cutting it. Overflow is marked explicitly, never silently dropped.
	maxDecompressedBytes = 4 * 1024 * 1024
	// fallbackKeepLines is how many trailing non-log lines to keep when the
	// decoded text has no Error:/Summary: marker to anchor on.
	fallbackKeepLines = 20
	// maxActionableLines caps the extracted line count (including a single
	// elision marker) so a pathological blob can't bloat the response.
	maxActionableLines = 40
	// keepFirst is how many leading lines to keep when the selection overflows
	// maxActionableLines; the remainder of the budget is kept from the tail.
	keepFirst = 4
	// maxLineRunes caps a single emitted line. The line-count budget alone
	// doesn't bound output size: an OpenTofu Error: body can carry a multi-MB
	// inline JSON/HTTP response on ONE line, which would otherwise pass through
	// whole (up to the 4 MiB decompress cap) and bloat the response. A longer
	// line is cut on a rune boundary with an explicit marker. This trims only
	// the derived decodedErrors output — the verbatim condition message stays
	// byte-identical in reasons (the never-truncate invariant is about that
	// message, not this derived field).
	maxLineRunes = 4000

	truncationMarker = "[crossplane-mcp: decoded output truncated at 4 MiB]"
	incompleteMarker = "[crossplane-mcp: decoded output incomplete]"
	elisionFmt       = "... %d line(s) elided (run the echo | base64 -d | gunzip hint for the full output) ..."
	lineTruncMarker  = " …[line truncated; run the echo | base64 -d | gunzip hint for the full line]"
)

// tfBlobRe matches the `echo "<base64>" | base64 -d | gunzip` hint. The capture
// is gated on the gzip-magic base64 prefix "H4sI" to avoid decoding unrelated
// base64. It accepts single/double quotes, `base64 -d`/`--decode`, and
// gunzip/gzip/zcat with any trailing flags. RE2 (no backtracking) → no ReDoS.
var tfBlobRe = regexp.MustCompile(`echo\s+(?:"(H4sI[A-Za-z0-9+/=\s]*?)"|'(H4sI[A-Za-z0-9+/=\s]*?)')\s*\|\s*base64\s+(?:-d|--decode)\b[^|]*\|\s*(?:g(?:unzip|zip)|zcat)\b(?:\s+-\S+)*`)

// tfErrorMarkers are the high-precision line prefixes (checked after left-key
// normalisation) that anchor the actionable block. Deliberately narrow: a
// trailing-space "error " or a "warning:" would false-positive on prose and
// deprecation noise, so they are excluded.
var tfErrorMarkers = []string{"error:", "summary:"}

// tfLogPrefixes are bracketed log-level prefixes dropped from the decoded blob.
var tfLogPrefixes = []string{"[trace]", "[debug]", "[info]", "[warn]", "[error]", "[fatal]"}

// tfOTELNoise are leftKey PREFIXES of OpenTelemetry boilerplate lines. Matched
// by prefix (not substring) so a legitimate error/context line that merely
// mentions one of these tokens — e.g. "  with aws_iam_role.otel_traces_exporter,"
// (a resource the user named after the env var) — is NOT misclassified as a log
// line and dropped, which would hide the failure site.
var tfOTELNoise = []string{"opentelemetry:", "otel_traces_exporter", "otel tracing is not enabled"}

// decodeTFErrors scans condition messages AND event messages for the shell hint,
// decodes each blob, extracts its actionable lines, and returns the results
// joined one entry per blob. seen dedups byte-identical decoded results within
// and across suspects in a single Diagnose call (the same blob re-fires every
// reconcile, so it lands in both the Synced condition and the recurring event,
// and often mirrors up the composite chain). Returns nil when nothing decodes,
// so the omitempty field stays absent and non-TF output is byte-identical.
func decodeTFErrors(condMsgs []string, events []k8s.Event, seen map[string]bool) []string {
	var out []string
	add := func(msg string) {
		for _, b64 := range tfBlobPayloads(msg) {
			decoded, ok := decodeBlob(b64)
			if !ok {
				continue
			}
			lines := extractActionable(decoded)
			if len(lines) == 0 {
				continue
			}
			joined := strings.Join(lines, "\n")
			if seen[joined] {
				continue
			}
			seen[joined] = true
			out = append(out, joined)
		}
	}
	for _, m := range condMsgs {
		add(m)
	}
	for _, e := range events {
		add(e.Message)
	}
	return out
}

// tfBlobPayloads returns the cleaned base64 payloads of every hint in msg
// (whitespace from a line-wrapped payload is stripped). The cheap regexp scan
// gates the expensive decode so messages without a hint allocate nothing.
func tfBlobPayloads(msg string) []string {
	matches := tfBlobRe.FindAllStringSubmatch(msg, -1)
	if matches == nil {
		return nil
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		b64 := m[1]
		if b64 == "" {
			b64 = m[2]
		}
		b64 = strings.Map(dropSpace, b64)
		if b64 != "" {
			out = append(out, b64)
		}
	}
	return out
}

// decodeBlob base64-decodes then gunzips a payload, bounded on both input and
// output. It is total: malformed base64, a non-gzip payload, or a panic all
// degrade to ("", false). A read error AFTER content has been decoded (a
// truncated member or concatenated stream) keeps the bytes already decoded and
// marks them incomplete — a successfully-decoded error is never thrown away.
func decodeBlob(b64 string) (decoded string, ok bool) {
	defer func() {
		if recover() != nil {
			decoded, ok = "", false
		}
	}()
	if b64 == "" {
		return "", false
	}
	compressed, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		if compressed, err = base64.RawStdEncoding.DecodeString(b64); err != nil {
			return "", false
		}
	}
	// Enforce the input cap on the DECODED length (base64 inflates ~33%), before
	// constructing the gzip reader, and verify the gzip magic bytes.
	if len(compressed) < 2 || len(compressed) > maxCompressedBytes {
		return "", false
	}
	if compressed[0] != 0x1f || compressed[1] != 0x8b {
		return "", false
	}
	gr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return "", false
	}
	// Single member only: provider-terraform emits one gzip member, so stop at
	// its end and ignore any trailing junk rather than trying to read a second
	// member (which would error AFTER the real content was already decoded).
	gr.Multistream(false)
	// Read one byte past the cap so overflow is detectable. Input and output are
	// both bounded, so this is not a decompression-bomb vector.
	out, rerr := io.ReadAll(io.LimitReader(gr, maxDecompressedBytes+1)) //nolint:gosec // G110: bounded input (maxCompressedBytes) + bounded output (LimitReader)
	overflow := false
	if len(out) > maxDecompressedBytes {
		out = out[:maxDecompressedBytes]
		overflow = true
	}
	if len(out) == 0 {
		return "", false
	}
	// Provider output is UTF-8 text; drop any invalid byte sequences — a rune
	// split by the overflow byte-cut, or stray bytes in a malformed stream — so
	// the decoded string stays valid UTF-8 (json would otherwise emit U+FFFD).
	s := strings.ToValidUTF8(string(out), "")
	switch {
	case overflow:
		s += "\n" + truncationMarker
	case rerr != nil && rerr != io.EOF:
		s += "\n" + incompleteMarker
	}
	return s, true
}

// extractActionable reduces a decoded OpenTofu blob to its actionable lines,
// operating at whole-line granularity only — it never substring-cuts a line, so
// the matched Error body survives intact. When an Error:/Summary: marker is
// present it keeps every non-log line from the first marker onward (the error is
// frequently last, after pages of boilerplate); otherwise it drops log/OTEL
// noise and keeps the trailing lines. Returns nil when the result is empty or
// all whitespace.
func extractActionable(decoded string) []string {
	lines := splitLines(decoded)

	var kept []string
	if start := firstMarker(lines); start >= 0 {
		for _, l := range lines[start:] {
			if !isLog(l) {
				kept = append(kept, l)
			}
		}
	} else {
		var survivors []string
		for _, l := range lines {
			if isLog(l) || strings.TrimSpace(l) == "" {
				continue
			}
			survivors = append(survivors, l)
		}
		if len(survivors) == 0 {
			survivors = nonBlank(lines)
		}
		kept = lastN(survivors, fallbackKeepLines)
	}

	kept = trimBlankEnds(kept)
	kept = applyBudget(kept)
	if allWhitespace(kept) {
		return nil
	}
	return capLineRunes(kept)
}

// capLineRunes bounds each line at maxLineRunes, cutting a longer one on a rune
// boundary (never mid-rune) and appending lineTruncMarker so a single
// pathological line can't defeat the line-count budget.
func capLineRunes(lines []string) []string {
	for i, l := range lines {
		if r := []rune(l); len(r) > maxLineRunes {
			lines[i] = string(r[:maxLineRunes]) + lineTruncMarker
		}
	}
	return lines
}

// applyBudget caps a selection at maxActionableLines lines, keeping the first
// keepFirst and the last (maxActionableLines-keepFirst-1) with one elision line
// spliced in the middle so the count stays honest.
func applyBudget(lines []string) []string {
	if len(lines) <= maxActionableLines {
		return lines
	}
	keepLast := maxActionableLines - keepFirst - 1
	elided := len(lines) - keepFirst - keepLast
	out := make([]string, 0, maxActionableLines)
	out = append(out, lines[:keepFirst]...)
	out = append(out, fmt.Sprintf(elisionFmt, elided))
	out = append(out, lines[len(lines)-keepLast:]...)
	return out
}

func splitLines(s string) []string {
	raw := strings.Split(s, "\n")
	for i := range raw {
		raw[i] = strings.TrimSuffix(raw[i], "\r")
	}
	return raw
}

// leftKey normalises a line for prefix matching: leading whitespace and box /
// quote-bar characters (TF's "│ " / "> " diagnostic gutters) stripped, lowered.
func leftKey(line string) string {
	return strings.ToLower(strings.TrimLeft(line, " \t│>"))
}

func firstMarker(lines []string) int {
	for i, l := range lines {
		if isMarker(l) {
			return i
		}
	}
	return -1
}

func isMarker(line string) bool {
	k := leftKey(line)
	for _, m := range tfErrorMarkers {
		if strings.HasPrefix(k, m) {
			return true
		}
	}
	return false
}

func isLog(line string) bool {
	k := leftKey(line)
	for _, p := range tfLogPrefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	for _, p := range tfOTELNoise {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

func dropSpace(r rune) rune {
	if unicode.IsSpace(r) {
		return -1
	}
	return r
}

func lastN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func nonBlank(lines []string) []string {
	var out []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func trimBlankEnds(lines []string) []string {
	start, end := 0, len(lines)
	for start < end && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	return lines[start:end]
}

func allWhitespace(lines []string) bool {
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			return false
		}
	}
	return true
}
