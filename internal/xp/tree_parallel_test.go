package xp

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/briferz/crossplane-mcp/internal/k8s"
)

// The parallel walk is only acceptable if it is invisible: the DFS decides
// inclusion, placement and the node budget in an order-dependent way, so the
// prefetch must never change what the walk returns — only how long it takes.
// buildTree(..., 1) is the sequential walk (prefetch disabled), which makes it
// the reference every test here compares against.

// fakeTree is a TreeClient over an in-memory object set. Resolve and Get are
// pure functions of their inputs unless a test opts into something else, which
// is the condition under which "parallel == sequential" is a meaningful claim.
type fakeTree struct {
	kinds map[string]fakeKind // by Kind
	objs  map[getKey]*unstructured.Unstructured

	latency func(k getKey) time.Duration // nil = no delay

	// resolveFailOnce makes the first Resolve of these kinds fail — a transient
	// discovery error.
	resolveFailOnce map[string]bool

	injected atomic.Int32 // resolveFailOnce failures actually returned

	mu          sync.Mutex
	resolveSeen map[string]int
	gets        map[getKey]int
	inFlight    atomic.Int32
	maxInFlight atomic.Int32
	active      atomic.Int32 // Gets currently executing; must be 0 after BuildTree
}

type fakeKind struct {
	group      string
	resource   string
	namespaced bool
}

func newFakeTree() *fakeTree {
	return &fakeTree{
		kinds: map[string]fakeKind{
			"XApp":     {group: "example.org", resource: "xapps", namespaced: true},
			"XCluster": {group: "example.org", resource: "xclusters", namespaced: false},
			"Bucket":   {group: "aws.example.org", resource: "buckets", namespaced: true},
			"Role":     {group: "iam.example.org", resource: "roles", namespaced: false},
		},
		objs:        map[getKey]*unstructured.Unstructured{},
		resolveSeen: map[string]int{},
		gets:        map[getKey]int{},
	}
}

func (f *fakeTree) Resolve(apiVersion, kind string) (k8s.Target, error) {
	f.mu.Lock()
	f.resolveSeen[kind]++
	first := f.resolveSeen[kind] == 1
	f.mu.Unlock()
	if first && f.resolveFailOnce[kind] {
		f.injected.Add(1)
		return k8s.Target{}, errors.New("discovery: transient transport error for " + kind)
	}
	fk, ok := f.kinds[kind]
	if !ok {
		return k8s.Target{}, fmt.Errorf("no kind %q is registered", kind)
	}
	group, version := groupOf(apiVersion), apiVersion
	if i := strings.IndexByte(apiVersion, '/'); i >= 0 {
		version = apiVersion[i+1:]
	}
	if group != fk.group {
		return k8s.Target{}, fmt.Errorf("kind %q is not served in group %q", kind, group)
	}
	return k8s.Target{
		GVR:        schema.GroupVersionResource{Group: group, Version: version, Resource: fk.resource},
		Namespaced: fk.namespaced,
		Kind:       kind,
	}, nil
}

func (f *fakeTree) Get(ctx context.Context, t k8s.Target, ns, name string) (*unstructured.Unstructured, error) {
	f.active.Add(1)
	defer f.active.Add(-1)
	cur := f.inFlight.Add(1)
	defer f.inFlight.Add(-1)
	for {
		prev := f.maxInFlight.Load()
		if cur <= prev || f.maxInFlight.CompareAndSwap(prev, cur) {
			break
		}
	}

	k := getKey{target: t, ns: ns, name: name}
	f.mu.Lock()
	f.gets[k]++
	f.mu.Unlock()

	if f.latency != nil {
		select {
		case <-time.After(f.latency(k)):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if t.Namespaced && ns == "" {
		return nil, fmt.Errorf("namespace is required for namespaced kind %q", t.Kind)
	}
	if o, ok := f.objs[k]; ok {
		return o.DeepCopy(), nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Group: t.GVR.Group, Resource: t.GVR.Resource}, name)
}

// requireConcurrent fails a test that would pass just the same with the
// prefetch disabled: if at most one Get was ever in flight, the concurrent path
// it claims to exercise never ran.
func (f *fakeTree) requireConcurrent(t *testing.T) {
	t.Helper()
	if peak := f.maxInFlight.Load(); peak < 2 {
		t.Fatalf("at most %d Get in flight: the concurrent path never ran, so this test proved nothing", peak)
	}
}

func (f *fakeTree) resetCounters() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolveSeen = map[string]int{}
	f.gets = map[getKey]int{}
	f.maxInFlight.Store(0)
}

