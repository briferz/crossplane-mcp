package xp

import (
	"context"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/briferz/crossplane-mcp/internal/k8s"
)

// Traversal safety limits to avoid runaway walks on large or cyclic graphs.
const (
	maxNodes = 200
	maxDepth = 20
)

// Node is one resource in the Crossplane tree.
type Node struct {
	APIVersion string      `json:"apiVersion"`
	Kind       string      `json:"kind"`
	Name       string      `json:"name"`
	Namespace  string      `json:"namespace,omitempty"`
	State      string      `json:"state"`
	Health     Health      `json:"health"`
	Conditions []Condition `json:"conditions,omitempty"`
	Children   []*Node     `json:"children,omitempty"`
	Error      string      `json:"error,omitempty"`

	// Internal, not serialised. deletionTime/creationTime are the object's
	// metadata timestamps (RFC3339, "" if absent), used to derive the lifecycle
	// label and surfaced raw on suspects. paused/finalizers come from metadata
	// too: the pause annotation and finalizer list, surfaced on suspects (and
	// paused also per tree node).
	uid          string
	depth        int
	deletionTime string
	creationTime string
	paused       bool
	finalizers   []string
	// nativeReasons explains a verdict reached by a per-kind native rule rather
	// than by Crossplane conditions (see native.go). Those rules can key on
	// status.phase or replica counts, which blockingMessages cannot see, so
	// without this a Blocked native resource would be a suspect with no reasons.
	nativeReasons []string
}

// Stats reports traversal coverage.
type Stats struct {
	Nodes int `json:"nodes"`
	// Capped is true when a safety limit actually stopped the walk from
	// inspecting something — not merely when a limit was reached. Anything built
	// from a capped walk is incomplete: the deepest-first ranking ran over a
	// partial tree, so the named root cause may not be the real one.
	//
	// Residual over-approximation, deliberately: once the node budget is spent, a
	// ref that would have been skipped anyway (already visited, or malformed)
	// still flips this. Over-reporting incompleteness is safe; under-reporting is
	// not.
	Capped bool `json:"capped,omitempty"`
}

