package xp

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/briferz/crossplane-mcp/internal/k8s"
)

// gzBytes gzips payload, producing one complete gzip member (deterministic — no
// cluster, no randomness).
func gzBytes(t *testing.T, payload string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(payload)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// gzipB64 is the base64-of-gzip payload the provider embeds in its shell hint.
func gzipB64(t *testing.T, payload string) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString(gzBytes(t, payload))
}

// tofuHint wraps a base64 payload in the literal provider-terraform shell hint.
func tofuHint(b64 string) string {
	return `create failed: cannot apply tofu configuration. To see the full error run: echo "` + b64 + `" | base64 -d | gunzip`
}

func TestIsMarkerPrecision(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{"Error: real failure", true},
		{"  Error: indented", true},
		{"│ Error: box-prefixed", true},
		{"Summary: failed to create", true},
		{"  summary: lowercased", true},
		{"error establishing connection (transient)", false}, // "error " not "error:"
		{"WARNING: deprecation notice", false},
		{"[ERROR] bracketed log noise", false},
		{"[INFO]  OpenTofu version: 1.10.0", false},
		{"some prose mentioning error: inline", false}, // not at line start
	}
	for _, c := range cases {
		if got := isMarker(c.line); got != c.want {
			t.Errorf("isMarker(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

func TestIsLog(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{"[TRACE] OpenTelemetry boot", true},
		{"[debug] using github.com/...", true},
		{"[INFO]  CLI args", true},
		{"[WARN] something", true},
		{"[ERROR] noise", true},
		{"│ [DEBUG] box-prefixed log", true},
		{"[TRACE] OTEL_TRACES_EXPORTER not set", true},
		{"Error: a real error", false},
		{"  with module.x.resource", false},
	}
	for _, c := range cases {
		if got := isLog(c.line); got != c.want {
			t.Errorf("isLog(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

func TestExtractActionable_MarkerBlockKeptWhole(t *testing.T) {
	decoded := strings.Join([]string{
		`Error: failed to create provider config resource: status code 400, response: {"x":1}`,
		`  with module.x.resource.name,`,
		`  on main.tf line 42, in resource "foo" "bar":`,
		`  42:   resource "foo" "bar" {`,
	}, "\n")
	got := extractActionable(decoded)
	if len(got) != 4 {
		t.Fatalf("expected 4 lines, got %d: %v", len(got), got)
	}
	for i, want := range strings.Split(decoded, "\n") {
		if got[i] != want {
			t.Errorf("line %d: got %q, want %q (must be byte-identical, never truncated)", i, got[i], want)
		}
	}
}

func TestExtractActionable_ErrorBuriedAfterBoilerplate(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&b, "[DEBUG] using github.com/hashicorp/dep v%d\n", i)
	}
	b.WriteString("[INFO]  OpenTofu version: 1.10.0\n")
	b.WriteString("Error: the real failure is here\n")
	b.WriteString("  on main.tf line 9\n")
	b.WriteString("[DEBUG] cleanup after error\n") // log AFTER the marker must also be dropped

	got := extractActionable(b.String())
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "Error: the real failure is here") || !strings.Contains(joined, "on main.tf line 9") {
		t.Fatalf("actionable error not surfaced: %v", got)
	}
	if strings.Contains(joined, "[DEBUG]") || strings.Contains(joined, "[INFO]") {
		t.Errorf("boilerplate leaked into output: %v", got)
	}
}

func TestExtractActionable_NoMarkerFallbackTailKeep(t *testing.T) {
	var lines []string
	for i := 0; i < 5; i++ {
		lines = append(lines, "[debug] noise")
	}
	for i := 1; i <= 25; i++ {
		lines = append(lines, fmt.Sprintf("data %d", i))
	}
	got := extractActionable(strings.Join(lines, "\n"))
	if len(got) != fallbackKeepLines {
		t.Fatalf("expected %d fallback lines, got %d", fallbackKeepLines, len(got))
	}
	if got[0] != "data 6" || got[len(got)-1] != "data 25" {
		t.Errorf("fallback should keep the TAIL survivors; got first=%q last=%q", got[0], got[len(got)-1])
	}
	if strings.Contains(strings.Join(got, "\n"), "[debug]") {
		t.Errorf("fallback must drop log lines: %v", got)
	}
}

func TestExtractActionable_FallbackAllFilteredKeepsRawTail(t *testing.T) {
	var lines []string
	for i := 1; i <= 30; i++ {
		lines = append(lines, fmt.Sprintf("[info] line %d", i))
	}
	got := extractActionable(strings.Join(lines, "\n"))
	if len(got) == 0 {
		t.Fatal("must never return empty when the blob has content")
	}
	if len(got) != fallbackKeepLines {
		t.Errorf("expected %d raw-tail lines, got %d", fallbackKeepLines, len(got))
	}
}

func TestExtractActionable_BudgetElision(t *testing.T) {
	// Exactly maxActionableLines → no elision.
	atCap := []string{"Error: head"}
	for i := 1; i < maxActionableLines; i++ {
		atCap = append(atCap, fmt.Sprintf("  ctx %d", i))
	}
	got := extractActionable(strings.Join(atCap, "\n"))
	if len(got) != maxActionableLines {
		t.Fatalf("at-cap: expected %d lines, got %d", maxActionableLines, len(got))
	}
	if strings.Contains(strings.Join(got, "\n"), "elided") {
		t.Errorf("at-cap must not elide: %v", got)
	}

	// One over the cap → first keepFirst + 1 elision + last (cap-keepFirst-1).
	over := append(append([]string{}, atCap...), "  ctx tail")
	got = extractActionable(strings.Join(over, "\n"))
	if len(got) != maxActionableLines {
		t.Fatalf("over-cap: expected %d lines total, got %d", maxActionableLines, len(got))
	}
	wantElided := len(over) - keepFirst - (maxActionableLines - keepFirst - 1)
	wantMarker := fmt.Sprintf(elisionFmt, wantElided)
	if got[keepFirst] != wantMarker {
		t.Errorf("expected elision marker %q at index %d, got %q", wantMarker, keepFirst, got[keepFirst])
	}
	if got[0] != "Error: head" || got[len(got)-1] != "  ctx tail" {
		t.Errorf("budget must keep head and tail; got first=%q last=%q", got[0], got[len(got)-1])
	}
}

func TestExtractActionable_EmptyAndWhitespaceNil(t *testing.T) {
	for _, in := range []string{"", "\n\n   \n", "   "} {
		if got := extractActionable(in); got != nil {
			t.Errorf("extractActionable(%q) = %v, want nil", in, got)
		}
	}
}

func TestExtractActionable_CRLFTrimmedIndentationPreserved(t *testing.T) {
	got := extractActionable("Error: boom\r\n  with module.x\r\n")
	if len(got) != 2 {
		t.Fatalf("expected 2 lines, got %d: %v", len(got), got)
	}
	if got[0] != "Error: boom" {
		t.Errorf("CR not trimmed: %q", got[0])
	}
	if got[1] != "  with module.x" {
		t.Errorf("indentation not preserved or CR not trimmed: %q", got[1])
	}
}

func TestExtractActionable_LongLineCapped(t *testing.T) {
	// A single huge line (e.g. an Error: with a multi-KB inline body) must be
	// capped so the line-count budget can't be defeated by one line.
	long := "Error: " + strings.Repeat("x", 10000)
	got := extractActionable(long)
	if len(got) != 1 {
		t.Fatalf("expected 1 line, got %d", len(got))
	}
	if r := []rune(got[0]); len(r) > maxLineRunes+len([]rune(lineTruncMarker)) {
		t.Errorf("line not capped: %d runes", len(r))
	}
	if !strings.HasPrefix(got[0], "Error: x") || !strings.HasSuffix(got[0], lineTruncMarker) {
		t.Errorf("capped line should keep the head and carry the marker: %.40q…", got[0])
	}
	// Multibyte safety: a cut must land on a rune boundary, not split a rune.
	box := strings.Repeat("│", 8000) // 3 bytes each
	if got := extractActionable("Error: " + box); !strings.HasSuffix(got[0], lineTruncMarker) {
		t.Error("multibyte line should cap cleanly")
	}
}

func TestDecodeTFErrors_BlobInEventOnly(t *testing.T) {
	// The blob lives only in an event message; the condition is plain text.
	got := decodeTFErrors(
		[]string{"Synced: ReconcileError — cannot apply tofu configuration"},
		[]k8s.Event{{Message: tofuHint(gzipB64(t, "Error: from-event\n  on x.tf line 1"))}},
		map[string]bool{},
	)
	if len(got) != 1 || !strings.Contains(got[0], "Error: from-event") {
		t.Fatalf("event-only blob must decode to 1 entry, got %v", got)
	}
}

func TestDecodeTFErrors_DistinctBlobsBothSurface(t *testing.T) {
	// Two DISTINCT blobs must both surface — the dedup seen-set must only
	// suppress byte-identical duplicates, never distinct errors.
	a := tofuHint(gzipB64(t, "Error: alpha\n  on a.tf line 1"))
	b := tofuHint(gzipB64(t, "Error: beta\n  on b.tf line 2"))
	got := decodeTFErrors([]string{"cond " + a, "cond " + b}, nil, map[string]bool{})
	if len(got) != 2 {
		t.Fatalf("two distinct blobs must both surface, got %d: %v", len(got), got)
	}
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "Error: alpha") || !strings.Contains(joined, "Error: beta") {
		t.Errorf("both distinct errors must be present: %v", got)
	}
}

func TestTFBlobPayloads_QuoteAndCommandVariants(t *testing.T) {
	b64 := gzipB64(t, "Error: x")
	variants := []string{
		`echo "` + b64 + `" | base64 -d | gunzip`,
		`echo '` + b64 + `' | base64 --decode | gunzip`,
		`echo "` + b64 + `" | base64 -d | gzip -d`,
		`echo "` + b64 + `" | base64 -d | gzip -dc`,
		`echo "` + b64 + `" | base64 -d | gunzip -c`,
		`echo "` + b64 + `" | base64 -d | zcat`,
	}
	for _, v := range variants {
		got := tfBlobPayloads(v)
		if len(got) != 1 || got[0] != b64 {
			t.Errorf("variant %q: got %v, want [%s]", v, got, b64)
		}
	}
}

func TestTFBlobPayloads_MultipleBlobsAndWhitespace(t *testing.T) {
	b64 := gzipB64(t, "Error: y")
	two := tofuHint(b64) + " ... and also " + tofuHint(b64)
	if got := tfBlobPayloads(two); len(got) != 2 {
		t.Errorf("expected 2 blobs, got %d", len(got))
	}
	// Line-wrapped payload: internal whitespace must be stripped to the original.
	wrapped := `echo "` + b64[:8] + "\n   " + b64[8:] + `" | base64 -d | gunzip`
	got := tfBlobPayloads(wrapped)
	if len(got) != 1 || got[0] != b64 {
		t.Errorf("wrapped payload: got %v, want [%s]", got, b64)
	}
}

func TestTFBlobPayloads_RejectsNonGzipAndOverMatch(t *testing.T) {
	// Plain base64 not starting with the gzip-magic prefix is not captured.
	if got := tfBlobPayloads(`echo "Zm9vYmFy" | base64 -d | gunzip`); got != nil {
		t.Errorf("non-gzip base64 must not match, got %v", got)
	}
	// Lazy, quote-excluding capture stops at the first closing quote.
	got := tfBlobPayloads(`echo "H4sIAAAAfoo" | base64 -d | gunzip then "other"`)
	if len(got) != 1 || got[0] != "H4sIAAAAfoo" {
		t.Errorf("over-match: got %v, want [H4sIAAAAfoo]", got)
	}
}

func TestDecodeBlob_HappyPath(t *testing.T) {
	payload := "[INFO] boot\nError: boom\n  on main.tf line 5"
	got, ok := decodeBlob(gzipB64(t, payload))
	if !ok {
		t.Fatal("expected ok")
	}
	if got != payload {
		t.Errorf("round-trip mismatch:\n got %q\nwant %q", got, payload)
	}
}

func TestDecodeBlob_RawStdFallback(t *testing.T) {
	// Find a payload whose base64 has padding, then strip it so StdEncoding
	// fails and the RawStdEncoding fallback must kick in.
	var payload, raw string
	for _, p := range []string{"Error: x", "Error: xy", "Error: xyz", "Error: abcd", "Error: abcde"} {
		std := gzipB64(t, p)
		if strings.HasSuffix(std, "=") {
			payload, raw = p, strings.TrimRight(std, "=")
			break
		}
	}
	if raw == "" {
		t.Skip("no padded fixture found")
	}
	got, ok := decodeBlob(raw)
	if !ok || got != payload {
		t.Errorf("RawStd fallback: ok=%v got=%q want=%q", ok, got, payload)
	}
}

func TestDecodeBlob_TrailingJunk(t *testing.T) {
	gz := gzBytes(t, "Error: complete")
	junked := append(append([]byte{}, gz...), []byte("TRAILINGJUNK")...)
	got, ok := decodeBlob(base64.StdEncoding.EncodeToString(junked))
	if !ok || got != "Error: complete" {
		t.Errorf("trailing junk after a complete member must decode cleanly: ok=%v got=%q", ok, got)
	}
	if strings.Contains(got, incompleteMarker) {
		t.Errorf("a complete member must not be marked incomplete: %q", got)
	}
}

func TestDecodeBlob_TruncatedKeepsPartial(t *testing.T) {
	gz := gzBytes(t, "Error: real actionable error")
	truncated := gz[:len(gz)-8] // drop the 8-byte trailer; deflate body stays complete
	got, ok := decodeBlob(base64.StdEncoding.EncodeToString(truncated))
	if !ok {
		t.Fatal("a decoded-but-trailerless member must not be dropped")
	}
	if !strings.Contains(got, "Error: real actionable error") {
		t.Errorf("decoded content lost on truncated member: %q", got)
	}
	if !strings.Contains(got, incompleteMarker) {
		t.Errorf("expected incomplete marker on a truncated member: %q", got)
	}
}

func TestDecodeBlob_MalformedAndNonGzip(t *testing.T) {
	if _, ok := decodeBlob("not!!base64"); ok {
		t.Error("malformed base64 must return ok=false")
	}
	if _, ok := decodeBlob(base64.StdEncoding.EncodeToString([]byte("hello, not gzip"))); ok {
		t.Error("valid base64 of non-gzip bytes must return ok=false")
	}
	if _, ok := decodeBlob(""); ok {
		t.Error("empty input must return ok=false")
	}
}

func TestDecodeBlob_OversizeInputRejected(t *testing.T) {
	big := make([]byte, maxCompressedBytes+1)
	big[0], big[1] = 0x1f, 0x8b // gzip magic; size check still rejects first
	if _, ok := decodeBlob(base64.StdEncoding.EncodeToString(big)); ok {
		t.Error("compressed input over the cap must be rejected before decompression")
	}
}

func TestDecodeBlob_OverflowTruncationMarker(t *testing.T) {
	payload := strings.Repeat("filler line of provider boilerplate\n", 130000) // > 4 MiB decompressed
	got, ok := decodeBlob(gzipB64(t, payload))
	if !ok {
		t.Fatal("expected ok for an oversized-but-valid stream")
	}
	if !strings.HasSuffix(got, truncationMarker) {
		t.Errorf("expected truncation marker suffix on overflow")
	}
	if len(got) > maxDecompressedBytes+len(truncationMarker)+1 {
		t.Errorf("output not capped: len=%d", len(got))
	}
}

func TestDecodeTFErrors_DedupConditionAndEvent(t *testing.T) {
	b64 := gzipB64(t, "Error: boom\n  on main.tf line 5")
	msg := tofuHint(b64)
	got := decodeTFErrors([]string{"Synced: ReconcileError — " + msg}, []k8s.Event{{Message: msg}}, map[string]bool{})
	if len(got) != 1 {
		t.Fatalf("identical blob in condition and event must dedup to 1, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "Error: boom") {
		t.Errorf("decoded entry missing the error: %q", got[0])
	}
}

func TestDecodeTFErrors_NilWhenNoHint(t *testing.T) {
	got := decodeTFErrors(
		[]string{"Synced: ReconcileError — AccessDenied: invalid credentials"},
		[]k8s.Event{{Message: "Created bucket"}},
		map[string]bool{},
	)
	if got != nil {
		t.Errorf("no hint → nil, got %v", got)
	}
}