func (f *fakeTree) totalGets() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.gets {
		n += c
	}
	return n
}

func (f *fakeTree) duplicateGets() []getKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	var dup []getKey
	for k, c := range f.gets {
		if c > 1 {
			dup = append(dup, k)
		}
	}
	return dup
}

func apiVersionFor(kind, version string) string {
	switch kind {
	case "XApp", "XCluster":
		return "example.org/" + version
	case "Bucket":
		return "aws.example.org/" + version
	case "Role":
		return "iam.example.org/" + version
	}
	return "unknown.example.org/" + version
}

// add registers an object and returns it so refs can be attached.
func (f *fakeTree) add(kind, version, ns, name string, conds ...map[string]any) *unstructured.Unstructured {
	fk := f.kinds[kind]
	if !fk.namespaced {
		ns = ""
	}
	o := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersionFor(kind, version),
		"kind":       kind,
		"metadata":   map[string]any{"name": name, "uid": kind + "/" + ns + "/" + name + "@" + version},
	}}
	if ns != "" {
		_ = unstructured.SetNestedField(o.Object, ns, "metadata", "namespace")
	}
	if len(conds) > 0 {
		list := make([]any, len(conds))
		for i, c := range conds {
			list[i] = c
		}
		_ = unstructured.SetNestedSlice(o.Object, list, "status", "conditions")
	}
	t, err := f.Resolve(o.GetAPIVersion(), kind)
	if err != nil {
		panic(err)
	}
	f.objs[getKey{target: t, ns: ns, name: name}] = o
	return o
}

func addRefs(o *unstructured.Unstructured, refs ...map[string]any) {
	existing, _, _ := unstructured.NestedSlice(o.Object, "spec", "resourceRefs")
	for _, r := range refs {
		existing = append(existing, r)
	}
	_ = unstructured.SetNestedSlice(o.Object, existing, "spec", "resourceRefs")
}

func fref(apiVersion, kind, ns, name string) map[string]any {
	m := map[string]any{"apiVersion": apiVersion, "kind": kind, "name": name}
	if ns != "" {
		m["namespace"] = ns
	}
	return m
}

// render prints every field of the tree, unexported ones included, so two
// walks compare equal only if they built the same thing in the same place.
func render(n *Node) string {
	var b strings.Builder
	var rec func(n *Node, indent string)
	rec = func(n *Node, indent string) {
		fmt.Fprintf(&b, "%s%s %s %s/%s state=%s health=%+v err=%q uid=%s depth=%d del=%q paused=%v fin=%v native=%q conds=%+v\n",
			indent, n.APIVersion, n.Kind, n.Namespace, n.Name, n.State, n.Health, n.Error,
			n.uid, n.depth, n.deletionTime, n.paused, n.finalizers, n.nativeReasons, n.Conditions)
		for _, c := range n.Children {
			rec(c, indent+"  ")
		}
	}
	rec(n, "")
	return b.String()
}

var condPool = []map[string]any{
	{"type": "Ready", "status": "True", "reason": "Available"},
	{"type": "Ready", "status": "False", "reason": "Creating", "message": "waiting"},
	{"type": "Synced", "status": "False", "reason": "ReconcileError", "message": "boom"},
	{"type": "Ready", "status": "False"},
}

