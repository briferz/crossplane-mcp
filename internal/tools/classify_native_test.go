package tools

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/briferz/crossplane-mcp/internal/k8s"
	"github.com/briferz/crossplane-mcp/internal/xp"
)

// nativeClient serves one core-group ConfigMap — an object that will never carry
// Crossplane's health conditions.
func nativeClient() *k8s.Client {
	resources := []*metav1.APIResourceList{
		{GroupVersion: "v1", APIResources: []metav1.APIResource{
			{Name: "configmaps", SingularName: "configmap", Kind: "ConfigMap", Namespaced: true},
		}},
	}
	disco := &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{Resources: resources}}

	gvrToListKind := map[schema.GroupVersionResource]string{
		{Group: "", Version: "v1", Resource: "configmaps"}: "ConfigMapList",
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToListKind,
		uobj("v1", "ConfigMap", "team-a", "app-config"))
	return &k8s.Client{Dyn: dyn, Disco: disco, Mapper: errMapper{}}
}

// TestGetResourceHandlerNativeStateUnknown pins the tools.go call site actually
// passing a real apiVersion into Classify: a ConfigMap must report Unknown, not
// a false Pending that reads as "still coming up".
func TestGetResourceHandlerNativeStateUnknown(t *testing.T) {
	h := getResourceHandler(nativeClient())

	_, out, err := h(context.Background(), nil, GetResourceInput{
		APIVersion: "v1", Kind: "ConfigMap", Namespace: "team-a", Name: "app-config",
	})
	if err != nil {
		t.Fatalf("get_resource: %v", err)
	}

	if out.State != xp.StateUnknown {
		t.Errorf("State = %q, want %q", out.State, xp.StateUnknown)
	}
	if out.Health.Ready != "" || out.Health.Synced != "" || out.Health.Healthy != "" {
		t.Errorf("a native resource carries no Crossplane health conditions, got %+v", out.Health)
	}
}
