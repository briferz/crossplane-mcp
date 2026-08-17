package main

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This project makes two promises that live in the PROTOCOL rather than in any
// response body: the server publishes read-only Instructions, and every tool
// declares readOnlyHint. Both are set at construction — and both were, until
// now, asserted nowhere end to end. Setting a field is not the promise; the
// client receiving it is.
//
// That gap is not theoretical. go-sdk v1.7.0 negotiates protocol 2026-07-28,
// which REPLACES the initialize/initialized handshake with a server/discover
// RPC — exactly the exchange Instructions used to arrive on. The SDK ships
// seven MCPGODEBUG escape hatches restoring older behaviour, all scheduled for
// removal in v1.9.0. So the compatibility shims that make this work today are
// explicitly temporary, and without this test the next bump could drop either
// promise with every check still green.
//
// A nil *k8s.Client is deliberate and safe: Register only closes over it inside
// handler factories and never dereferences it, and this test performs the
// handshake and lists tools without ever invoking one. Building a fake cluster
// here would test the fake, not the protocol.
func connectProbe(t *testing.T) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()

	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := newServer(nil, nil).Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "probe", Version: "0"}, nil).
		Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// TestInstructionsReachTheClient pins the read-only promise at the protocol
// boundary. The Instructions text is how a client learns this server never
// mutates anything, so it silently failing to arrive is a correctness problem,
// not a cosmetic one.
func TestInstructionsReachTheClient(t *testing.T) {
	cs := connectProbe(t)

	res := cs.InitializeResult()
	if res == nil {
		t.Fatal("no initialize result — the client never completed the handshake")
	}
	t.Logf("negotiated protocol version: %q", res.ProtocolVersion)

	if res.Instructions != serverInstructions {
		t.Fatalf("Instructions did not survive the handshake.\n got: %q\nwant: %q",
			res.Instructions, serverInstructions)
	}
	// The specific sentence carrying the promise. Asserted separately so a future
	// rewrite of the Instructions cannot quietly drop it while the equality check
	// above still passes against the new text.
	if !strings.Contains(res.Instructions, "Every tool is read-only") {
		t.Error("the Instructions no longer state the read-only promise")
	}
}

// TestEveryToolDeclaresReadOnlyHint asserts the annotation as the client sees
// it. internal/tools already checks this against its own registration; this
// checks it survived serialisation and the handshake, which is the part a
// protocol change can break.
func TestEveryToolDeclaresReadOnlyHint(t *testing.T) {
	cs := connectProbe(t)

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(res.Tools) == 0 {
		t.Fatal("no tools listed, so this test asserted nothing")
	}
	for _, tool := range res.Tools {
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("tool %q does not declare readOnlyHint to the client", tool.Name)
		}
	}
	t.Logf("verified readOnlyHint on %d tools", len(res.Tools))
}