// randomTree builds a graph designed to hit every order-sensitive part of the
// walk: diamonds (a resource reachable from several parents), cycles back to
// the root, dangling refs, unresolvable kinds, empty names, refs that omit the
// namespace (inheriting the parent's, which may not match), and —
// sometimes — enough nodes to trip the budget or a chain long enough to trip the
// depth cap.
func randomTree(seed uint64) (*fakeTree, *unstructured.Unstructured) {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)) //nolint:gosec // G404: seeded on purpose — a failing seed must replay exactly
	f := newFakeTree()
	kinds := []string{"XApp", "XCluster", "Bucket", "Role"}
	nss := []string{"a", "b"}

	size := 5 + r.IntN(60)
	switch r.IntN(6) {
	case 0:
		size = maxNodes + 40 // over the node budget
	case 1:
		size = maxDepth + 6 // long enough to chain past the depth cap
	}

	var objs []*unstructured.Unstructured
	for i := range size {
		kind := kinds[r.IntN(len(kinds))]
		if i == 0 {
			kind = "XApp"
		}
		var conds []map[string]any
		if r.IntN(3) > 0 {
			conds = append(conds, condPool[r.IntN(len(condPool))])
		}
		objs = append(objs, f.add(kind, "v1", nss[r.IntN(2)], fmt.Sprintf("o%d", i), conds...))
	}

	chain := size == maxDepth+6
	for i, o := range objs {
		if chain && i+1 < len(objs) {
			// A strict chain, so depth rather than width decides the cut.
			c := objs[i+1]
			addRefs(o, fref(c.GetAPIVersion(), c.GetKind(), c.GetNamespace(), c.GetName()))
			continue
		}
		kind := o.GetKind()
		if kind == "Bucket" || kind == "Role" {
			continue // managed resources are leaves
		}
		nrefs := r.IntN(9)
		if i == 0 && size > maxNodes {
			nrefs = size // make the budget genuinely bite at the root
		}
		for range nrefs {
			switch r.IntN(12) {
			case 0: // dangling: a name that does not exist
				addRefs(o, fref(apiVersionFor("Bucket", "v1"), "Bucket", o.GetNamespace(), fmt.Sprintf("ghost%d", r.IntN(5))))
			case 1: // unresolvable kind
				addRefs(o, fref("nope.example.org/v1", "Nope", "", "x"))
			case 2: // empty name
				addRefs(o, fref(apiVersionFor("Bucket", "v1"), "Bucket", "", ""))
			case 3: // back to the root: a cycle
				addRefs(o, fref(objs[0].GetAPIVersion(), "XApp", objs[0].GetNamespace(), objs[0].GetName()))
			default:
				c := objs[r.IntN(len(objs))]
				ns := c.GetNamespace()
				if r.IntN(3) == 0 {
					ns = "" // omit: inherit the parent's namespace, which may not match
				}
				addRefs(o, fref(c.GetAPIVersion(), c.GetKind(), ns, c.GetName()))
			}
		}
	}
	return f, objs[0]
}

func TestParallelWalkMatchesSequential(t *testing.T) {
	// Completion order cannot matter by construction — each window is joined
	// before the loop reads it — so latency here exists only to shake goroutine
	// interleavings for the race detector, not to reorder anything the DFS sees.
	latencies := map[string]func(seed uint64) func(getKey) time.Duration{
		"none": func(uint64) func(getKey) time.Duration { return nil },
		"random": func(seed uint64) func(getKey) time.Duration {
			var mu sync.Mutex
			r := rand.New(rand.NewPCG(seed, 7)) //nolint:gosec // G404: seeded on purpose — a failing seed must replay exactly
			return func(getKey) time.Duration {
				mu.Lock()
				defer mu.Unlock()
				return time.Duration(r.IntN(300)) * time.Microsecond
			}
		},
	}

	seeds := uint64(150)
	if testing.Short() {
		seeds = 25
	}
	// The corpus must actually contain every order-sensitive case, or equality
	// over it proves nothing about that case. Counted per latency profile.
	type coverage struct{ nodeCap, depthCap, errNodes, diamonds int }
	for lname, mk := range latencies {
		t.Run(lname, func(t *testing.T) {
			var cov coverage
			for seed := range seeds {
				f, root := randomTree(seed)

				wantNode, wantStats := buildTree(context.Background(), f, root, 1)
				want := render(wantNode)
				wantGets := f.totalGets()

				f.resetCounters()
				f.latency = mk(seed)
				gotNode, gotStats := buildTree(context.Background(), f, root, walkConcurrency)
				f.latency = nil

				if got := render(gotNode); got != want {
					t.Fatalf("seed %d: parallel walk built a different tree\n--- sequential ---\n%s--- parallel ---\n%s", seed, want, got)
				}
				if gotStats != wantStats {
					t.Fatalf("seed %d: Stats differ: sequential %+v, parallel %+v", seed, wantStats, gotStats)
				}
				// A loose sanity ceiling only. The random corpus does not build wide
				// NESTED composites, so it cannot catch waste compounding across
				// ancestors — TestParallelWalkWasteDoesNotCompound pins that bound.
				if got := f.totalGets(); got > 2*wantGets+1 {
					t.Fatalf("seed %d: %d Gets in parallel vs %d sequential", seed, got, wantGets)
				}
				if n := f.active.Load(); n != 0 {
					t.Fatalf("seed %d: %d Gets still running after BuildTree returned", seed, n)
				}

				flat := wantNode.Flatten()
				if wantStats.Capped && wantStats.Nodes >= maxNodes {
					cov.nodeCap++
				}
				for _, fn := range flat {
					if wantStats.Capped && fn.Depth == maxDepth {
						cov.depthCap++
						break
					}
				}
				for _, fn := range flat {
					if fn.Error != "" {
						cov.errNodes++
						break
					}
				}
				// Waste in an UNCAPPED walk can only come from a diamond (a window-mate
				// an earlier sibling's subtree reached first), so this counts real
				// diamonds rather than budget-tail waste.
				if !wantStats.Capped && f.totalGets() > wantGets {
					cov.diamonds++
				}
				f.resetCounters()
			}
			t.Logf("coverage over %d seeds: %+v", seeds, cov)
			if cov.nodeCap == 0 || cov.depthCap == 0 || cov.errNodes == 0 || cov.diamonds == 0 {
				t.Fatalf("the random corpus never exercised a case it exists to test: %+v", cov)
			}
		})
	}
}

