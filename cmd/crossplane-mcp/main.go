// Command crossplane-mcp is a read-only diagnostic MCP server for Crossplane.
// It exposes Crossplane-aware tools (diagnose, list_unhealthy,
// get_resource_tree, get_resource, list_contexts) over stdio for use by an MCP
// client such as Claude.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/briferz/crossplane-mcp/internal/k8s"
	"github.com/briferz/crossplane-mcp/internal/tools"
)

// version is overridable at build time with -ldflags "-X main.version=...".
var version = "0.1.0-dev"

// serverInstructions teaches any connected MCP client the intended diagnostic
// workflow up front, instead of relying on out-of-band docs.
const serverInstructions = `Read-only Crossplane diagnostics for a Kubernetes cluster.

Recommended workflow:
1. list_unhealthy — when you don't know what is broken: lists composite resources (XRs) and claims that are not Ready/Synced, as tiny rows ready to feed into diagnose.
2. diagnose — when you know the resource (e.g. a row from list_unhealthy): walks its XR -> managed-resource tree and ranks the deepest blocking resource first, with full condition messages, recent events, decoded provider errors, and lifecycle labels (Terminating/Creating/Paused).
3. get_resource / get_resource_tree — drill into one resource's conditions+events+spec, or view the whole tree structure.
Use list_contexts to see the available kubeconfig contexts (the server is pinned to one context per process).

Every tool is read-only: only Kubernetes get/list requests are issued, nothing is ever mutated, and Secret values are never read.`

func main() {
	var (
		kubeconfig  = flag.String("kubeconfig", "", "path to kubeconfig (defaults to KUBECONFIG / ~/.kube/config; in-cluster if absent)")
		kubeContext = flag.String("context", "", "kubeconfig context to use (defaults to current-context)")
		logFile     = flag.String("log-file", "", "append a JSONL record of each tool call (input+output) to this path, or '-' for stderr; also via CROSSPLANE_MCP_LOG_FILE")
		logRedact   = flag.Bool("log-redact", true, "mask scalar values under sensitive keys (password/token/secret/…) in the log; also via CROSSPLANE_MCP_LOG_REDACT=false")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	// Optional tool-call recording (flag wins over env).
	logDest := *logFile
	if logDest == "" {
		logDest = os.Getenv("CROSSPLANE_MCP_LOG_FILE")
	}
	var rec *tools.Recorder
	if logDest != "" {
		redact := *logRedact
		if v := os.Getenv("CROSSPLANE_MCP_LOG_REDACT"); v == "false" || v == "0" {
			redact = false
		}
		r, err := tools.NewRecorder(logDest, redact)
		if err != nil {
			fmt.Fprintf(os.Stderr, "crossplane-mcp: open log file: %v\n", err)
			os.Exit(1)
		}
		rec = r
		defer func() { _ = rec.Close() }()
	}

	cl, err := k8s.New(*kubeconfig, *kubeContext)
	if err != nil {
		fmt.Fprintf(os.Stderr, "crossplane-mcp: %v\n", err)
		os.Exit(1)
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "crossplane-mcp",
		Version: version,
	}, &mcp.ServerOptions{Instructions: serverInstructions})
	tools.Register(server, cl, rec)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintf(os.Stderr, "crossplane-mcp: %v\n", err)
		os.Exit(1)
	}
}
