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
	"sync"
	"time"

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
	// Dyn is write-capable by type — client-go ships no read-only variant — but
	// the read-only invariant is enforced mechanically, not by convention: the
	// forbidigo rule in .golangci.yml fails the lint gate on any Create/Update/
	// Delete/Patch/Apply issued through it, and TestHandlersIssueOnlyReadVerbs
	// asserts the tool handlers record only get/list actions here. It stays
	// exported because tests in other packages inject a fake. A write through
	// some future second client would escape both checks — keep this the only
	// cluster-mutating-capable handle.
	Dyn    dynamic.Interface
	Disco  discovery.DiscoveryInterface
	Mapper meta.RESTMapper

	// loader is the kubeconfig client config used to enumerate contexts. It is
	// nil when running in-cluster (no kubeconfig contexts exist).
	loader clientcmd.ClientConfig

	// mu guards lastInvalidate, which rate-limits discovery-cache invalidation
	// so a walk over many unresolvable refs cannot turn every miss into a full
	// re-discovery of the cluster.
	mu             sync.Mutex
	lastInvalidate time.Time
}

// DefaultRequestTimeout bounds every API request this client issues. Without it
// rest.Config.Timeout is 0 and only client-go's own transport defaults apply —
// 32s on discovery, nothing at all on the dynamic client — so a wedged
// apiserver or load balancer can park a tool call indefinitely. Overridable
// with --request-timeout, where 0 still means unbounded: main translates that
// to a negative Options.RequestTimeout, because a Go zero value cannot mean
// both "caller said nothing" and "caller said none".
const DefaultRequestTimeout = 30 * time.Second

// invalidateInterval rate-limits discovery invalidation. A tree walk resolves
// many refs, and a cluster-wide-unresolvable kind would otherwise re-discover
// the whole cluster once per miss.
const invalidateInterval = 5 * time.Second

// DefaultQPS and DefaultBurst replace client-go's own defaults, which are
// DefaultQPS=5 / DefaultBurst=10 whenever the fields are left zero
// (rest/config.go). Those are a legacy client-side guard from before API
// Priority and Fairness did the job server-side, and they are badly matched to
// this tool: a diagnose walk is bounded at maxNodes=200 one-object Gets plus
// events for up to maxSuspects=10, so ~210 requests at 5/s is roughly 40
// seconds of pure client-side throttling against a cluster that could answer in
// one.
//
// These are still a bound, not a removal — the walk is capped, so this cannot
// become an unbounded scrape. Set --qps negative to disable client-side
// throttling entirely and rely on the server's APF.
const (
	DefaultQPS   float32 = 50
	DefaultBurst int     = 100
)

// Options configure a Client. The zero value is valid and applies the
// Default* constants; only RequestTimeout distinguishes "unset" from "disabled"
// (see New).
type Options struct {
	// KubeconfigPath is an explicit kubeconfig; empty honours KUBECONFIG and
	// the default path, then falls back to in-cluster config.
	KubeconfigPath string
	// Context pins a kubeconfig context; empty uses current-context.
	Context string
	// RequestTimeout bounds every request. Zero means DefaultRequestTimeout;
	// pass a negative value to disable the bound.
	RequestTimeout time.Duration
	// QPS/Burst bound client-side request rate. Zero means the Default*
	// constants; negative disables client-side throttling.
	QPS   float32
	Burst int
	// UserAgent identifies this tool in the apiserver's audit log. Empty uses
	// a generic client-go string, which is worth avoiding for something whose
	// whole pitch is being safe to point at production.
	UserAgent string
}

// New builds a Client from a kubeconfig (honouring KUBECONFIG and the default
// path), optionally pinned to a named context. If no kubeconfig is found it
// falls back to in-cluster config.
func New(opts Options) (*Client, error) {
	cfg, loader, err := restConfig(opts)
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
	// Cache discovery so scanForKind (ServerPreferredResources /
	// ServerGroupsAndResources) and the mapper share results instead of
	// re-hitting the API server on every resolution during a tree walk.
	cached := memory.NewMemCacheClient(disco)
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(cached)

	return &Client{Dyn: dyn, Disco: cached, Mapper: mapper, loader: loader}, nil
}

