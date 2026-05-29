package tools

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Recorder appends a JSONL record per tool call — its input, output, duration,
// and any error — so a session against a real cluster can be inspected later.
// Enabled via --log-file / CROSSPLANE_MCP_LOG_FILE.
//
// It records the full tool input and output. The server never reads Kubernetes
// Secret objects, but the output it logs is not sanitised: `get_resource`
// includes the resource's `spec`, which on Crossplane resources can carry inline
// sensitive fields (provider config, connection parameters, credentials), and
// provider error messages can contain identifiers (account IDs, ARNs, …). Treat
// the log as potentially sensitive and review it before sharing off a machine
// that touches production.
type Recorder struct {
	mu sync.Mutex
	w  io.Writer
	c  io.Closer
}

// NewRecorder opens dest for appending. dest "-" or "stderr" writes to stderr;
// anything else is treated as a file path (created if absent, mode 0600).
func NewRecorder(dest string) (*Recorder, error) {
	if dest == "-" || dest == "stderr" {
		return &Recorder{w: os.Stderr}, nil
	}
	// #nosec G304 -- dest is an operator-provided log path (flag/env), not attacker-controlled.
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // operator-provided log path
	if err != nil {
		return nil, err
	}
	return &Recorder{w: f, c: f}, nil
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
		Input:      in,
	}
	if callErr != nil {
		rec.Error = callErr.Error()
	} else {
		rec.Output = out
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, _ = r.w.Write(append(b, '\n'))
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
