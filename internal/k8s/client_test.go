package k8s

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
)

// discoStub drives the two discovery views scanForKind reads — the
// preferred-resources view (unconstrained scans) and the full
// groups+resources view (gv-constrained scans) — directly: the stock discovery
// fake leaves ServerPreferredResources unimplemented, so resolution tests need
// their own stub. Only the methods Resolve's scan calls are implemented; the
// rest would panic if reached.
type discoStub struct {
	discovery.DiscoveryInterface
	preferred []*metav1.APIResourceList // one entry per group, at its preferred version
	all       []*metav1.APIResourceList // every served version
	err       error
}

func (s discoStub) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	return s.preferred, s.err
}

func (s discoStub) ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
	return nil, s.all, s.err
}

// failingMapper rejects every RESTMapping, forcing Resolve's apiVersion path
// into its lenient-scan fallback.
type failingMapper struct{ meta.RESTMapper }

func (failingMapper) RESTMapping(gk schema.GroupKind, _ ...string) (*meta.RESTMapping, error) {
	return nil, fmt.Errorf("no mapping for %s", gk)
}

func resolveFixture() *Client {
	lists := []*metav1.APIResourceList{
		{GroupVersion: "s3.example.org/v1", APIResources: []metav1.APIResource{
			{Name: "buckets", SingularName: "bucket", Kind: "Bucket", Namespaced: true},
			{Name: "buckets/status", Kind: "Bucket", Namespaced: true}, // subresource: never matched
		}},
		{GroupVersion: "db.example.org/v1", APIResources: []metav1.APIResource{
			{Name: "xpostgresqlinstances", SingularName: "xpostgresqlinstance", Kind: "XPostgreSQLInstance", Namespaced: true},
		}},
		{GroupVersion: "legacy.example.org/v1", APIResources: []metav1.APIResource{
			// A resource whose *name* collides with the lowercase form of the
			// Bucket kind above — exact Kind matches must still win outright.
			{Name: "bucket", SingularName: "bucket", Kind: "LegacyBucket", Namespaced: false},
		}},
	}
	return &Client{Disco: discoStub{preferred: lists, all: lists}, Mapper: failingMapper{}}
}

// TestResolveLenientKind covers the LLM-friendly resolution forms: canonical
// kind, miscased kind, plural and singular resource names — all landing on the
// same canonical target.
func TestResolveLenientKind(t *testing.T) {
	cl := resolveFixture()
	cases := []struct {
		in       string
		wantKind string
		wantGVR  string
	}{
		{"XPostgreSQLInstance", "XPostgreSQLInstance", "db.example.org/v1, Resource=xpostgresqlinstances"},
		{"xpostgresqlinstance", "XPostgreSQLInstance", "db.example.org/v1, Resource=xpostgresqlinstances"},
		{"xpostgresqlinstances", "XPostgreSQLInstance", "db.example.org/v1, Resource=xpostgresqlinstances"},
		{"XPOSTGRESQLINSTANCES", "XPostgreSQLInstance", "db.example.org/v1, Resource=xpostgresqlinstances"},
	}
	for _, c := range cases {
		got, err := cl.Resolve("", c.in)
		if err != nil {
			t.Errorf("Resolve(%q): unexpected error: %v", c.in, err)
			continue
		}
		if got.Kind != c.wantKind || got.GVR.String() != c.wantGVR || !got.Namespaced {
			t.Errorf("Resolve(%q) = %+v, want kind %s gvr %s", c.in, got, c.wantKind, c.wantGVR)
		}
	}
}

// TestResolveExactKindBeatsLenient guards backward compatibility: "Bucket"
// matches the Bucket kind exactly, so the legacy resource literally named
// "bucket" must not turn a previously-unambiguous kind ambiguous.
func TestResolveExactKindBeatsLenient(t *testing.T) {
	cl := resolveFixture()
	got, err := cl.Resolve("", "Bucket")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Kind != "Bucket" || got.GVR.Group != "s3.example.org" {
		t.Errorf("exact kind must win over a lenient name collision, got %+v", got)
	}
}

// TestResolveLenientAmbiguous confirms a lenient query that matches multiple
// targets still fails loudly with candidates, never picking one silently.
func TestResolveLenientAmbiguous(t *testing.T) {
	cl := resolveFixture()
	// "bucket" matches the Bucket kind leniently AND the legacy "bucket"
	// resource name — with no exact Kind match to break the tie.
	_, err := cl.Resolve("", "bucket")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected an ambiguity error, got %v", err)
	}
	if !strings.Contains(err.Error(), "s3.example.org/v1") || !strings.Contains(err.Error(), "legacy.example.org/v1") {
		t.Errorf("ambiguity error should list both candidate groups, got: %v", err)
	}
}

