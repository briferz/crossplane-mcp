package e2e

import (
	"context"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/briferz/crossplane-mcp/internal/k8s"
	"github.com/briferz/crossplane-mcp/internal/xp"
)

// TestDiscoveryInvalidatesOnMiss is the headline for this tier.
//
// internal/k8s/invalidate_test.go covers the same logic against a hand-written
// stub whose Invalidate() just flips a counter. That proves the code calls
// Invalidate; it cannot prove that calling it makes a newly-installed CRD
// visible, because the stub IS the thing being asked. Here the memory cache,
// the DeferredDiscoveryRESTMapper, and the apiserver are all real, and the CRD
// is genuinely installed mid-session.
func TestDiscoveryInvalidatesOnMiss(t *testing.T) {
	requireEnvtest(t)
	cl := newServerClient(t, testEnv.Config)

	// Populate the discovery cache with a kind that already exists. After this
	// the memcache is warm and Fresh() is permanently true, which is what used
	// to make a later install invisible.
	if _, err := cl.Resolve("", "XApp"); err != nil {
		t.Fatalf("XApp should resolve from the initial CRDs: %v", err)
	}

	// It must not resolve yet — otherwise the test proves nothing.
	if _, err := cl.Resolve("", "Latecomer"); err == nil {
		t.Fatal("Latecomer resolved before its CRD was installed; the fixture is wrong")
	}

	if _, err := envtest.InstallCRDs(testEnv.Config, envtest.CRDInstallOptions{
		Paths:              []string{filepath.Join("testdata", "latecrd")},
		ErrorIfPathMissing: true,
	}); err != nil {
		t.Fatalf("installing the late CRD: %v", err)
	}

	// The invalidation is rate-limited to once per interval, so give the
	// apiserver a moment to serve the new type and retry a few times rather
	// than asserting on the very first attempt.
	var lastErr error
	for i := range 10 {
		if _, err := cl.Resolve("", "Latecomer"); err == nil {
			return // resolved: the cache was invalidated and re-read
		} else {
			lastErr = err
		}
		if i < 9 {
			time.Sleep(time.Second)
		}
	}
	t.Fatalf("a CRD installed mid-session never became visible — the discovery cache "+
		"was not invalidated: %v", lastErr)
}

// TestEventsUseFieldSelector pins behaviour no unit test can reach: dynamicfake
// ignores field selectors entirely, so a broken involvedObject.uid selector
// passes the whole existing suite while silently attributing one resource's
// events to another.
func TestEventsUseFieldSelector(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	cl := newServerClient(t, testEnv.Config)
	core := kubernetes.NewForConfigOrDie(testEnv.Config)

	ns := "events-test"
	mustNamespace(t, core, ns)

	// Two objects, each with its own event, plus a decoy on the other one.
	subject := mustNopResource(t, ns, "subject")
	other := mustNopResource(t, ns, "other")

	mustEvent(t, core, ns, "subject-evt", subject, "SubjectWarning", "this one belongs to subject")
	mustEvent(t, core, ns, "other-evt", other, "OtherWarning", "this one belongs to other")

	got, err := cl.Events(ctx, ns, string(subject.GetUID()), 10)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly the subject's event, got %d: %+v", len(got), got)
	}
	if got[0].Reason != "SubjectWarning" {
		t.Errorf("event Reason = %q, want SubjectWarning — the uid field selector is not filtering",
			got[0].Reason)
	}
}