// A wide, flat composite — the common Crossplane shape, one XR over many
// managed resources — must be fetched concurrently, within the bound.
func TestParallelWalkIsActuallyConcurrent(t *testing.T) {
	f := newFakeTree()
	root := f.add("XApp", "v1", "a", "root")
	for i := range 30 {
		b := f.add("Bucket", "v1", "a", fmt.Sprintf("b%d", i))
		addRefs(root, fref(b.GetAPIVersion(), "Bucket", "a", b.GetName()))
	}
	f.latency = func(getKey) time.Duration { return 20 * time.Millisecond }

	node, _ := BuildTree(context.Background(), f, root)

	if len(node.Children) != 30 {
		t.Fatalf("walked %d children, want 30", len(node.Children))
	}
	peak := f.maxInFlight.Load()
	if peak < 2 {
		t.Fatalf("at most %d Get in flight: the walk is still sequential", peak)
	}
	if peak > walkConcurrency {
		t.Fatalf("%d Gets in flight, above the walkConcurrency bound of %d", peak, walkConcurrency)
	}
}

// Keys are Get's full input. Two refs that share a name and namespace but not a
// kind — or share a name but inherit different namespaces from different
// parents — must each get their own object. A prefetch keyed on anything less
// would hand one ref the other's object.
func TestParallelWalkKeysOnFullGetInput(t *testing.T) {
	f := newFakeTree()
	root := f.add("XCluster", "v1", "", "root")
	pa := f.add("XApp", "v1", "a", "pa")
	pb := f.add("XApp", "v1", "b", "pb")
	addRefs(root, fref(pa.GetAPIVersion(), "XApp", "a", "pa"), fref(pb.GetAPIVersion(), "XApp", "b", "pb"))

	// Only a/c exists. Both parents reference "c" WITHOUT a namespace, so each
	// inherits its own: under pb it must be a NotFound, not a's object.
	f.add("Bucket", "v1", "a", "c", condPool[0])
	f.add("XApp", "v1", "a", "same")                // same name + ns as the Bucket below...
	f.add("Bucket", "v1", "a", "same", condPool[2]) // ...different kind
	for _, p := range []*unstructured.Unstructured{pa, pb} {
		addRefs(p,
			fref(apiVersionFor("Bucket", "v1"), "Bucket", "", "c"),
			fref(apiVersionFor("XApp", "v1"), "XApp", "a", "same"),
			fref(apiVersionFor("Bucket", "v1"), "Bucket", "a", "same"),
		)
	}

	want, _ := buildTree(context.Background(), f, root, 1)
	f.resetCounters()
	f.latency = func(getKey) time.Duration { return 2 * time.Millisecond }
	got, _ := buildTree(context.Background(), f, root, walkConcurrency)
	f.requireConcurrent(t)
	if render(got) != render(want) {
		t.Fatalf("parallel differs from sequential\n--- sequential ---\n%s--- parallel ---\n%s", render(want), render(got))
	}

	var bc *Node
	for _, p := range got.Children {
		if p.Name == "pb" {
			for _, c := range p.Children {
				if c.Name == "c" {
					bc = c
				}
			}
		}
	}
	if bc == nil || bc.Error == "" || !strings.Contains(bc.Error, "not found") {
		t.Fatalf("b/c must be a NotFound error node (only a/c exists), got %+v", bc)
	}
}

