package tools

import (
	"context"
	"strings"
	"testing"
)

// Client.Get's scope branch ran in no test: mutation testing showed that both
// collapsing it to always-namespace AND silently defaulting to "default" — the
// behaviour its doc comment says it deliberately avoids — passed the whole
// suite. Under Crossplane v1 every XR and MR is cluster-scoped, so this is the
// dominant Get path for the entire v1/LegacyCluster world the project promises
// to support.

// TestGetResourceHandlerClusterScoped pins the non-namespaced branch: a
// cluster-scoped kind fetched with no namespace must succeed and stay
// namespace-less.
func TestGetResourceHandlerClusterScoped(t *testing.T) {
	h := getResourceHandler(treeClient(t))

	_, out, err := h(context.Background(), nil, GetResourceInput{
		APIVersion: "legacy.example.org/v1", Kind: "XCluster", Name: "cluster1",
	})
	if err != nil {
		t.Fatalf("cluster-scoped get: %v", err)
	}
	if out.Name != "cluster1" || out.Kind != "XCluster" {
		t.Errorf("got %s/%s, want XCluster/cluster1", out.Kind, out.Name)
	}
	if out.Namespace != "" {
		t.Errorf("a cluster-scoped resource must carry no namespace, got %q", out.Namespace)
	}
}

// TestGetResourceHandlerNamespaceRequired pins the guard: a namespaced kind with
// no namespace must error rather than silently assume "default", which would
// return the wrong object or a confusing NotFound.
func TestGetResourceHandlerNamespaceRequired(t *testing.T) {
	h := getResourceHandler(treeClient(t))

	_, _, err := h(context.Background(), nil, GetResourceInput{
		APIVersion: "apps.example.org/v1", Kind: "XApp", Name: "app1",
	})
	if err == nil {
		t.Fatal("expected an error when a namespaced kind is fetched without a namespace")
	}
	if !strings.Contains(err.Error(), "namespace is required") {
		t.Errorf("error should name the missing namespace, got %q", err)
	}
}
