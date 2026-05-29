package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"
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
