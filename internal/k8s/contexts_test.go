package k8s

import (
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// This pins the SHAPE Contexts returns for a loader with no contexts: an empty,
// non-nil slice, not nil. It does not demonstrate the pod path. That rests on
// reading client-go v0.37.0 (merged_client_builder.go): its deferred loader
// falls back to in-cluster config INSIDE a successful ClientConfig() call, so an
// ordinary pod keeps a non-nil loader with no contexts. It cannot be exercised
// here, because client-go keys in-cluster detection on a hardcoded
// service-account token path. The consequence for list_contexts — which used to
// test for nil and so reported "not in-cluster" in that case — is pinned in
// tools TestInClusterKeysOnNoContextsNotNil.
func TestContextsOfAnEmptyKubeconfigIsEmptyNotNil(t *testing.T) {
	cl := &Client{loader: clientcmd.NewDefaultClientConfig(*clientcmdapi.NewConfig(), &clientcmd.ConfigOverrides{})}

	ctxs, err := cl.Contexts()
	if err != nil {
		t.Fatalf("Contexts: %v", err)
	}
	if ctxs == nil {
		t.Fatal("expected an empty non-nil slice: that is the shape an in-cluster pod produces, " +
			"and the one a nil check misses")
	}
	if len(ctxs) != 0 {
		t.Fatalf("an empty kubeconfig has no contexts, got %v", ctxs)
	}
}