// TestTreeWalkBothRefShapes drives the walk against real CRDs in both the
// Crossplane v2 and v1 shapes. Hard rule 2 requires both; the unit fixtures
// assert the parsing, this asserts it against an apiserver that actually
// stores and serves the objects.
func TestTreeWalkBothRefShapes(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	cl := newServerClient(t, testEnv.Config)
	core := kubernetes.NewForConfigOrDie(testEnv.Config)

	ns := "tree-test"
	mustNamespace(t, core, ns)

	leaf := mustNopResource(t, ns, "leaf")
	setStatusConditions(t, leaf, cond("Synced", "False", "ApplyFailed", "the provider rejected the spec"))

	t.Run("v2 namespaced XR via spec.crossplane.resourceRefs", func(t *testing.T) {
		xr := newUnstructured("apps.example.org/v1", "XApp", ns, "v2-app")
		setNestedRefs(t, xr, []map[string]any{
			{"apiVersion": "nop.example.org/v1", "kind": "NopResource", "name": "leaf"},
		}, "spec", "crossplane", "resourceRefs")
		xr = mustCreate(t, xr) // the returned object carries resourceVersion
		setStatusConditions(t, xr, cond("Ready", "False", "Unavailable", "composed resource not ready"))

		root, err := cl.Get(ctx, mustResolve(t, cl, "apps.example.org/v1", "XApp"), ns, "v2-app")
		if err != nil {
			t.Fatalf("get XR: %v", err)
		}
		_, stats := xp.BuildTree(ctx, cl, root)
		if stats.Nodes != 2 {
			t.Fatalf("walked %d nodes, want 2 (XR + composed NopResource)", stats.Nodes)
		}
	})

	t.Run("v1 cluster-scoped XR via spec.resourceRefs", func(t *testing.T) {
		xr := newUnstructured("legacy.example.org/v1", "XCluster", "", "v1-cluster")
		setNestedRefs(t, xr, []map[string]any{
			{"apiVersion": "nop.example.org/v1", "kind": "NopResource", "namespace": ns, "name": "leaf"},
		}, "spec", "resourceRefs")
		xr = mustCreate(t, xr)

		root, err := cl.Get(ctx, mustResolve(t, cl, "legacy.example.org/v1", "XCluster"), "", "v1-cluster")
		if err != nil {
			t.Fatalf("get cluster-scoped XR: %v", err)
		}
		if root.GetNamespace() != "" {
			t.Errorf("a cluster-scoped XR must carry no namespace, got %q", root.GetNamespace())
		}
		_, stats := xp.BuildTree(ctx, cl, root)
		if stats.Nodes != 2 {
			t.Fatalf("walked %d nodes, want 2", stats.Nodes)
		}
	})
}

// TestDiscoveryCategoriesSurviveTheWire pins that the Crossplane discovery
// categories list_unhealthy and the package tools key on actually round-trip
// through client-go 0.36's aggregated discovery — the mechanism, not a fake's
// hand-populated APIResourceList.
func TestDiscoveryCategoriesSurviveTheWire(t *testing.T) {
	requireEnvtest(t)
	cl := newServerClient(t, testEnv.Config)

	kinds, notes, err := cl.DiscoverComposite() // no args -> composite + claim
	if err != nil {
		t.Fatalf("DiscoverComposite: %v (notes: %v)", err, notes)
	}
	seen := map[string]string{}
	for _, k := range kinds {
		seen[k.Kind] = k.Category
	}
	for kind, want := range map[string]string{"XApp": "composite", "XCluster": "composite"} {
		if got := seen[kind]; got != want {
			t.Errorf("%s category = %q, want %q (discovered: %v)", kind, got, want, seen)
		}
	}
}

// --- helpers (all writes; the server under test never does any of this) ---

func mustNamespace(t *testing.T, core kubernetes.Interface, name string) {
	t.Helper()
	_, err := core.CoreV1().Namespaces().Create(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace %s: %v", name, err)
	}
}

// mustResolve resolves through the server's own client, so the test exercises
// the same kind-resolution path the tools use.
func mustResolve(t *testing.T, cl *k8s.Client, apiVersion, kind string) k8s.Target {
	t.Helper()
	target, err := cl.Resolve(apiVersion, kind)
	if err != nil {
		t.Fatalf("resolve %s %s: %v", apiVersion, kind, err)
	}
	return target
}

func newUnstructured(apiVersion, kind, ns, name string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetAPIVersion(apiVersion)
	o.SetKind(kind)
	o.SetName(name)
	if ns != "" {
		o.SetNamespace(ns)
	}
	return o
}

