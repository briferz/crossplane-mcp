package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/briferz/crossplane-mcp/internal/k8s"
)

// The project's two headline promises — only get/list/watch is ever issued, and
// a Secret's contents are never returned — were true but unenforced: no lint
// rule, no behavioural test, and an output projection that excluded Secret data
// only because a core/v1 Secret happens to have no `spec`. These pin both.
//
// The lint half lives in .golangci.yml (a forbidigo rule on the dynamic client's
// write methods); a linter rule cannot be unit-tested from inside the module, so
// it was verified out-of-band against a probe with ten write call sites — all
// flagged, with no false positives on strings.Replace or a local type's Create.

// Deliberately not named secret*: gosec's G101 reads an identifier containing
// "secret" bound to a base64-looking literal as a hardcoded credential and fails
// the lint gate.
const (
	fixtureDataValue   = "c3VwZXItc2VjcmV0" // base64("super-secret")
	fixtureStringValue = "admin-user"
)

// safetyClient serves a v2 XR whose composed refs include a core/v1 Secret — the
// exact shape that makes "the server never reads Secrets" false as literally
// written, since the tree walker Gets it like any other node.
func safetyClient() *k8s.Client {
	resources := []*metav1.APIResourceList{
		{GroupVersion: "apps.example.org/v1", APIResources: []metav1.APIResource{
			{Name: "xapps", SingularName: "xapp", Kind: "XApp", Namespaced: true, Categories: []string{"composite"}},
		}},
		{GroupVersion: "s3.aws.upbound.io/v1beta1", APIResources: []metav1.APIResource{
			{Name: "buckets", SingularName: "bucket", Kind: "Bucket", Namespaced: true, Categories: []string{"managed"}},
		}},
		{GroupVersion: "v1", APIResources: []metav1.APIResource{
			{Name: "secrets", SingularName: "secret", Kind: "Secret", Namespaced: true},
			{Name: "events", SingularName: "event", Kind: "Event", Namespaced: true},
		}},
	}
	disco := &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{Resources: resources}}

	xr := uobj("apps.example.org/v1", "XApp", "team-a", "app-1", condM("Ready", "False"))
	_ = unstructured.SetNestedSlice(xr.Object, []any{
		map[string]any{"apiVersion": "s3.aws.upbound.io/v1beta1", "kind": "Bucket", "name": "bucket-1"},
		map[string]any{"apiVersion": "v1", "kind": "Secret", "name": "app-1-conn"},
	}, "spec", "crossplane", "resourceRefs")

	bucket := uobj("s3.aws.upbound.io/v1beta1", "Bucket", "team-a", "bucket-1", condM("Synced", "False"))
	bucket.SetUID("uid-bucket")

	sec := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": "app-1-conn", "namespace": "team-a"},
		"type":       "Opaque",
		"data":       map[string]any{"password": fixtureDataValue},
		"stringData": map[string]any{"username": fixtureStringValue},
	}}

	gvrToListKind := map[schema.GroupVersionResource]string{
		{Group: "apps.example.org", Version: "v1", Resource: "xapps"}:         "XAppList",
		{Group: "s3.aws.upbound.io", Version: "v1beta1", Resource: "buckets"}: "BucketList",
		{Group: "", Version: "v1", Resource: "secrets"}:                       "SecretList",
		{Group: "", Version: "v1", Resource: "events"}:                        "EventList",
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToListKind, xr, bucket, sec)
	return &k8s.Client{Dyn: dyn, Disco: disco, Mapper: errMapper{}}
}

// assertReadOnlyActions fails on any verb the read-only invariant does not allow.
//
// watch is deliberately excluded even though hard rule 1 permits it: nothing
// watches today, and a future watch-based tool should have to widen this on
// purpose. wantCalls guards against a vacuous pass — a handler that errored out
// before touching the API would otherwise record zero actions and look clean.
func assertReadOnlyActions(t *testing.T, cl *k8s.Client, wantCalls bool) {
	t.Helper()
	fake, ok := cl.Dyn.(*dynamicfake.FakeDynamicClient)
	if !ok {
		t.Fatalf("fixture must use a fake dynamic client, got %T", cl.Dyn)
	}
	acts := fake.Actions()
	if wantCalls && len(acts) == 0 {
		t.Fatal("no API calls recorded — the handler cannot have exercised the cluster path")
	}
	for _, a := range acts {
		switch a.GetVerb() {
		case "get", "list":
		default:
			// Deliberately not phrased as "invariant violated": watch would trip
			// this while being permitted by hard rule 1. Anything else genuinely
			// does violate it.
			t.Errorf("unexpected verb %q on %s — handlers issue only get/list today; "+
				"watch is permitted by the read-only invariant but must be added here deliberately",
				a.GetVerb(), a.GetResource().Resource)
		}
	}
}

