package tools

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/briferz/crossplane-mcp/internal/k8s"
	"github.com/briferz/crossplane-mcp/internal/xp"
)

// pkgU builds a cluster-scoped package/revision object with a uid (the fake
// event lister and the revision correlation both key on it).
func pkgU(apiVersion, kind, name, uid string, conds ...map[string]any) *unstructured.Unstructured {
	o := uobj(apiVersion, kind, "", name, conds...)
	o.SetUID(types.UID(uid))
	return o
}

// packagesClient models a mixed package landscape:
//   - Provider provider-aws: healthy, one Active healthy revision.
//   - Provider provider-gcp: Installed=False (unpack failure), one revision.
//   - Function fn-patch: served ONLY at v1beta1 (a Crossplane 1.14–1.16-shaped
//     cluster), healthy.
//   - Configuration cfg-platform: healthy, no revisions.
//   - A third-party decoy "Provider" in another group that reuses category pkg
//     — it must never leak into list_providers.
func packagesClient() *k8s.Client {
	resources := []*metav1.APIResourceList{
		{GroupVersion: "pkg.crossplane.io/v1", APIResources: []metav1.APIResource{
			{Name: "providers", Kind: "Provider", Namespaced: false, Categories: []string{"crossplane", "pkg"}},
			{Name: "providerrevisions", Kind: "ProviderRevision", Namespaced: false, Categories: []string{"crossplane", "pkgrev"}},
			{Name: "providers/status", Kind: "Provider", Namespaced: false, Categories: []string{"crossplane", "pkg"}},
			{Name: "configurations", Kind: "Configuration", Namespaced: false, Categories: []string{"crossplane", "pkg"}},
			{Name: "configurationrevisions", Kind: "ConfigurationRevision", Namespaced: false, Categories: []string{"crossplane", "pkgrev"}},
		}},
		{GroupVersion: "pkg.crossplane.io/v1beta1", APIResources: []metav1.APIResource{
			{Name: "functions", Kind: "Function", Namespaced: false, Categories: []string{"crossplane", "pkg"}},
			{Name: "functionrevisions", Kind: "FunctionRevision", Namespaced: false, Categories: []string{"crossplane", "pkgrev"}},
		}},
		{GroupVersion: "decoy.example.org/v1", APIResources: []metav1.APIResource{
			{Name: "providers", Kind: "Provider", Namespaced: false, Categories: []string{"pkg"}},
		}},
	}
	disco := &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{Resources: resources}}

	awsRev := pkgU("pkg.crossplane.io/v1", "ProviderRevision", "provider-aws-1234", "rev-aws",
		condM("RevisionHealthy", "True"), condM("RuntimeHealthy", "True"))
	awsRev.SetLabels(map[string]string{"pkg.crossplane.io/package": "provider-aws"})
	_ = unstructured.SetNestedField(awsRev.Object, "Active", "spec", "desiredState")
	_ = unstructured.SetNestedField(awsRev.Object, int64(1), "spec", "revision")

	gcpRev := pkgU("pkg.crossplane.io/v1", "ProviderRevision", "provider-gcp-9999", "rev-gcp",
		condM("RevisionHealthy", "False"))
	gcpRev.SetLabels(map[string]string{"pkg.crossplane.io/package": "provider-gcp"})
	_ = unstructured.SetNestedField(gcpRev.Object, "Active", "spec", "desiredState")
	_ = unstructured.SetNestedField(gcpRev.Object, int64(1), "spec", "revision")

	aws := pkgU("pkg.crossplane.io/v1", "Provider", "provider-aws", "uid-aws",
		condM("Installed", "True"), condM("Healthy", "True"))
	_ = unstructured.SetNestedField(aws.Object, "xpkg.example.org/provider-aws:v1", "spec", "package")
	_ = unstructured.SetNestedField(aws.Object, "provider-aws-1234", "status", "currentRevision")

	gcp := pkgU("pkg.crossplane.io/v1", "Provider", "provider-gcp", "uid-gcp",
		map[string]any{"type": "Installed", "status": "False", "reason": "UnpackingPackage",
			"message": "cannot unpack package: GET https://xpkg.example.org/...: UNAUTHORIZED"})
	_ = unstructured.SetNestedField(gcp.Object, "xpkg.example.org/provider-gcp:v2", "spec", "package")
	_ = unstructured.SetNestedField(gcp.Object, "provider-gcp-9999", "status", "currentRevision")

	fn := pkgU("pkg.crossplane.io/v1beta1", "Function", "fn-patch", "uid-fn",
		condM("Installed", "True"), condM("Healthy", "True"))
	_ = unstructured.SetNestedField(fn.Object, "xpkg.example.org/fn-patch:v0.9", "spec", "package")

	cfg := pkgU("pkg.crossplane.io/v1", "Configuration", "cfg-platform", "uid-cfg",
		condM("Installed", "True"), condM("Healthy", "True"))
	_ = unstructured.SetNestedField(cfg.Object, "xpkg.example.org/cfg-platform:v3", "spec", "package")

	decoy := pkgU("decoy.example.org/v1", "Provider", "impostor", "uid-decoy", condM("Installed", "False"))

	gcpEvent := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Event",
		"metadata":       map[string]any{"name": "gcp-unpack.1", "namespace": "default"},
		"involvedObject": map[string]any{"uid": "uid-gcp"},
		"type":           "Warning", "reason": "UnpackPackage",
		"message": "cannot unpack package: GET https://xpkg.example.org/...: UNAUTHORIZED",
		"count":   int64(12), "lastTimestamp": "2026-06-10T00:00:00Z",
	}}

	gvrToListKind := map[schema.GroupVersionResource]string{
		{Group: "pkg.crossplane.io", Version: "v1", Resource: "providers"}:              "ProviderList",
		{Group: "pkg.crossplane.io", Version: "v1", Resource: "providerrevisions"}:      "ProviderRevisionList",
		{Group: "pkg.crossplane.io", Version: "v1", Resource: "configurations"}:         "ConfigurationList",
		{Group: "pkg.crossplane.io", Version: "v1", Resource: "configurationrevisions"}: "ConfigurationRevisionList",
		{Group: "pkg.crossplane.io", Version: "v1beta1", Resource: "functions"}:         "FunctionList",
		{Group: "pkg.crossplane.io", Version: "v1beta1", Resource: "functionrevisions"}: "FunctionRevisionList",
		{Group: "decoy.example.org", Version: "v1", Resource: "providers"}:              "ProviderList",
		{Group: "", Version: "v1", Resource: "events"}:                                  "EventList",
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToListKind,
		aws, gcp, fn, cfg, decoy, awsRev, gcpRev, gcpEvent)
	return &k8s.Client{Dyn: dyn, Disco: disco}
}

