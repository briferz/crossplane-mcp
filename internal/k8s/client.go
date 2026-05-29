// Package k8s wraps read-only access to a Kubernetes cluster: dynamic +
// discovery clients, a REST mapper for kind→resource resolution, kubeconfig /
// in-cluster auth, and small helpers for fetching resources and their events.
//
// Everything here is deliberately read-only — only get/list verbs are ever
// issued.
package k8s

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	memory "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

// Client holds the read-only handles used by the diagnostic tools.
type Client struct {
	Dyn    dynamic.Interface
	Disco  discovery.DiscoveryInterface
	Mapper meta.RESTMapper

	// loader is the kubeconfig client config used to enumerate contexts. It is
	// nil when running in-cluster (no kubeconfig contexts exist).
	loader clientcmd.ClientConfig
}

// New builds a Client from a kubeconfig (honouring KUBECONFIG and the default
// path), optionally pinned to a named context. If no kubeconfig is found it
// falls back to in-cluster config.
func New(kubeconfigPath, contextName string) (*Client, error) {
	cfg, loader, err := restConfig(kubeconfigPath, contextName)
	if err != nil {
		return nil, err
	}

	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}
	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("discovery client: %w", err)
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco))

	return &Client{Dyn: dyn, Disco: disco, Mapper: mapper, loader: loader}, nil
}

func restConfig(kubeconfigPath, contextName string) (*rest.Config, clientcmd.ClientConfig, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfigPath != "" {
		rules.ExplicitPath = kubeconfigPath
	}
	overrides := &clientcmd.ConfigOverrides{}
	if contextName != "" {
		overrides.CurrentContext = contextName
	}
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)

	cfg, err := loader.ClientConfig()
	if err != nil {
		if inCfg, inErr := rest.InClusterConfig(); inErr == nil {
			return inCfg, nil, nil
		}
		return nil, nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	return cfg, loader, nil
}

// Target identifies a resolved resource type to query.
type Target struct {
	GVR        schema.GroupVersionResource
	Namespaced bool
	Kind       string
}

// Resolve maps a (apiVersion, kind) pair to a queryable Target. apiVersion may
// be empty, in which case the kind is resolved by scanning the server's
// preferred resources; an ambiguous kind returns an error listing candidates.
func (c *Client) Resolve(apiVersion, kind string) (Target, error) {
	if kind == "" {
		return Target{}, fmt.Errorf("kind is required")
	}
	if apiVersion != "" {
		gv, err := schema.ParseGroupVersion(apiVersion)
		if err != nil {
			return Target{}, fmt.Errorf("parse apiVersion %q: %w", apiVersion, err)
		}
		m, err := c.Mapper.RESTMapping(schema.GroupKind{Group: gv.Group, Kind: kind}, gv.Version)
		if err != nil {
			return Target{}, fmt.Errorf("resolve %s/%s: %w", apiVersion, kind, err)
		}
		return Target{
			GVR:        m.Resource,
			Namespaced: m.Scope.Name() == meta.RESTScopeNameNamespace,
			Kind:       kind,
		}, nil
	}
	return c.resolveByKind(kind)
}

func (c *Client) resolveByKind(kind string) (Target, error) {
	lists, err := c.Disco.ServerPreferredResources()
	// ServerPreferredResources may return partial results alongside an error
	// (e.g. an unavailable aggregated API). Only fail if we got nothing.
	if len(lists) == 0 && err != nil {
		return Target{}, fmt.Errorf("discover resources: %w", err)
	}

	var matches []Target
	for _, list := range lists {
		if list == nil {
			continue
		}
		gv, perr := schema.ParseGroupVersion(list.GroupVersion)
		if perr != nil {
			continue
		}
		for _, r := range list.APIResources {
			if r.Kind == kind && !strings.Contains(r.Name, "/") {
				matches = append(matches, Target{
					GVR:        gv.WithResource(r.Name),
					Namespaced: r.Namespaced,
					Kind:       kind,
				})
			}
		}
	}

	switch len(matches) {
	case 0:
		if err != nil {
			// Discovery returned a partial list with an error; surface it so a
			// missing kind caused by a degraded API group is diagnosable.
			return Target{}, fmt.Errorf("no resource found for kind %q (discovery error: %w)", kind, err)
		}
		return Target{}, fmt.Errorf("no resource found for kind %q", kind)
	case 1:
		return matches[0], nil
	default:
		var cands []string
		for _, m := range matches {
			cands = append(cands, m.GVR.GroupVersion().String())
		}
		sort.Strings(cands)
		return Target{}, fmt.Errorf("kind %q is ambiguous; specify apiVersion (candidates: %s)",
			kind, strings.Join(cands, ", "))
	}
}