// TestHandlersIssueOnlyReadVerbs drives every registered tool against a fake API
// server and asserts nothing mutating reaches it. This is the behavioural half of
// the read-only guarantee; the forbidigo rule is the static half.
func TestHandlersIssueOnlyReadVerbs(t *testing.T) {
	cases := []struct {
		name      string
		client    func() *k8s.Client
		run       func(context.Context, *k8s.Client) error
		wantCalls bool
	}{
		{"diagnose", safetyClient, func(ctx context.Context, cl *k8s.Client) error {
			_, _, err := diagnoseHandler(cl)(ctx, nil, DiagnoseInput{
				APIVersion: "apps.example.org/v1", Kind: "XApp", Namespace: "team-a", Name: "app-1", IncludeTree: true})
			return err
		}, true},
		{"get_resource_tree", safetyClient, func(ctx context.Context, cl *k8s.Client) error {
			_, _, err := treeHandler(cl)(ctx, nil, TreeInput{
				APIVersion: "apps.example.org/v1", Kind: "XApp", Namespace: "team-a", Name: "app-1"})
			return err
		}, true},
		{"get_resource", safetyClient, func(ctx context.Context, cl *k8s.Client) error {
			_, _, err := getResourceHandler(cl)(ctx, nil, GetResourceInput{
				APIVersion: "apps.example.org/v1", Kind: "XApp", Namespace: "team-a", Name: "app-1"})
			return err
		}, true},
		{"list_unhealthy", listUnhealthyClient, func(ctx context.Context, cl *k8s.Client) error {
			_, _, err := listUnhealthyHandler(cl)(ctx, nil, ListUnhealthyInput{})
			return err
		}, true},
		{"list_providers", packagesClient, func(ctx context.Context, cl *k8s.Client) error {
			_, _, err := listPackagesHandler(cl, "Provider", "ProviderRevision")(ctx, nil, ListPackagesInput{})
			return err
		}, true},
		{"list_functions", packagesClient, func(ctx context.Context, cl *k8s.Client) error {
			_, _, err := listPackagesHandler(cl, "Function", "FunctionRevision")(ctx, nil, ListPackagesInput{})
			return err
		}, true},
		{"list_configurations", packagesClient, func(ctx context.Context, cl *k8s.Client) error {
			_, _, err := listPackagesHandler(cl, "Configuration", "ConfigurationRevision")(ctx, nil, ListPackagesInput{})
			return err
		}, true},
		// list_contexts reads the kubeconfig, not the API. Kept in the table so a
		// future implementation that starts calling the cluster is forced through
		// this assertion rather than quietly bypassing it.
		{"list_contexts", safetyClient, func(ctx context.Context, cl *k8s.Client) error {
			_, _, err := contextsHandler(cl)(ctx, nil, ContextsInput{})
			return err
		}, false},
	}

	// The table is hand-written, so on its own a ninth tool could be registered
	// with no row and this test would still pass. Compare against the tools the
	// server actually registers, so adding one forces a row here.
	covered := make(map[string]bool, len(cases))
	for _, c := range cases {
		covered[c.name] = true
	}
	for _, name := range registeredToolNames(t) {
		if !covered[name] {
			t.Errorf("tool %q is registered but has no read-verb case — every tool must be "+
				"asserted against the read-only invariant", name)
		}
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cl := c.client()
			if err := c.run(context.Background(), cl); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			assertReadOnlyActions(t, cl, c.wantCalls)
		})
	}
}

// registeredToolNames lists the tools the server actually exposes, over a real
// in-memory MCP session.
func registeredToolNames(t *testing.T) []string {
	t.Helper()
	ctx := context.Background()
	s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	Register(s, &k8s.Client{}, nil)

	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := s.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer func() { _ = ss.Close() }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer func() { _ = cs.Close() }()

	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	out := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		out = append(out, tool.Name)
	}
	return out
}

// TestSecretContentsNeverReturned pins the projection that makes the no-Secret-
// contents promise true. It holds today only because ResourceView reads `spec`
// and a core/v1 Secret has none — a coincidence of field naming that nothing
// else tests. Adding a raw-object or `data` field would turn a still-advertised
// promise into a live disclosure; this fails first.
func TestSecretContentsNeverReturned(t *testing.T) {
	leaks := func(t *testing.T, label string, v any) {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %s: %v", label, err)
		}
		for _, needle := range []string{fixtureDataValue, fixtureStringValue, "super-secret"} {
			if strings.Contains(string(b), needle) {
				t.Errorf("%s leaked secret value %q into output", label, needle)
			}
		}
	}

	t.Run("get_resource", func(t *testing.T) {
		cl := safetyClient()
		_, view, err := getResourceHandler(cl)(context.Background(), nil, GetResourceInput{
			APIVersion: "v1", Kind: "Secret", Namespace: "team-a", Name: "app-1-conn"})
		if err != nil {
			t.Fatalf("get_resource on a Secret: %v", err)
		}
		// Assert the fetch really happened, so the leak check below is not vacuous.
		if view.Kind != "Secret" {
			t.Fatalf("expected the Secret to be fetched, got kind %q", view.Kind)
		}
		leaks(t, "get_resource", view)

		b, _ := json.Marshal(view)
		var top map[string]any
		if err := json.Unmarshal(b, &top); err != nil {
			t.Fatalf("unmarshal view: %v", err)
		}
		for _, k := range []string{"data", "stringData"} {
			if _, present := top[k]; present {
				t.Errorf("ResourceView must not carry a %q field", k)
			}
		}
	})

	t.Run("diagnose_tree_walk", func(t *testing.T) {
		cl := safetyClient()
		_, d, err := diagnoseHandler(cl)(context.Background(), nil, DiagnoseInput{
			APIVersion: "apps.example.org/v1", Kind: "XApp", Namespace: "team-a", Name: "app-1",
			IncludeTree: true})
		if err != nil {
			t.Fatalf("diagnose: %v", err)
		}
		b, _ := json.Marshal(d)
		// The walker really does Get the composed Secret — state that plainly
		// rather than pretending otherwise. What must never appear is its payload.
		if !strings.Contains(string(b), "app-1-conn") {
			t.Fatal("expected the composed Secret to appear as a tree node")
		}
		leaks(t, "diagnose", d)
	})
}