// restConfig builds the REST config. requestTimeout is applied to whichever
// path wins (kubeconfig or in-cluster); 0 leaves it unbounded.
//
// Note for future work: this timeout is a per-request deadline on the shared
// config, so a genuine Watch — nothing issues one today — would be truncated at
// it. A watch client would need its own config with Timeout unset.
func restConfig(opts Options) (*rest.Config, clientcmd.ClientConfig, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if opts.KubeconfigPath != "" {
		rules.ExplicitPath = opts.KubeconfigPath
	}
	overrides := &clientcmd.ConfigOverrides{}
	if opts.Context != "" {
		overrides.CurrentContext = opts.Context
	}
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)

	cfg, err := loader.ClientConfig()
	if err != nil {
		inCfg, inErr := rest.InClusterConfig()
		if inErr != nil {
			return nil, nil, fmt.Errorf("load kubeconfig: %w", err)
		}
		applyOptions(inCfg, opts)
		return inCfg, nil, nil
	}
	applyOptions(cfg, opts)
	return cfg, loader, nil
}

// applyOptions maps Options onto a rest.Config. Kept in one place so the
// in-cluster and kubeconfig paths cannot drift — the in-cluster branch
// previously set only the timeout, so anything added here would have silently
// applied to one path and not the other.
func applyOptions(cfg *rest.Config, opts Options) {
	switch {
	case opts.RequestTimeout < 0:
		cfg.Timeout = 0 // explicitly unbounded
	case opts.RequestTimeout == 0:
		cfg.Timeout = DefaultRequestTimeout
	default:
		cfg.Timeout = opts.RequestTimeout
	}

	// Zero means "unset" to client-go and silently yields 5/10, so the zero
	// value has to be mapped here rather than passed through.
	switch {
	case opts.QPS < 0:
		cfg.QPS = -1 // client-go treats <0 as "no client-side limiter"
	case opts.QPS == 0:
		cfg.QPS = DefaultQPS
	default:
		cfg.QPS = opts.QPS
	}
	switch {
	case opts.Burst < 0:
		cfg.Burst = -1
	case opts.Burst == 0:
		cfg.Burst = DefaultBurst
	default:
		cfg.Burst = opts.Burst
	}

	if opts.UserAgent != "" {
		cfg.UserAgent = opts.UserAgent
	}
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
//
// The kind is matched leniently: an exact Kind match always wins, but when none
// exists it falls back to a case-insensitive match against the Kind and the
// plural/singular resource names — so "Bucket", "bucket", and "buckets" all
// resolve. The callers are LLMs, and a failed exact match otherwise costs a
// whole tool-call round trip.
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
			// The REST mapper is exact-case; before giving up, retry with the
			// lenient scan constrained to the requested group/version.
			if t, lerr := c.scanForKind(kind, &gv); lerr == nil {
				return t, nil
			}
			return Target{}, fmt.Errorf("resolve %s/%s: %w", apiVersion, kind, err)
		}
		return Target{
			GVR:        m.Resource,
			Namespaced: m.Scope.Name() == meta.RESTScopeNameNamespace,
			Kind:       kind,
		}, nil
	}
	return c.scanForKind(kind, nil)
}