// Resolve must not be cached. A transient discovery failure seen only by a
// prefetch window must NOT become an error node: the loop resolves again and
// wins. With a memo shared between the two, the failure would be pinned onto
// every ref of that kind — each an unreachable node, tier 1, deepest-first, and
// likely the named root cause.
//
// The Role comes first on purpose. The loop resolves the ref it is on BEFORE a
// window opens, so the first Bucket's Resolve must happen inside the window —
// behind a ref of another kind — or the failure lands on the loop's own call,
// which is simply the sequential behaviour and proves nothing about caching.
func TestParallelWalkDoesNotPinTransientResolveFailures(t *testing.T) {
	f := newFakeTree()
	root := f.add("XApp", "v1", "a", "root")
	r0 := f.add("Role", "v1", "", "r0")
	addRefs(root, fref(r0.GetAPIVersion(), "Role", "", "r0"))
	for i := range 3 {
		b := f.add("Bucket", "v1", "a", fmt.Sprintf("b%d", i))
		addRefs(root, fref(b.GetAPIVersion(), "Bucket", "a", b.GetName()))
	}
	// add() resolves every object to key it, so the "first call" would already
	// be spent before the walk began and the failure would never fire. Reset,
	// then arm — and assert below that it really was injected. Without both, an
	// earlier version of this test passed with a memoised Resolve in place.
	f.resetCounters()
	f.resolveFailOnce = map[string]bool{"Bucket": true}

	node, _ := BuildTree(context.Background(), f, root)

	if f.injected.Load() == 0 {
		t.Fatal("no Resolve failure was injected, so this test proved nothing")
	}
	// The sequential walk makes b0 an error node here: its single Resolve is the
	// failing one. The parallel walk's window absorbs it and the loop's own
	// Resolve succeeds — better, never worse.
	for _, c := range node.Children {
		if c.Error != "" {
			t.Errorf("%s/%s became an error node from a Resolve failure only the prefetch saw: %s", c.Kind, c.Name, c.Error)
		}
	}
	// b0 missed the window (its window Resolve failed) and sits below c.next when
	// the loop reaches it. That is the one path where get() must fall back to a
	// plain Get rather than reopen a window — reopening would re-fetch the
	// window-mates already cached. Nothing else in the suite reaches it.
	if dup := f.duplicateGets(); len(dup) > 0 {
		t.Errorf("fetched more than once: %v — a window was reopened below c.next", dup)
	}
}

// Under the budget there is nothing for the prefetch to guess: a flat node's
// children are fetched exactly once each, the same Gets the sequential walk
// makes. Over the budget, the cut must hold — an unbudgeted prefetch of a
// 240-wide node would fetch refs the loop never reaches.
func TestParallelWalkFetchesNothingExtraOnAFlatNode(t *testing.T) {
	for _, width := range []int{25, maxNodes + 40} {
		f := newFakeTree()
		root := f.add("XApp", "v1", "a", "root")
		for i := range width {
			b := f.add("Bucket", "v1", "a", fmt.Sprintf("b%d", i))
			addRefs(root, fref(b.GetAPIVersion(), "Bucket", "a", b.GetName()))
		}
		// And a ref back to the root: the prefetch must honour visited too.
		addRefs(root, fref(root.GetAPIVersion(), "XApp", "a", "root"))

		_, _ = buildTree(context.Background(), f, root, 1)
		want := f.totalGets()
		f.resetCounters()

		f.latency = func(getKey) time.Duration { return time.Millisecond }
		_, _ = buildTree(context.Background(), f, root, walkConcurrency)
		f.requireConcurrent(t)
		if got := f.totalGets(); got != want {
			t.Errorf("width %d: %d Gets in parallel, %d sequential", width, got, want)
		}
		if dup := f.duplicateGets(); len(dup) > 0 {
			t.Errorf("width %d: fetched more than once: %v", width, dup)
		}
	}
}