// FlatNode is the non-recursive, token-light projection of a tree node used in
// tool output. Hierarchy is encoded via Parent (index into the slice, -1 for
// the root) and Depth. Full per-node conditions are intentionally omitted —
// fetch them with get_resource when needed.
type FlatNode struct {
	Depth      int    `json:"depth"`
	Parent     int    `json:"parent"`
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace,omitempty"`
	State      string `json:"state"`
	Health     Health `json:"health"`
	// DeletionTimestamp is set (RFC3339) when the resource is being deleted —
	// surfacing a terminating node directly in the tree.
	DeletionTimestamp string `json:"deletionTimestamp,omitempty"`
	// Paused is true when the resource carries the crossplane.io/paused="true"
	// annotation: reconciliation is suspended, so the node cannot progress (or
	// finish deleting) until the annotation is removed — and its conditions may
	// be stale, since nothing updates them while paused.
	Paused bool   `json:"paused,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Flatten projects the tree into a depth-first slice of FlatNodes.
func (n *Node) Flatten() []FlatNode {
	var out []FlatNode
	var rec func(node *Node, parent int)
	rec = func(node *Node, parent int) {
		idx := len(out)
		out = append(out, FlatNode{
			Depth:             node.depth,
			Parent:            parent,
			APIVersion:        node.APIVersion,
			Kind:              node.Kind,
			Name:              node.Name,
			Namespace:         node.Namespace,
			State:             node.State,
			Health:            node.Health,
			DeletionTimestamp: node.deletionTime,
			Paused:            node.paused,
			Error:             node.Error,
		})
		for _, c := range node.Children {
			rec(c, idx)
		}
	}
	if n != nil {
		rec(n, -1)
	}
	return out
}

type ref struct {
	apiVersion string
	kind       string
	name       string
	namespace  string
}

// walkConcurrency bounds how many of one node's children are fetched at once.
// The client's own rate limiter (k8s.DefaultQPS) still governs the overall
// request rate; this only stops a composite with hundreds of composed resources
// from opening hundreds of connections at the same moment.
const walkConcurrency = 10

// TreeClient is the read surface the walk needs. *k8s.Client satisfies it; the
// interface exists so tests can inject latency and count concurrent requests,
// which is the only way to check that the walk is parallel without timing it.
type TreeClient interface {
	Resolve(apiVersion, kind string) (k8s.Target, error)
	Get(ctx context.Context, t k8s.Target, namespace, name string) (*unstructured.Unstructured, error)
}

// BuildTree walks the composition tree starting from root, following composed
// refs (v1 spec.resourceRefs and v2 spec.crossplane.resourceRefs) and the v1
// claim's spec.resourceRef. Composed resources that are themselves composites
// recurse naturally.
func BuildTree(ctx context.Context, cl TreeClient, root *unstructured.Unstructured) (*Node, Stats) {
	return buildTree(ctx, cl, root, walkConcurrency)
}

// buildTree is BuildTree with the fan-out as a parameter. concurrency <= 1
// disables prefetching entirely and is exactly the sequential walk — the
// reference the equivalence tests compare against.
func buildTree(ctx context.Context, cl TreeClient, root *unstructured.Unstructured, concurrency int) (*Node, Stats) {
	st := &Stats{}
	// Seed visited with the root so a child that references back to it is not
	// re-fetched and re-expanded as its own subtree.
	visited := map[string]bool{
		identityKey(groupOf(root.GetAPIVersion()), root.GetKind(), root.GetNamespace(), root.GetName()): true,
	}
	w := &walker{cl: cl, concurrency: concurrency}
	node := w.build(ctx, root, 0, visited, st)
	return node, *st
}

// walker carries the client and the fan-out through the recursion.
//
// HOW THE WALK IS PARALLEL. The traversal is a DFS whose result depends on its
// order in two ways: the node budget is checked before each child against
// every node built so far (earlier siblings' whole subtrees included), and a
// resource reachable from two parents is placed under whichever DFS reaches
// first. So the DFS itself stays exactly as it was — same visited checks, same
// budget checks, same order, and the loop still resolves every ref itself.
// Only the Gets move: as the loop reaches a ref whose object is not yet fetched,
// childFetcher fetches it together with the next few siblings concurrently,
// joins them, and serves each from a map keyed on EXACTLY Get's inputs. A
// missing or mismatched entry falls through to a real Get, so a prefetched
// entry can never substitute a different object.
//
// What "the same output" does and does not mean. Given a cluster that answers
// the same question the same way for the length of a walk, the tree and Stats
// are identical to the sequential walk's — TestParallelWalkMatchesSequential
// compares them in full. Two things are not identical, deliberately:
//   - A window-mate is resolved twice (once to plan the window, once by the
//     loop when it gets there; the ref a window opens on is resolved only by
//     the loop). So a transient discovery failure seen only by a
//     window is overridden by the loop's own successful Resolve — the
//     sequential walk would have made an error node of it. Better, not worse,
//     except through discovery-invalidation timing: a window can spend the
//     client's rate-limited invalidation slot slightly earlier than the
//     sequential walk would have. Crossplane refs carry an apiVersion and a
//     canonical kind, so they resolve through the REST mapper, which that
//     invalidation does not reset; the window is narrow.
//   - A prefetched object can be a slightly older read than the sequential walk
//     would have made, since a window-mate is fetched before the siblings ahead
//     of it are walked. Neither walk offers a snapshot anyway.
//
// Resolve is deliberately NOT cached. A memo pins one transient discovery
// failure onto every later ref of that kind, turning each into an unreachable
// node — which ranks tier 1, deepest-first, and gets named the root cause. That
// is the failure k8s.withRetry exists to prevent; mutation-testing a memo in
// made every ref of the failing kind an error node. The loop resolves each ref
// itself, as it always did; on a warm discovery cache that is CPU only.
//
// Within a walk, Resolve is also never called CONCURRENTLY: a window resolves
// its refs in order before starting any goroutine, the goroutines only call
// Get, and they are joined before the loop continues. So nothing here depends
// on the discovery cache or REST mapper being safe under concurrent use — only
// on the dynamic client's Get being so, which it is. (Concurrent tool calls
// sharing one Client are a separate matter, unchanged by this.)
//
// Latency: each composite with children costs ceil(fetchable children /
// walkConcurrency) round-trips rather than one per child — an XR over 30 managed
// resources is 3 rounds, not 30. A chain of composites still costs one round
// per level, since a child's refs are unknown until it has been fetched. The
// client's rate limiter (k8s.DefaultQPS/Burst) sets a floor for both walks on
// large trees, so the gain is largest below ~100 requests.
type walker struct {
	cl          TreeClient
	concurrency int
}

// getKey is Get's full input. Keying the prefetch on anything less — the
// identity key, which drops the version, or name+namespace — would let one ref
// read another's object.
type getKey struct {
	target k8s.Target
	ns     string
	name   string
}

type fetched struct {
	obj *unstructured.Unstructured
	err error
}

// identityKey is the dedup/cycle-detection key for a resource. It deliberately
// omits the API version: the same object referenced under different versions
// (e.g. v1 vs v1beta1) is still one resource, so group+kind+namespace+name is
// its true identity.
func identityKey(group, kind, namespace, name string) string {
	return group + "/" + kind + "/" + namespace + "/" + name
}

// groupOf extracts the API group from an apiVersion ("group/version" → "group",
// or "" for the core group).
func groupOf(apiVersion string) string {
	if i := strings.IndexByte(apiVersion, '/'); i >= 0 {
		return apiVersion[:i]
	}
	return ""
}

// pausedAnnotation suspends a resource's reconciliation when set to "true" —
// the standard Crossplane pause switch, honoured by crossplane-runtime managed
// reconcilers and the composite/claim controllers alike.
//
// Aliased rather than redeclared: k8s.ProjectTriageFields must retain this exact
// key when trimming listed objects, so the two cannot be allowed to drift.
const pausedAnnotation = k8s.PausedAnnotation

// IsPaused reports whether the resource carries the Crossplane pause
// annotation. A paused resource never reconciles: its conditions go stale and
// a deletion can never complete (finalizers don't run) — and nothing in status
// says so, which makes it an easy signal to miss.
func IsPaused(obj *unstructured.Unstructured) bool {
	return obj.GetAnnotations()[pausedAnnotation] == "true"
}

// deletionTime returns the object's metadata.deletionTimestamp as RFC3339, or ""
// when the resource is not being deleted.
func deletionTime(obj *unstructured.Unstructured) string {
	if dt := obj.GetDeletionTimestamp(); dt != nil {
		return metaTimeString(*dt)
	}
	return ""
}

// metaTimeString formats a metav1.Time as RFC3339 (UTC), or "" when zero/absent.
func metaTimeString(t metav1.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func (w *walker) build(ctx context.Context, obj *unstructured.Unstructured, depth int, visited map[string]bool, st *Stats) *Node {
	conds := Conditions(obj)
	health, state, nativeReasons := ClassifyObject(obj)
	n := &Node{
		APIVersion:    obj.GetAPIVersion(),
		Kind:          obj.GetKind(),
		Name:          obj.GetName(),
		Namespace:     obj.GetNamespace(),
		State:         state,
		Health:        health,
		Conditions:    conds,
		uid:           string(obj.GetUID()),
		depth:         depth,
		deletionTime:  deletionTime(obj),
		creationTime:  metaTimeString(obj.GetCreationTimestamp()),
		paused:        IsPaused(obj),
		finalizers:    obj.GetFinalizers(),
		nativeReasons: nativeReasons,
	}
	st.Nodes++

	// Capped must mean "a limit actually stopped us from inspecting something",
	// not merely "a limit was reached": a final leaf sitting exactly at maxDepth
	// with no children has skipped nothing, and reporting that as an incomplete
	// walk would make diagnose withhold a healthy verdict for no reason.
	refs := childRefs(obj)
	if depth >= maxDepth {
		if len(refs) > 0 {
			st.Capped = true
		}
		return n
	}
	// The node budget is enforced by the in-loop guard below rather than here, so
	// a node arriving at the cap with no refs likewise does not flip Capped. The
	// guard breaks before the loop consumes a ref, so the node count is as it
	// always was; the prefetch (see childFetcher) may issue a few Gets the loop
	// then does not use, bounded per node by walkConcurrency-1 plus diamonds.
	cf := &childFetcher{w: w, parent: obj, refs: refs, visited: visited, st: st}
	for i, r := range refs {
		if st.Nodes >= maxNodes {
			st.Capped = true
			break
		}
		if r.name == "" || r.kind == "" {
			continue
		}

		// Resolve once, here, so the visited key uses the cluster-canonical
		// group/kind and the *effective* namespace (v2 composed resources often
		// omit it and inherit the parent XR's). Normalising against the resolved
		// target — rather than the raw, possibly-partial ref fields — keeps the
		// key consistent with the root's and makes cycle/dedup detection sound.
		target, rerr := w.cl.Resolve(r.apiVersion, r.kind)
		group, kind := groupOf(r.apiVersion), r.kind
		ns := r.namespace
		if rerr == nil {
			group, kind = target.GVR.Group, target.Kind
			if target.Namespaced && ns == "" {
				ns = obj.GetNamespace()
			}
		}

		key := identityKey(group, kind, ns, r.name)
		if visited[key] {
			continue
		}
		visited[key] = true

		n.Children = append(n.Children, w.fetchChild(ctx, cf, i, r, target, rerr, ns, depth+1, visited, st))
	}
	return n
}

func (w *walker) fetchChild(ctx context.Context, cf *childFetcher, i int, r ref, target k8s.Target, resolveErr error, ns string, depth int, visited map[string]bool, st *Stats) *Node {
	if resolveErr != nil {
		st.Nodes++
		return &Node{APIVersion: r.apiVersion, Kind: r.kind, Name: r.name, Namespace: r.namespace, State: StatePending, Error: resolveErr.Error(), depth: depth}
	}

	obj, err := cf.get(ctx, i, target, ns, r.name)
	if err != nil {
		st.Nodes++
		return &Node{APIVersion: r.apiVersion, Kind: r.kind, Name: r.name, Namespace: ns, State: StatePending, Error: err.Error(), depth: depth}
	}
	return w.build(ctx, obj, depth, visited, st)
}

// childFetcher serves one node's child Gets to build's loop, prefetching them
// concurrently in WINDOWS as the loop reaches them.
//
// Windowed rather than all-at-once, and the difference is not cosmetic. An
// earlier version prefetched every sibling the remaining budget allowed before
// the loop started. When an early sibling is a composite whose subtree then
// consumes the whole budget, every later prefetched sibling is thrown away — and
// that repeats at every ancestor on the DFS path. Five nested levels of "one
// composite + 240 managed resources" cost 985 Gets against the sequential 199 —
// by arithmetic on QPS 50 / burst 100, a slower walk than the sequential one. A window of walkConcurrency caps the loss at walkConcurrency-1 per
// ancestor (plus diamonds, below) at the same round-trip count: a flat node of
// 30 children is still 3 rounds of 10.
//
// Each window starts at the ref the loop is on, which fetchChild has ALREADY
// marked visited — so that ref is taken as given rather than filtered, or the
// window would exclude the one Get it was opened for.
type childFetcher struct {
	w       *walker
	parent  *unstructured.Unstructured
	refs    []ref
	visited map[string]bool
	st      *Stats
	next    int // refs before this index have been considered by some window
	cache   map[getKey]fetched
}

// get returns the prefetched result for exactly these Get inputs, or opens a
// window at ref i and tries again, or performs the Get itself. The cache is
// written only between the loop's steps (windows are joined before get
// returns), so it needs no lock.
func (c *childFetcher) get(ctx context.Context, i int, target k8s.Target, ns, name string) (*unstructured.Unstructured, error) {
	k := getKey{target: target, ns: ns, name: name}
	if f, ok := c.cache[k]; ok {
		return f.obj, f.err
	}
	if c.w.concurrency > 1 && i >= c.next {
		c.window(ctx, i, k)
		if f, ok := c.cache[k]; ok {
			return f.obj, f.err
		}
	}
	return c.w.cl.Get(ctx, target, ns, name)
}

// window fetches, concurrently, the Get for ref i (key cur) plus the next
// survivors after it, up to walkConcurrency in all and never more than the
// remaining node budget, then joins them.
//
// Survivors mirror the loop's filtering — non-empty, not yet visited, not a
// repeat of an identity already in this window — so the window fetches (nearly)
// only what the loop will use. The budget cut is sound: every survivor the loop
// passes adds a node, either as its own child or inside an earlier sibling's
// subtree (a key is only ever in visited because its node was built), so the
// loop hits the budget before it could need more. Refs that fail to resolve
// here count toward the budget, since the loop makes error nodes of them, but
// are not fetched.
//
// Still wasted: a window-mate that an earlier sibling's subtree reaches first
// (a diamond), and the tail of the last window at a node whose budget runs out.
func (c *childFetcher) window(ctx context.Context, i int, cur getKey) {
	budget := maxNodes - c.st.Nodes
	keys := []getKey{cur}
	seen := map[string]bool{identityKey(cur.target.GVR.Group, cur.target.Kind, cur.ns, cur.name): true}
	j := i + 1
	for ; j < len(c.refs) && len(keys) < c.w.concurrency && len(seen) < budget; j++ {
		r := c.refs[j]
		if r.name == "" || r.kind == "" {
			continue
		}
		// Same resolution and key normalisation as the loop, so this filter
		// agrees with the loop's visited check.
		target, err := c.w.cl.Resolve(r.apiVersion, r.kind)
		group, kind, ns := groupOf(r.apiVersion), r.kind, r.namespace
		if err == nil {
			group, kind = target.GVR.Group, target.Kind
			if target.Namespaced && ns == "" {
				ns = c.parent.GetNamespace()
			}
		}
		id := identityKey(group, kind, ns, r.name)
		if c.visited[id] || seen[id] {
			continue
		}
		seen[id] = true
		if err == nil {
			keys = append(keys, getKey{target: target, ns: ns, name: r.name})
		}
	}
	c.next = j
	// One fetch gains nothing from a goroutine; let get make it.
	if len(keys) < 2 {
		return
	}

	results := make([]fetched, len(keys))
	done := make([]bool, len(keys))
	var wg sync.WaitGroup
	for n, k := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ctx.Err() != nil {
				// An optimisation, not a guard: skip a Get that can only fail.
				// get's own fallback Get then fails the same way the sequential
				// walk would, with or without this check.
				return
			}
			obj, err := c.w.cl.Get(ctx, k.target, k.ns, k.name)
			results[n] = fetched{obj: obj, err: err}
			done[n] = true
		}()
	}
	wg.Wait()

	if c.cache == nil {
		c.cache = make(map[getKey]fetched, len(keys))
	}
	for n, k := range keys {
		if done[n] {
			c.cache[k] = results[n]
		}
	}
}

// childRefs collects downward references to composed/managed resources.
//
// The location of composed-resource refs differs by Crossplane version:
//   - v1 XRs put them at the top-level spec.resourceRefs.
//   - v2 namespaced XRs nest Crossplane machinery under spec.crossplane, so the
//     refs live at spec.crossplane.resourceRefs.
//
// A v1 Claim points to its XR via the single spec.resourceRef.
func childRefs(obj *unstructured.Unstructured) []ref {
	var refs []ref
	for _, path := range [][]string{
		{"spec", "resourceRefs"},               // v1 composite → composed
		{"spec", "crossplane", "resourceRefs"}, // v2 composite → composed
	} {
		if list, found, _ := unstructured.NestedSlice(obj.Object, path...); found {
			for _, it := range list {
				if m, ok := it.(map[string]any); ok {
					refs = append(refs, refFromMap(m))
				}
			}
		}
	}
	if m, found, _ := unstructured.NestedMap(obj.Object, "spec", "resourceRef"); found {
		refs = append(refs, refFromMap(m))
	}
	return refs
}

func refFromMap(m map[string]any) ref {
	return ref{
		apiVersion: str(m, "apiVersion"),
		kind:       str(m, "kind"),
		name:       str(m, "name"),
		namespace:  str(m, "namespace"),
	}
}
