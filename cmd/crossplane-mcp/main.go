// Command crossplane-mcp is a read-only diagnostic MCP server for Crossplane.
// It exposes Crossplane-aware tools (diagnose, get_resource_tree, get_resource,
// list_contexts) over stdio for use by an MCP client such as Claude.
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

func main() {
	var (
		kubeconfig  = flag.String("kubeconfig", "", "path to kubeconfig (defaults to KUBECONFIG / ~/.kube/config; in-cluster if absent)")
		kubeContext = flag.String("context", "", "kubeconfig context to use (defaults to current-context)")
	)
	flag.Parse()

	cl, err := k8s.New(*kubeconfig, *kubeContext)
	if err != nil {
		fmt.Fprintf(os.Stderr, "crossplane-mcp: %v\n", err)
		os.Exit(1)
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "crossplane-mcp",
		Version: version,
	}, nil)
	tools.Register(server, cl)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintf(os.Stderr, "crossplane-mcp: %v\n", err)
		os.Exit(1)
	}
}