func TestListProvidersHandler(t *testing.T) {
	h := listPackagesHandler(packagesClient(), "Provider", "ProviderRevision")
	_, out, err := h(context.Background(), nil, ListPackagesInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Scanned != 2 || out.Summary.Blocked != 1 || out.Summary.Ready != 1 {
		t.Fatalf("expected 2 providers (1 blocked, 1 ready), got scanned=%d summary=%+v", out.Scanned, out.Summary)
	}
	if len(out.Items) != 2 {
		t.Fatalf("default must list ALL providers, got %d", len(out.Items))
	}
	// Blocked first.
	broken, healthy := out.Items[0], out.Items[1]
	if broken.Name != "provider-gcp" || broken.State != xp.StateBlocked {
		t.Fatalf("expected provider-gcp blocked first, got %+v", broken)
	}
	if healthy.Name != "provider-aws" || healthy.State != xp.StateReady {
		t.Fatalf("expected provider-aws ready second, got %+v", healthy)
	}
	// Kind isolation + the cross-group decoy: neither Function, Configuration,
	// nor the decoy Provider may appear.
	for _, it := range out.Items {
		if it.Kind != "Provider" || it.Name == "impostor" {
			t.Errorf("non-Provider or decoy leaked into list_providers: %+v", it)
		}
	}
	// The broken provider carries the full condition message, its revision row,
	// and the UnpackPackage event from the "default" namespace.
	if len(broken.Reasons) == 0 || !strings.Contains(broken.Reasons[0], "UNAUTHORIZED") {
		t.Errorf("full unpack error expected in reasons, got %v", broken.Reasons)
	}
	if len(broken.Revisions) != 1 || broken.Revisions[0].RevisionHealthy != "False" {
		t.Errorf("the failing revision row should render, got %+v", broken.Revisions)
	}
	if len(broken.Events) != 1 || broken.Events[0].Reason != "UnpackPackage" || broken.Events[0].Count != 12 {
		t.Errorf("the UnpackPackage event should attach with its count, got %+v", broken.Events)
	}
	// The healthy provider stays tiny.
	if healthy.Events != nil || healthy.Revisions != nil || healthy.Reasons != nil {
		t.Errorf("healthy row must carry no enrichment, got %+v", healthy)
	}
	if healthy.RevisionCount == nil || *healthy.RevisionCount != 1 {
		t.Errorf("healthy row should still count its revisions, got %v", healthy.RevisionCount)
	}
}

// TestListFunctionsHandlerV1beta1 proves a Crossplane 1.14–1.16-shaped cluster
// (Function served only at v1beta1) works end-to-end with no version
// branching, reporting the apiVersion actually listed.
func TestListFunctionsHandlerV1beta1(t *testing.T) {
	h := listPackagesHandler(packagesClient(), "Function", "FunctionRevision")
	_, out, err := h(context.Background(), nil, ListPackagesInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Items) != 1 || out.Items[0].Name != "fn-patch" {
		t.Fatalf("expected the one Function, got %+v", out.Items)
	}
	if out.Items[0].APIVersion != "pkg.crossplane.io/v1beta1" {
		t.Errorf("apiVersion should report the served version, got %q", out.Items[0].APIVersion)
	}
}

func TestListPackagesHandlerFilters(t *testing.T) {
	h := listPackagesHandler(packagesClient(), "Provider", "ProviderRevision")

	_, out, err := h(context.Background(), nil, ListPackagesInput{UnhealthyOnly: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Items) != 1 || out.Items[0].Name != "provider-gcp" {
		t.Errorf("unhealthyOnly should keep only the broken provider, got %+v", out.Items)
	}
	if out.Scanned != 2 {
		t.Errorf("unhealthyOnly must not change scanned, got %d", out.Scanned)
	}

	// Substring against the OCI ref.
	_, out, err = h(context.Background(), nil, ListPackagesInput{Name: "provider-aws:v1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Items) != 1 || out.Items[0].Name != "provider-aws" || out.Scanned != 1 {
		t.Errorf("name filter should match the ref and exclude from scanned, got %+v", out)
	}
}

// TestListProvidersRevisionsForbidden: an RBAC denial on revisions must not
// fail the call — rows ship with revision-derived output suppressed and a
// note explaining why.
func TestListProvidersRevisionsForbidden(t *testing.T) {
	cl := packagesClient()
	fakeDyn := cl.Dyn.(*dynamicfake.FakeDynamicClient)
	fakeDyn.PrependReactor("list", "providerrevisions", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "pkg.crossplane.io", Resource: "providerrevisions"}, "", nil)
	})

	h := listPackagesHandler(cl, "Provider", "ProviderRevision")
	_, out, err := h(context.Background(), nil, ListPackagesInput{})
	if err != nil {
		t.Fatalf("the call must succeed without revision access, got %v", err)
	}
	if len(out.Items) != 2 {
		t.Fatalf("package rows must still ship, got %d", len(out.Items))
	}
	for _, it := range out.Items {
		if it.RevisionCount != nil || it.Revisions != nil {
			t.Errorf("revision-derived output must be suppressed when unlistable, got %+v", it)
		}
	}
	var noted bool
	for _, n := range out.Notes {
		if strings.Contains(n, "providerrevisions.pkg.crossplane.io") {
			noted = true
			// Cluster-scoped kinds must not get the "re-call with an explicit
			// namespace" advice — there is no namespace input and a namespace
			// could not scope them anyway.
			if strings.Contains(n, "explicit namespace") {
				t.Errorf("unactionable namespace advice on a cluster-scoped kind: %q", n)
			}
		}
	}
	if !noted {
		t.Errorf("expected a skip note naming the revision GroupResource, got %v", out.Notes)
	}
}