// Cancelling mid-walk must return promptly and leave nothing running. The
// resulting tree is NOT compared with the sequential walk: which fetches
// finished before the cancel is timing, in both.
func TestParallelWalkStopsOnCancel(t *testing.T) {
	f := newFakeTree()
	root := f.add("XApp", "v1", "a", "root")
	for i := range 40 {
		b := f.add("Bucket", "v1", "a", fmt.Sprintf("b%d", i))
		addRefs(root, fref(b.GetAPIVersion(), "Bucket", "a", b.GetName()))
	}
	f.latency = func(getKey) time.Duration { return time.Hour }

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for f.active.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()

	done := make(chan struct{})
	go func() {
		_, _ = BuildTree(ctx, f, root)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("BuildTree did not return after its context was cancelled")
	}
	if n := f.active.Load(); n != 0 {
		t.Fatalf("%d Gets still running after BuildTree returned", n)
	}
	// The sequential walk passes everything above too; this is what makes the
	// test about the concurrent path — Gets were in flight together when the
	// cancel landed, and none survived it.
	f.requireConcurrent(t)
}

// latencyEvents is an EventFetcher whose answers depend on the uid asked about,
// so a result delivered to the wrong suspect is detectable, and whose latency
// runs in REVERSE suspect order so completion order is the opposite of the
// order diagnose reads them in.
type latencyEvents struct {
	order       map[string]int // uid -> suspect index, for reverse latency
	inFlight    atomic.Int32
	maxInFlight atomic.Int32
}