// Get fetches a single resource. A namespaced kind requires a namespace —
// rather than silently defaulting to "default" (which would mask the real
// resource and return a confusing not-found), it returns an explicit error so
// the caller can supply one.
func (c *Client) Get(ctx context.Context, t Target, namespace, name string) (*unstructured.Unstructured, error) {
	ri := c.Dyn.Resource(t.GVR)
	if t.Namespaced {
		if namespace == "" {
			return nil, fmt.Errorf("namespace is required for namespaced kind %q", t.Kind)
		}
		return ri.Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	}
	return ri.Get(ctx, name, metav1.GetOptions{})
}

// Event is a pruned Kubernetes event.
type Event struct {
	Type    string `json:"type,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	Count   int64  `json:"count,omitempty"`
	Last    string `json:"lastTimestamp,omitempty"`
}

var eventsGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "events"}

// Events returns recent events for the object with the given uid, sorted oldest
// to newest and capped at limit (keeping the newest).
//
// The query is scoped to namespace to stay within a namespace-scoped read-only
// role rather than requiring cluster-wide list access on events. Events for
// cluster-scoped objects (empty namespace) are recorded in "default" by the
// Kubernetes event recorder, so that is queried instead.
func (c *Client) Events(ctx context.Context, namespace, uid string, limit int) ([]Event, error) {
	if uid == "" {
		return nil, nil
	}
	ns := namespace
	if ns == "" {
		ns = metav1.NamespaceDefault
	}
	list, err := c.Dyn.Resource(eventsGVR).Namespace(ns).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.uid=" + uid,
	})
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}

	out := make([]Event, 0, len(list.Items))
	for i := range list.Items {
		m := list.Items[i].Object
		out = append(out, Event{
			Type:    nestedString(m, "type"),
			Reason:  nestedString(m, "reason"),
			Message: nestedString(m, "message"),
			Count:   nestedInt(m, "count"),
			Last:    firstNonEmpty(nestedString(m, "lastTimestamp"), nestedString(m, "eventTime")),
		})
	}
	// RFC3339 timestamps sort lexicographically in chronological order.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Last < out[j].Last })
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

// ContextInfo describes a kubeconfig context.
type ContextInfo struct {
	Name      string `json:"name"`
	Cluster   string `json:"cluster,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Current   bool   `json:"current,omitempty"`
}

// Contexts lists the available kubeconfig contexts. Returns nil when running
// in-cluster.
func (c *Client) Contexts() ([]ContextInfo, error) {
	if c.loader == nil {
		return nil, nil
	}
	raw, err := c.loader.RawConfig()
	if err != nil {
		return nil, fmt.Errorf("read kubeconfig: %w", err)
	}
	out := make([]ContextInfo, 0, len(raw.Contexts))
	for name, ctx := range raw.Contexts {
		out = append(out, ContextInfo{
			Name:      name,
			Cluster:   ctx.Cluster,
			Namespace: ctx.Namespace,
			Current:   name == raw.CurrentContext,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func nestedString(m map[string]any, fields ...string) string {
	s, _, _ := unstructured.NestedString(m, fields...)
	return s
}

func nestedInt(m map[string]any, fields ...string) int64 {
	i, _, _ := unstructured.NestedInt64(m, fields...)
	return i
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