// TestListPackagesHandlerKindAbsent: a cluster without Crossplane (or without
// Functions, pre-1.14) reports honestly instead of an empty silence.
func TestListPackagesHandlerKindAbsent(t *testing.T) {
	resources := []*metav1.APIResourceList{
		{GroupVersion: "apps.example.org/v1", APIResources: []metav1.APIResource{
			{Name: "xapps", Kind: "XApp", Namespaced: true, Categories: []string{"composite"}},
		}},
	}
	disco := &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{Resources: resources}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{})
	cl := &k8s.Client{Dyn: dyn, Disco: disco}

	h := listPackagesHandler(cl, "Function", "FunctionRevision")
	_, out, err := h(context.Background(), nil, ListPackagesInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Scanned != 0 || len(out.Items) != 0 {
		t.Errorf("expected empty result, got %+v", out)
	}
	var noted bool
	for _, n := range out.Notes {
		if strings.Contains(n, "Crossplane >= 1.14") {
			noted = true
		}
	}
	if !noted {
		t.Errorf("expected the version-aware Functions note, got %v", out.Notes)
	}

	h = listPackagesHandler(cl, "Provider", "ProviderRevision")
	_, out, err = h(context.Background(), nil, ListPackagesInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !containsNote(out.Notes, "no Provider package types found") {
		t.Errorf("expected the generic not-installed note, got %v", out.Notes)
	}
}

func containsNote(notes []string, sub string) bool {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}