func dynClient(t *testing.T) dynamic.Interface {
	t.Helper()
	return dynamic.NewForConfigOrDie(testEnv.Config)
}

func gvrFor(apiVersion, resource string) schema.GroupVersionResource {
	gv, _ := schema.ParseGroupVersion(apiVersion)
	return gv.WithResource(resource)
}

func mustCreate(t *testing.T, o *unstructured.Unstructured) *unstructured.Unstructured {
	t.Helper()
	gvr := gvrFor(o.GetAPIVersion(), pluralOf(o.GetKind()))
	ri := dynClient(t).Resource(gvr)
	var created *unstructured.Unstructured
	var err error
	if ns := o.GetNamespace(); ns != "" {
		created, err = ri.Namespace(ns).Create(context.Background(), o, metav1.CreateOptions{})
	} else {
		created, err = ri.Create(context.Background(), o, metav1.CreateOptions{})
	}
	if err != nil {
		t.Fatalf("create %s/%s: %v", o.GetKind(), o.GetName(), err)
	}
	return created
}

func pluralOf(kind string) string {
	switch kind {
	case "XApp":
		return "xapps"
	case "XCluster":
		return "xclusters"
	case "NopResource":
		return "nopresources"
	}
	return ""
}

func mustNopResource(t *testing.T, ns, name string) *unstructured.Unstructured {
	t.Helper()
	return mustCreate(t, newUnstructured("nop.example.org/v1", "NopResource", ns, name))
}

func cond(typ, status, reason, msg string) map[string]any {
	return map[string]any{
		"type": typ, "status": status, "reason": reason, "message": msg,
		"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
	}
}

func setStatusConditions(t *testing.T, o *unstructured.Unstructured, conds ...map[string]any) {
	t.Helper()
	cs := make([]any, len(conds))
	for i, c := range conds {
		cs[i] = c
	}
	if err := unstructured.SetNestedSlice(o.Object, cs, "status", "conditions"); err != nil {
		t.Fatalf("set conditions: %v", err)
	}
	gvr := gvrFor(o.GetAPIVersion(), pluralOf(o.GetKind()))
	ri := dynClient(t).Resource(gvr)
	var err error
	if ns := o.GetNamespace(); ns != "" {
		_, err = ri.Namespace(ns).UpdateStatus(context.Background(), o, metav1.UpdateOptions{})
	} else {
		_, err = ri.UpdateStatus(context.Background(), o, metav1.UpdateOptions{})
	}
	if err != nil {
		t.Fatalf("update status of %s/%s: %v", o.GetKind(), o.GetName(), err)
	}
}

func setNestedRefs(t *testing.T, o *unstructured.Unstructured, refs []map[string]any, path ...string) {
	t.Helper()
	list := make([]any, len(refs))
	for i, r := range refs {
		list[i] = r
	}
	if err := unstructured.SetNestedSlice(o.Object, list, path...); err != nil {
		t.Fatalf("set refs at %v: %v", path, err)
	}
}

func mustEvent(t *testing.T, core kubernetes.Interface, ns, name string, about *unstructured.Unstructured, reason, msg string) {
	t.Helper()
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: about.GetAPIVersion(),
			Kind:       about.GetKind(),
			Name:       about.GetName(),
			Namespace:  about.GetNamespace(),
			UID:        about.GetUID(),
		},
		Reason:         reason,
		Message:        msg,
		Type:           corev1.EventTypeWarning,
		Count:          3,
		LastTimestamp:  metav1.Now(),
		FirstTimestamp: metav1.Now(),
	}
	if _, err := core.CoreV1().Events(ns).Create(context.Background(), ev, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create event %s: %v", name, err)
	}
}

