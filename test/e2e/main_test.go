// Package e2e runs crossplane-mcp against a REAL kube-apiserver (envtest:
// apiserver + etcd, no kubelet, no controllers, no scheduler).
//
// It is a separate Go module on purpose. Two things follow from that, and both
// are the point rather than a side effect:
//
//   - controller-runtime and apiextensions-apiserver stay out of the SHIPPED
//     go.mod, so the binary's dependency surface — and therefore what
//     govulncheck gates on — is unchanged. A //go:build tag would not achieve
//     this: go mod tidy resolves imports in tagged files too.
//   - the harness necessarily WRITES (CRDs, CRs, events) while the server under
//     test must only ever read. Keeping the writes in a module golangci-lint
//     never scans means the forbidigo read-only rule keeps its exact semantics
//     with no exclusions and no //nolint.
//
// What this tier is for: the paths that unit tests structurally cannot reach
// because a fake stands in for the apiserver — real discovery caching and
// invalidation, real field selectors, real RBAC, real HTTP/2 transport. It runs
// no controllers, so it says nothing about how Crossplane itself behaves; that
// is the kind + real-Crossplane tier's job.
package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/briferz/crossplane-mcp/internal/k8s"
)

var testEnv *envtest.Environment

// This module serves three tiers that never run together:
//
//	envtest    (default)          apiserver + etcd, no controllers. Fast, runs
//	                              on every PR. Needs KUBEBUILDER_ASSETS.
//	cluster    (CLUSTER_E2E)      a real kind cluster with real controllers and
//	                              a kubelet, but no Crossplane. One external
//	                              image on its critical path.
//	crossplane (CROSSPLANE_E2E)   the above plus real Crossplane, which adds
//	                              three registries. Cron/dispatch/label only.
//
// Selecting by environment rather than build tags keeps one module, one
// `go vet`, and no tag combination that compiles in CI but not locally.
func crossplaneTier() bool { return os.Getenv("CROSSPLANE_E2E") == "1" }

// clusterTier reports whether a real cluster is driving the run. The Crossplane
// tier is a superset — it necessarily has controllers and a kubelet — so it
// implies this one rather than being a separate case every caller must handle.
func clusterTier() bool { return os.Getenv("CLUSTER_E2E") == "1" || crossplaneTier() }

// requireEnvtest skips a test when the process is running against a real
// cluster, where there is no envtest apiserver.
func requireEnvtest(t *testing.T) {
	t.Helper()
	if testEnv == nil {
		t.Skip("envtest not started (running a real-cluster tier)")
	}
}

// requireCrossplane skips a test unless a real Crossplane cluster is available.
func requireCrossplane(t *testing.T) {
	t.Helper()
	if !crossplaneTier() {
		t.Skip("needs a real Crossplane cluster; set CROSSPLANE_E2E=1 with KUBECONFIG pointed at one")
	}
}

func TestMain(m *testing.M) {
	// A real-cluster tier brings its own cluster; envtest must not start.
	if clusterTier() {
		os.Exit(m.Run())
	}

	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Fprintln(os.Stderr,
			"KUBEBUILDER_ASSETS is unset — run via `make e2e-envtest`, which resolves the\n"+
				"apiserver/etcd binaries with setup-envtest. Skipping.")
		os.Exit(0)
	}

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("testdata", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	if _, err := testEnv.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "starting envtest: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	if err := testEnv.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stopping envtest: %v\n", err)
	}
	os.Exit(code)
}

// newServerClient builds the client the way cmd/crossplane-mcp does — through
// k8s.New and a kubeconfig on disk — rather than assembling the struct by hand.
// That matters: the memory discovery cache and the deferred RESTMapper are
// created inside New, and they are precisely what the invalidation test needs
// to be real.
func newServerClient(t *testing.T, cfg *rest.Config) *k8s.Client {
	t.Helper()
	path := writeKubeconfig(t, cfg)
	cl, err := k8s.New(path, "", 30*time.Second)
	if err != nil {
		t.Fatalf("k8s.New: %v", err)
	}
	return cl
}

// writeKubeconfig serialises a rest.Config to a kubeconfig file, so the server
// is exercised through its real auth-loading path instead of being handed a
// pre-built config.
func writeKubeconfig(t *testing.T, cfg *rest.Config) string {
	t.Helper()
	c := clientcmdapi.NewConfig()
	c.Clusters["envtest"] = &clientcmdapi.Cluster{
		Server:                   cfg.Host,
		CertificateAuthorityData: cfg.CAData,
		InsecureSkipTLSVerify:    cfg.CAData == nil && cfg.Insecure,
	}
	c.AuthInfos["user"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: cfg.CertData,
		ClientKeyData:         cfg.KeyData,
		Token:                 cfg.BearerToken,
		Username:              cfg.Username,
		Password:              cfg.Password,
	}
	c.Contexts["envtest"] = &clientcmdapi.Context{Cluster: "envtest", AuthInfo: "user"}
	c.CurrentContext = "envtest"

	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*c, path); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}