// TestResolveAPIVersionLenientFallback covers the apiVersion path: the REST
// mapper is exact-case, so a miscased kind falls back to a lenient scan
// constrained to the requested group/version.
func TestResolveAPIVersionLenientFallback(t *testing.T) {
	cl := resolveFixture()
	got, err := cl.Resolve("s3.example.org/v1", "buckets")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Kind != "Bucket" || got.GVR.Group != "s3.example.org" {
		t.Errorf("expected the s3 Bucket via constrained lenient fallback, got %+v", got)
	}
	// The group/version constraint must hold: a kind that only exists in
	// another group stays unresolved (the mapper's error is surfaced).
	if _, err := cl.Resolve("s3.example.org/v1", "xpostgresqlinstance"); err == nil {
		t.Error("expected an error for a kind outside the requested group/version")
	}
}

func TestResolveUnknownKind(t *testing.T) {
	cl := resolveFixture()
	_, err := cl.Resolve("", "Nonexistent")
	if err == nil || !strings.Contains(err.Error(), `no resource found for kind "Nonexistent"`) {
		t.Errorf("expected a not-found error, got %v", err)
	}
}

// TestResolveAPIVersionNonPreferredVersion guards the discovery-view choice: a
// gv-constrained lenient scan must see every *served* version, not just each
// group's preferred one — otherwise a kind requested at v1beta1 while v1beta2
// is preferred (common for provider CRDs) could never lenient-resolve.
func TestResolveAPIVersionNonPreferredVersion(t *testing.T) {
	preferred := []*metav1.APIResourceList{
		{GroupVersion: "multi.example.org/v1beta2", APIResources: []metav1.APIResource{
			{Name: "widgets", SingularName: "widget", Kind: "Widget", Namespaced: true},
		}},
	}
	all := append([]*metav1.APIResourceList{
		{GroupVersion: "multi.example.org/v1beta1", APIResources: []metav1.APIResource{
			{Name: "widgets", SingularName: "widget", Kind: "Widget", Namespaced: true},
		}},
	}, preferred...)
	cl := &Client{Disco: discoStub{preferred: preferred, all: all}, Mapper: failingMapper{}}

	got, err := cl.Resolve("multi.example.org/v1beta1", "widgets")
	if err != nil {
		t.Fatalf("non-preferred served version must lenient-resolve, got error: %v", err)
	}
	if got.Kind != "Widget" || got.GVR.Version != "v1beta1" {
		t.Errorf("expected Widget at the requested v1beta1, got %+v", got)
	}
	// The preferred version keeps working through the same path.
	if got, err := cl.Resolve("multi.example.org/v1beta2", "widget"); err != nil || got.GVR.Version != "v1beta2" {
		t.Errorf("preferred version should resolve too, got %+v / %v", got, err)
	}
}

// TestResolveDiscoveryErrors covers scanForKind's two failure shapes: a total
// discovery outage hard-fails, and partial discovery with no match surfaces the
// discovery error so a kind missing due to a degraded API group is diagnosable.
func TestResolveDiscoveryErrors(t *testing.T) {
	t.Run("total failure", func(t *testing.T) {
		cl := &Client{Disco: discoStub{err: errors.New("discovery server down")}, Mapper: failingMapper{}}
		_, err := cl.Resolve("", "Bucket")
		if err == nil || !strings.Contains(err.Error(), "discover resources") {
			t.Errorf("expected a hard discover-resources error, got %v", err)
		}
	})

	t.Run("partial with no match", func(t *testing.T) {
		lists := []*metav1.APIResourceList{
			{GroupVersion: "s3.example.org/v1", APIResources: []metav1.APIResource{
				{Name: "buckets", SingularName: "bucket", Kind: "Bucket", Namespaced: true},
			}},
		}
		cl := &Client{Disco: discoStub{preferred: lists, all: lists, err: errors.New("group metrics.k8s.io is unavailable")}, Mapper: failingMapper{}}
		_, err := cl.Resolve("", "Missing")
		if err == nil || !strings.Contains(err.Error(), "(discovery error:") {
			t.Errorf("expected the partial-discovery error surfaced, got %v", err)
		}
		// A kind that IS present still resolves despite the partial error.
		if _, err := cl.Resolve("", "Bucket"); err != nil {
			t.Errorf("present kind should resolve despite partial discovery, got %v", err)
		}
	})
}
