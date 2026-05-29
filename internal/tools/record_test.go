package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

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