// scanForKind resolves a kind by scanning server discovery, optionally
// constrained to one group/version. Exact Kind matches are collected separately
// from lenient ones (case-insensitive Kind, plural or singular resource name)
// and win outright when present — so behaviour for a kind that already resolved
// exactly can never change, and leniency cannot introduce new ambiguity for it.
// invalidateDiscoveryOnce drops the cached discovery snapshot, at most once per
// invalidateInterval. Returns whether it actually invalidated.
//
// The memory cache never expires on its own and the deferred mapper's own reset
// is gated on !Fresh(), which is permanently false once populated — so without
// this a CRD installed mid-session (a provider install, a new XRD: exactly the
// events that prompt someone to reach for this tool) stays invisible until the
// server restarts.
func (c *Client) invalidateDiscoveryOnce() bool {
	cached, ok := c.Disco.(discovery.CachedDiscoveryInterface)
	if !ok {
		return false // uncached client or a test fake: nothing to invalidate
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if now.Sub(c.lastInvalidate) < invalidateInterval {
		return false
	}
	c.lastInvalidate = now
	cached.Invalidate()
	return true
}

// scanForKind resolves a kind through the discovery cache, retrying once against
// a freshly invalidated cache when the kind is not found — the kubectl pattern.
// Only a genuine not-found triggers the retry: an ambiguous kind or a discovery
// transport error is not something re-reading discovery would fix.
func (c *Client) scanForKind(kind string, gv *schema.GroupVersion) (Target, error) {
	t, notFound, err := c.scanForKindAttempt(kind, gv)
	if !notFound || !c.invalidateDiscoveryOnce() {
		return t, err
	}
	retried, stillMissing, retryErr := c.scanForKindAttempt(kind, gv)
	if stillMissing {
		// The retry learned nothing; keep the original error so the message does
		// not change depending on whether an invalidation happened to be due.
		return t, err
	}
	return retried, retryErr
}

// scanForKindAttempt is one pass over the discovery view. notFound distinguishes
// "this kind is not served" from an ambiguity or a discovery failure.
func (c *Client) scanForKindAttempt(kind string, gv *schema.GroupVersion) (_ Target, notFound bool, _ error) {
	// The unconstrained scan reads the preferred-resources view: one version per
	// group, so a kind served at several versions cannot make itself ambiguous.
	// A gv-constrained scan must read the full groups+resources view instead —
	// preferred-resources collapses each group to its preferred version, which
	// would make a constraint on any *other* served version unmatchable (e.g. a
	// provider CRD requested at v1beta1 while v1beta2 is preferred). Same full
	// view DiscoverComposite reads; the gv filter below keeps it unambiguous.
	var lists []*metav1.APIResourceList
	var err error
	if gv == nil {
		lists, err = c.Disco.ServerPreferredResources()
	} else {
		_, lists, err = c.Disco.ServerGroupsAndResources()
	}
	// Discovery may return partial results alongside an error (e.g. an
	// unavailable aggregated API). Only fail if we got nothing.
	if len(lists) == 0 && err != nil {
		return Target{}, false, fmt.Errorf("discover resources: %w", err)
	}

	var exact, lenient []Target
	for _, list := range lists {
		if list == nil {
			continue
		}
		lgv, perr := schema.ParseGroupVersion(list.GroupVersion)
		if perr != nil || (gv != nil && lgv != *gv) {
			continue
		}
		for _, r := range list.APIResources {
			if strings.Contains(r.Name, "/") { // subresource
				continue
			}
			// Target.Kind is the cluster-canonical casing, not the caller's, so
			// downstream identity keys and output stay consistent.
			t := Target{GVR: lgv.WithResource(r.Name), Namespaced: r.Namespaced, Kind: r.Kind}
			switch {
			case r.Kind == kind:
				exact = append(exact, t)
			case strings.EqualFold(r.Kind, kind),
				strings.EqualFold(r.Name, kind),
				strings.EqualFold(r.SingularName, kind):
				lenient = append(lenient, t)
			}
		}
	}
	matches := exact
	if len(matches) == 0 {
		matches = lenient
	}

	switch len(matches) {
	case 0:
		if err != nil {
			// Discovery returned a partial list with an error; surface it so a
			// missing kind caused by a degraded API group is diagnosable.
			return Target{}, true, fmt.Errorf("no resource found for kind %q (discovery error: %w)", kind, err)
		}
		return Target{}, true, fmt.Errorf("no resource found for kind %q", kind)
	case 1:
		return matches[0], false, nil
	default:
		var cands []string
		for _, m := range matches {
			cands = append(cands, m.GVR.GroupVersion().String())
		}
		sort.Strings(cands)
		return Target{}, false, fmt.Errorf("kind %q is ambiguous; specify apiVersion (candidates: %s)",
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
	}
	// Retried because a dropped fetch is not a gap in the answer — it becomes a
	// suspect. See retry.go: an unreachable node ranks tier 1, deepest-first,
	// and is usually the deepest, so a blip can be named the root cause.
	var (
		obj *unstructured.Unstructured
		err error
	)
	err = withRetry(ctx, func() error {
		if t.Namespaced {
			obj, err = ri.Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		} else {
			obj, err = ri.Get(ctx, name, metav1.GetOptions{})
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return obj, nil
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
// to newest and capped at limit (keeping the newest). A non-positive limit
// returns all of them — the API server already lists every matching event, so
// the cap only trims the returned slice, never the query.
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