// TestTreeWalkFanOutAgainstRealAPIServer drives the parallel walk's concurrent
// path against a real apiserver, dynamic client and HTTP/2 transport. The other
// walk tests here give each XR a single ref, and the walk only fans out at a
// node with two or more fetchable children — so they never reach the
// concurrent code at all. `make e2e-envtest` — which CI's required `integration`
// job runs — passes -race, and that is what makes this meaningful; peakClient
// below proves the concurrent path actually ran.
//
// The refs mix the cases whose placement the walk must not disturb: a ref that
// omits its namespace (inheriting the XR's), and a dangling ref that must come
// back as a NotFound error node in its original position.
func TestTreeWalkFanOutAgainstRealAPIServer(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	cl := newServerClient(t, testEnv.Config)
	core := kubernetes.NewForConfigOrDie(testEnv.Config)

	ns := "fanout-test"
	mustNamespace(t, core, ns)

	var refs []map[string]any
	for i := range 12 {
		name := fmt.Sprintf("leaf-%02d", i)
		mustNopResource(t, ns, name)
		refs = append(refs, map[string]any{"apiVersion": "nop.example.org/v1", "kind": "NopResource", "namespace": ns, "name": name})
	}
	mustNopResource(t, ns, "inherits")
	refs = append(refs,
		map[string]any{"apiVersion": "nop.example.org/v1", "kind": "NopResource", "name": "inherits"}, // no namespace
		map[string]any{"apiVersion": "nop.example.org/v1", "kind": "NopResource", "namespace": ns, "name": "does-not-exist"},
	)

	xr := newUnstructured("apps.example.org/v1", "XApp", ns, "fanout")
	setNestedRefs(t, xr, refs, "spec", "crossplane", "resourceRefs")
	mustCreate(t, xr)

	root, err := cl.Get(ctx, mustResolve(t, cl, "apps.example.org/v1", "XApp"), ns, "fanout")
	if err != nil {
		t.Fatalf("get XR: %v", err)
	}
	// Wrap the REAL client to observe concurrency. Without this the test would
	// pass identically with the prefetch switched off, and its -race run would
	// say nothing about the concurrent path it exists for.
	pc := &peakClient{Client: cl}
	tree, stats := xp.BuildTree(ctx, pc, root)
	if peak := pc.peak.Load(); peak < 2 {
		t.Fatalf("at most %d Get in flight against the real apiserver: the concurrent path never ran", peak)
	}

	if stats.Nodes != 1+len(refs) {
		t.Fatalf("walked %d nodes, want %d (XR + every ref)", stats.Nodes, 1+len(refs))
	}
	if len(tree.Children) != len(refs) {
		t.Fatalf("%d children, want %d", len(tree.Children), len(refs))
	}
	// Order is the refs' order, whatever order the fetches completed in.
	for i, c := range tree.Children {
		if want := refs[i]["name"]; c.Name != want {
			t.Errorf("child %d is %q, want %q — the walk reordered its children", i, c.Name, want)
		}
	}
	if c := tree.Children[12]; c.Error != "" || c.Namespace != ns {
		t.Errorf("the namespace-less ref must inherit %q and resolve, got ns=%q err=%q", ns, c.Namespace, c.Error)
	}
	if c := tree.Children[13]; c.Error == "" {
		t.Error("the dangling ref must be a NotFound error node")
	}
	for i, c := range tree.Children[:12] {
		if c.Error != "" {
			t.Errorf("leaf %d errored: %s", i, c.Error)
		}
	}
}

// peakClient forwards to a real *k8s.Client and records the peak number of Gets
// in flight. The short hold keeps overlapping requests overlapping on a fast
// local apiserver, so the peak is observable rather than a race against it.
type peakClient struct {
	*k8s.Client
	cur, peak atomic.Int32
}

func (p *peakClient) Get(ctx context.Context, t k8s.Target, ns, name string) (*unstructured.Unstructured, error) {
	n := p.cur.Add(1)
	defer p.cur.Add(-1)
	for {
		prev := p.peak.Load()
		if n <= prev || p.peak.CompareAndSwap(prev, n) {
			break
		}
	}
	time.Sleep(5 * time.Millisecond)
	return p.Client.Get(ctx, t, ns, name)
}
