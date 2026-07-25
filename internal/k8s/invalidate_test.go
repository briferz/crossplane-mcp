package k8s

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
)

// invalidatingDisco is a CachedDiscoveryInterface whose served resource list
// changes after the first Invalidate — modelling a CRD installed while the
// server is already running, which is the case the cache used to hide until a
// restart.
type invalidatingDisco struct {
	discovery.DiscoveryInterface
	before      []*metav1.APIResourceList
	after       []*metav1.APIResourceList
	invalidated int
}

func (s *invalidatingDisco) current() []*metav1.APIResourceList {
	if s.invalidated > 0 {
		return s.after
	}
	return s.before
}

func (s *invalidatingDisco) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	return s.current(), nil
}

func (s *invalidatingDisco) ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
	return nil, s.current(), nil
}

func (s *invalidatingDisco) Fresh() bool { return true }

func (s *invalidatingDisco) Invalidate() { s.invalidated++ }

func resourceList(gv string, rs ...metav1.APIResource) []*metav1.APIResourceList {
	return []*metav1.APIResourceList{{GroupVersion: gv, APIResources: rs}}
}

// TestScanForKindInvalidatesOnMiss: a kind absent from the cached snapshot but
// present in the cluster must resolve after one invalidation, not require a
// server restart.
func TestScanForKindInvalidatesOnMiss(t *testing.T) {
	disco := &invalidatingDisco{
		before: resourceList("infra.example.org/v1",
			metav1.APIResource{Name: "buckets", Kind: "Bucket", Namespaced: true}),
		after: resourceList("infra.example.org/v1",
			metav1.APIResource{Name: "buckets", Kind: "Bucket", Namespaced: true},
			metav1.APIResource{Name: "widgets", Kind: "Widget", Namespaced: true}),
	}
	cl := &Client{Disco: disco, Mapper: failingMapper{}}

	got, err := cl.Resolve("", "Widget")
	if err != nil {
		t.Fatalf("Widget should resolve after invalidation: %v", err)
	}
	if got.Kind != "Widget" {
		t.Errorf("Kind = %q, want Widget", got.Kind)
	}
	if disco.invalidated != 1 {
		t.Errorf("invalidated %d times, want exactly 1", disco.invalidated)
	}

	// A kind that was already cached must not trigger any further invalidation.
	if _, err := cl.Resolve("", "Bucket"); err != nil {
		t.Fatalf("Bucket: %v", err)
	}
	if disco.invalidated != 1 {
		t.Errorf("a hit must not invalidate; count = %d", disco.invalidated)
	}
}

// TestScanForKindInvalidateRateLimited: a walk over many unresolvable refs must
// not re-discover the whole cluster once per miss.
func TestScanForKindInvalidateRateLimited(t *testing.T) {
	list := resourceList("infra.example.org/v1",
		metav1.APIResource{Name: "buckets", Kind: "Bucket", Namespaced: true})
	disco := &invalidatingDisco{before: list, after: list}
	cl := &Client{Disco: disco, Mapper: failingMapper{}}

	for i := range 5 {
		if _, err := cl.Resolve("", "NeverThere"); err == nil {
			t.Fatalf("call %d: expected an error for an absent kind", i)
		}
	}
	if disco.invalidated != 1 {
		t.Errorf("invalidated %d times across 5 misses, want 1 (rate limited)", disco.invalidated)
	}
}

// TestScanForKindAmbiguousDoesNotInvalidate: re-reading discovery cannot fix an
// ambiguous kind, so it must not pay for a re-discovery.
func TestScanForKindAmbiguousDoesNotInvalidate(t *testing.T) {
	lists := []*metav1.APIResourceList{
		{GroupVersion: "a.example.org/v1", APIResources: []metav1.APIResource{
			{Name: "buckets", Kind: "Bucket", Namespaced: true}}},
		{GroupVersion: "b.example.org/v1", APIResources: []metav1.APIResource{
			{Name: "buckets", Kind: "Bucket", Namespaced: true}}},
	}
	disco := &invalidatingDisco{before: lists, after: lists}
	cl := &Client{Disco: disco, Mapper: failingMapper{}}

	if _, err := cl.Resolve("", "Bucket"); err == nil {
		t.Fatal("expected an ambiguity error")
	}
	if disco.invalidated != 0 {
		t.Errorf("an ambiguous kind must not invalidate discovery, count = %d", disco.invalidated)
	}
}
