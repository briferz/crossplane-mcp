package k8s

import (
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
)

// preferredStub drives ServerPreferredResources directly: the stock discovery
// fake leaves it unimplemented (it only populates ServerGroupsAndResources), so
// resolution tests need their own stub. Only the one method Resolve's scan
// calls is implemented; the rest would panic if reached.
type preferredStub struct {
	discovery.DiscoveryInterface
	lists []*metav1.APIResourceList
	err   error
}

func (s preferredStub) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	return s.lists, s.err
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
	return &Client{Disco: preferredStub{lists: lists}, Mapper: failingMapper{}}
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