func (l *latencyEvents) Events(ctx context.Context, _, uid string, _ int) ([]k8s.Event, error) {
	cur := l.inFlight.Add(1)
	defer l.inFlight.Add(-1)
	for {
		prev := l.maxInFlight.Load()
		if cur <= prev || l.maxInFlight.CompareAndSwap(prev, cur) {
			break
		}
	}
	select {
	case <-time.After(time.Duration(20-l.order[uid]) * 2 * time.Millisecond):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return []k8s.Event{{Type: "Warning", Reason: "For", Message: "events-of-" + uid}}, nil
}

// Diagnose fetches suspects' events concurrently; each suspect must still get
// its OWN events, however the fetches complete.
func TestDiagnoseEventsFetchedConcurrentlyAndMatched(t *testing.T) {
	var kids []*Node
	for i := range 8 {
		n := node(1, "Bucket", fmt.Sprintf("b%d", i), []Condition{cond("Ready", "False", "Creating", "waiting")})
		n.uid = fmt.Sprintf("uid-%d", i)
		kids = append(kids, n)
	}
	root := node(0, "App", "a", []Condition{cond("Ready", "False", "Creating", "waiting")}, kids...)
	root.uid = "uid-root"

	ev := &latencyEvents{order: map[string]int{"uid-root": 9}}
	for i := range 8 {
		ev.order[fmt.Sprintf("uid-%d", i)] = i
	}

	d := Diagnose(context.Background(), ev, root, Stats{Nodes: 9}, false)

	if peak := ev.maxInFlight.Load(); peak < 2 {
		t.Fatalf("at most %d event lookup in flight: still sequential", peak)
	}
	if len(d.Suspects) == 0 {
		t.Fatal("expected suspects")
	}
	byName := map[string]string{"a": "uid-root"}
	for i := range 8 {
		byName[fmt.Sprintf("b%d", i)] = fmt.Sprintf("uid-%d", i)
	}
	for _, s := range d.Suspects {
		want := "events-of-" + byName[s.Name]
		if len(s.Events) == 0 || s.Events[0].Message != want {
			t.Errorf("%s got events %+v, want its own (%q)", s.Name, s.Events, want)
		}
	}
}

// Benchmarks: a fake client whose Gets each cost a simulated 10ms round-trip and
// which has NO rate limiter, so the numbers read as "how many round-trips does
// this shape cost". The real client's QPS/Burst floor narrows the gap on large
// trees. Not run in CI.
func benchShape(b *testing.B, build func(f *fakeTree) *unstructured.Unstructured) {
	for _, conc := range []int{1, walkConcurrency} {
		name := "sequential"
		if conc > 1 {
			name = "parallel"
		}
		b.Run(name, func(b *testing.B) {
			f := newFakeTree()
			root := build(f)
			f.latency = func(getKey) time.Duration { return 10 * time.Millisecond }
			b.ResetTimer()
			for range b.N {
				buildTree(context.Background(), f, root, conc)
			}
		})
	}
}

// One XR over thirty managed resources: the common Crossplane shape.
func BenchmarkWalkFlat(b *testing.B) {
	benchShape(b, func(f *fakeTree) *unstructured.Unstructured {
		root := f.add("XApp", "v1", "a", "root")
		for i := range 30 {
			addRefs(root, fref(apiVersionFor("Bucket", "v1"), "Bucket", "a", f.add("Bucket", "v1", "a", fmt.Sprintf("b%d", i)).GetName()))
		}
		return root
	})
}

// An XR over five nested XRs of six managed resources each.
func BenchmarkWalkNested(b *testing.B) {
	benchShape(b, func(f *fakeTree) *unstructured.Unstructured {
		root := f.add("XApp", "v1", "a", "root")
		for i := range 5 {
			sub := f.add("XApp", "v1", "a", fmt.Sprintf("x%d", i))
			addRefs(root, fref(sub.GetAPIVersion(), "XApp", "a", sub.GetName()))
			for j := range 6 {
				addRefs(sub, fref(apiVersionFor("Bucket", "v1"), "Bucket", "a", f.add("Bucket", "v1", "a", fmt.Sprintf("b%d-%d", i, j)).GetName()))
			}
		}
		return root
	})
}

// A chain of ten composites, each also owning one managed resource: the shape
// this design helps least, since a child's refs are unknown until it arrives.
func BenchmarkWalkChain(b *testing.B) {
	benchShape(b, func(f *fakeTree) *unstructured.Unstructured {
		root := f.add("XApp", "v1", "a", "c0")
		prev := root
		for i := 1; i <= 10; i++ {
			next := f.add("XApp", "v1", "a", fmt.Sprintf("c%d", i))
			leaf := f.add("Bucket", "v1", "a", fmt.Sprintf("leaf%d", i))
			addRefs(prev, fref(next.GetAPIVersion(), "XApp", "a", next.GetName()), fref(leaf.GetAPIVersion(), "Bucket", "a", leaf.GetName()))
			prev = next
		}
		return root
	})
}

// TestParallelWalkWasteDoesNotCompound pins a defect caught in review before
// merge: an earlier version prefetched every sibling the remaining budget
// allowed before the loop started. When the first sibling is a composite whose
// subtree then eats the whole budget, every later prefetched sibling is
// discarded — at EVERY ancestor. Five levels of "one composite + 240 managed
// resources" cost 985 Gets against the sequential 199 — by arithmetic on QPS 50
// / burst 100, a ~18s limiter floor against ~2s, i.e. slower than sequential.
//
// With windows of walkConcurrency the loss is at most walkConcurrency-1 per
// ancestor. The trees are also compared, since the fix must not buy its bound
// by fetching something different.
func TestParallelWalkWasteDoesNotCompound(t *testing.T) {
	for _, levels := range []int{1, 2, 3, 5} {
		f := newFakeTree()
		root := f.add("XApp", "v1", "a", "L0")
		cur := root
		for l := 1; l <= levels; l++ {
			next := f.add("XApp", "v1", "a", fmt.Sprintf("L%d", l))
			addRefs(cur, fref(next.GetAPIVersion(), "XApp", "a", next.GetName()))
			for i := range maxNodes + 40 {
				b := f.add("Bucket", "v1", "a", fmt.Sprintf("b%d-%d", l, i))
				addRefs(cur, fref(b.GetAPIVersion(), "Bucket", "a", b.GetName()))
			}
			cur = next
		}
		f.resetCounters()

		wantNode, wantStats := buildTree(context.Background(), f, root, 1)
		seq := f.totalGets()
		f.resetCounters()
		gotNode, gotStats := buildTree(context.Background(), f, root, walkConcurrency)
		par := f.totalGets()

		if render(gotNode) != render(wantNode) || gotStats != wantStats {
			t.Fatalf("levels=%d: parallel built a different tree", levels)
		}
		// levels-1, not levels: the deepest level is walked up to the cap, so it
		// wastes nothing. A looser bound would let one extra window slip through.
		if limit := seq + (walkConcurrency-1)*(levels-1); par > limit {
			t.Errorf("levels=%d: %d Gets in parallel vs %d sequential — above the %d bound "+
				"(walkConcurrency-1 per wasting ancestor); waste is compounding again", levels, par, seq, limit)
		}
	}
}
