package xp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/briferz/crossplane-mcp/internal/k8s"
)

// Condition types specific to Crossplane package objects (Provider / Function /
// Configuration and their revisions). They are used only to surface named
// statuses on rows — classification deliberately folds over ALL conditions
// instead of gating on a type list, because the package condition vocabulary
// changes across Crossplane versions: revisions carry a single Healthy on 1.x
// but RevisionHealthy/RuntimeHealthy on 2.x, and Verified existed only on
// 1.19–2.1.
const (
	TypeInstalled       = "Installed"       // packages: an active revision exists
	TypeRevisionHealthy = "RevisionHealthy" // revisions, Crossplane 2.x: package contents health
	TypeRuntimeHealthy  = "RuntimeHealthy"  // Provider/FunctionRevisions, 2.x: runtime Deployment health
	TypeVerified        = "Verified"        // revisions, 1.19–2.1: image signature verification
)

// parentPackageLabel is the label the package manager stamps on every revision
// with its parent package's name — the manager's own lookup key, and ours.
const parentPackageLabel = "pkg.crossplane.io/package"

// PackageRow is one package (Provider/Function/Configuration) in a
// list_providers / list_functions / list_configurations response. A healthy
// package costs only the small identity/status fields; the enrichment fields
// (reasons, skew, lifecycle, revisions, events) render only when they carry
// signal, keeping the steady state token-light without ever truncating a
// condition message.
type PackageRow struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"` // feeds straight into get_resource
	Name       string `json:"name"`
	// Package is spec.package verbatim — the OCI reference including its tag or
	// digest. There is deliberately no parsed "version" field: a digest pin or
	// an ImageConfig rewrite would make one wrong.
	Package string `json:"package,omitempty"`
	State   string `json:"state"`
	// Installed/Healthy are the package's condition statuses. Synced appears
	// only when the package is paused (Crossplane sets Synced=False with reason
	// ReconcilePaused; on unpause the conditions are wiped).
	Installed string `json:"installed,omitempty"`
	Healthy   string `json:"healthy,omitempty"`
	Synced    string `json:"synced,omitempty"`
	Paused    bool   `json:"paused,omitempty"`
	// CurrentRevision is status.currentRevision: the NEWEST revision, named
	// before it is active or healthy — not necessarily the one serving.
	CurrentRevision string `json:"currentRevision,omitempty"`
	// RevisionCount is the pre-cap number of revisions matched to this package.
	// 0 is meaningful (no unpack ever produced a revision) and still rendered;
	// nil means revisions could not be listed at all (e.g. RBAC), so missing
	// data is never mistaken for "no revisions".
	RevisionCount *int `json:"revisionCount,omitempty"`
	// ActivationPolicy surfaces spec.revisionActivationPolicy only when it is
	// "Manual" — the approval trap, visible before it fires.
	ActivationPolicy string `json:"activationPolicy,omitempty"`
	// CurrentIdentifier is status.currentIdentifier, only when it differs from
	// Package: the last source that successfully unpacked. A mismatch is the
	// stuck-upgrade smoking gun.
	CurrentIdentifier string `json:"currentIdentifier,omitempty"`
	// ResolvedPackage is status.resolvedPackage, only when it differs from
	// Package: an ImageConfig path rewrite. Explicitly NOT skew.
	ResolvedPackage string `json:"resolvedPackage,omitempty"`
	// Lifecycle labels a failing package the way diagnose labels suspects —
	// "Installing (blocked, 3h)", "Paused (blocked, 5d)",
	// "Terminating (stuck 140d)" — so how long it has been stuck is visible.
	Lifecycle         string   `json:"lifecycle,omitempty"`
	DeletionTimestamp string   `json:"deletionTimestamp,omitempty"`
	Finalizers        []string `json:"finalizers,omitempty"` // only while terminating
	// Reasons carries the failing/Unknown condition lines, full and never
	// truncated.
	Reasons []string `json:"reasons,omitempty"`
	// Skew lists upgrade/rollout anomalies derived from stable spec/status
	// fields (never from reason strings): a stuck unpack, an un-approved
	// Manual revision, a wedged rollout, GC lag, or package health lagging a
	// failing new revision.
	Skew   []string    `json:"skew,omitempty"`
	Events []k8s.Event `json:"events,omitempty"`
	// Revisions renders only when it carries signal (failing/extra/missing
	// active revisions); a healthy steady-state package pays only
	// CurrentRevision + RevisionCount.
	Revisions          []RevisionRow `json:"revisions,omitempty"`
	RevisionsTruncated bool          `json:"revisionsTruncated,omitempty"`

	uid string // event-decoration handle; never serialized
}

// RevisionRow is one package revision attached to a PackageRow. For providers
// and functions its name is by default also the name of the revision's runtime
// Deployment (a DeploymentRuntimeConfig can override it) — the pivot to
// pod-level detail outside this server.
type RevisionRow struct {
	Name         string `json:"name"`
	Revision     int64  `json:"revision"` // spec.revision ordinal; highest = newest
	DesiredState string `json:"desiredState,omitempty"`
	Current      bool   `json:"current,omitempty"` // named by the parent's status.currentRevision
	State        string `json:"state"`
	// Healthy is the 1.x single revision condition; RevisionHealthy (contents)
	// and RuntimeHealthy (runtime Deployment; never on ConfigurationRevisions)
	// replace it on 2.x; Verified existed on 1.19–2.1 only. The row shape
	// self-adapts to whatever the cluster serves.
	Healthy         string `json:"healthy,omitempty"`
	RevisionHealthy string `json:"revisionHealthy,omitempty"`
	RuntimeHealthy  string `json:"runtimeHealthy,omitempty"`
	Verified        string `json:"verified,omitempty"`
	// Image is spec.image, only when it differs from the parent's Package —
	// present exactly when it disambiguates an old revision from the new one.
	Image string `json:"image,omitempty"`
	// Deps summarises the revision's dependency tallies, only when something is
	// off (invalid > 0 or installed < found).
	Deps              *DepCounts  `json:"deps,omitempty"`
	Paused            bool        `json:"paused,omitempty"`
	DeletionTimestamp string      `json:"deletionTimestamp,omitempty"`
	Reasons           []string    `json:"reasons,omitempty"`
	Events            []k8s.Event `json:"events,omitempty"`

	uid string
}

// DepCounts mirrors a revision's status dependency tallies.
type DepCounts struct {
	Found     int64 `json:"found"`
	Installed int64 `json:"installed"`
	Invalid   int64 `json:"invalid"`
}

// PackagesParams tunes BuildPackages.
type PackagesParams struct {
	Name          string // case-insensitive substring against object name AND spec.package; "" matches all
	UnhealthyOnly bool   // keep only Blocked/Pending rows (all are still counted in Summary)
	Limit         int    // max Items; <=0 means no cap
	// RevisionsListed reports whether the revision kind was actually listed.
	// When false (RBAC denial, discovery gap, aborted list), revision-derived
	// output — rows, counts, and the skew sentences that need revisions — is
	// suppressed rather than letting missing data read as "no active revision".
	RevisionsListed bool
}

// PackagesResult is the classified, filtered, sorted, capped package-health
// output, before event decoration.
type PackagesResult struct {
	Items     []PackageRow
	Summary   UnhealthySummary
	Scanned   int
	Truncated bool
	Notes     []string
}

// BuildPackages classifies each package, correlates its revisions, derives
// skew, orders most-actionable first (Blocked before Pending, then kind/name),
// and caps Items while keeping honest pre-cap Summary and Scanned totals
// (packages only — revisions are attachments, never counted). Pure: no cluster
// access, fully unit-testable.
func BuildPackages(pkgs, revs []k8s.Listed, p PackagesParams) *PackagesResult {
	res := &PackagesResult{}

	byName := map[string]int{}
	byUID := map[string]int{}
	for i := range pkgs {
		obj := &pkgs[i].Object
		byName[obj.GetName()] = i
		if uid := string(obj.GetUID()); uid != "" {
			byUID[uid] = i
		}
	}

	// Correlate revisions to their parent package. Orphans (parent filtered by
	// RBAC, label/ownerRef stripped, or mid-deletion) become a note, never a
	// row — a revision is meaningless without its package context.
	matched := map[int][]int{}
	var orphans []string
	for j := range revs {
		obj := &revs[j].Object
		if i, ok := parentIndex(obj, pkgs, byName, byUID); ok {
			matched[i] = append(matched[i], j)
		} else {
			orphans = append(orphans, obj.GetName())
		}
	}
	if len(orphans) > 0 {
		sort.Strings(orphans)
		res.Notes = append(res.Notes, fmt.Sprintf(
			"%d revision(s) matched no listed package: %s (parent unreadable, deleted, or label/ownerRef stripped)",
			len(orphans), strings.Join(orphans, ", ")))
	}

	filter := strings.ToLower(strings.TrimSpace(p.Name))
	var items []PackageRow
	for i := range pkgs {
		obj := &pkgs[i].Object
		pkgRef := nestedStr(obj, "spec", "package")
		// The name filter excludes before counting (the kind-filter precedent in
		// BuildUnhealthy); it also matches the OCI ref because agents usually
		// know the provider's image/group, not the object name.
		if filter != "" &&
			!strings.Contains(strings.ToLower(obj.GetName()), filter) &&
			!strings.Contains(strings.ToLower(pkgRef), filter) {
			continue
		}

		conds := Conditions(obj)
		state := classifyAll(conds)
		res.Scanned++
		switch state {
		case StateBlocked:
			res.Summary.Blocked++
		case StatePending:
			res.Summary.Pending++
		default:
			res.Summary.Ready++
		}
		// Unlike list_unhealthy, the default lists everything: package counts
		// are small, the inventory is half the value, and "your suspect is
		// Ready" is itself an answer. UnhealthyOnly filters after counting.
		if p.UnhealthyOnly && state == StateReady {
			continue
		}

		row := PackageRow{
			APIVersion:      obj.GetAPIVersion(),
			Kind:            obj.GetKind(),
			Name:            obj.GetName(),
			Package:         pkgRef,
			State:           state,
			Installed:       byType(conds, TypeInstalled),
			Healthy:         byType(conds, TypeHealthy),
			Synced:          byType(conds, TypeSynced),
			Paused:          IsPaused(obj),
			CurrentRevision: nestedStr(obj, "status", "currentRevision"),
			Reasons:         blockingMessages(conds),
			uid:             string(obj.GetUID()),
		}
		if pol := nestedStr(obj, "spec", "revisionActivationPolicy"); pol == "Manual" {
			row.ActivationPolicy = pol
		}
		if id := nestedStr(obj, "status", "currentIdentifier"); id != "" && id != pkgRef {
			row.CurrentIdentifier = id
		}
		if rp := nestedStr(obj, "status", "resolvedPackage"); rp != "" && rp != pkgRef {
			row.ResolvedPackage = rp
		}

		// Lifecycle via the shared label logic; a package that never came up is
		// "Installing", not "Creating".
		ln := &Node{
			State:        state,
			Conditions:   conds,
			deletionTime: deletionTime(obj),
			creationTime: metaTimeString(obj.GetCreationTimestamp()),
			paused:       row.Paused,
		}
		row.Lifecycle = lifecycleLabelFor(ln, nowFn(), "Installing")
		if ln.deletionTime != "" {
			row.DeletionTimestamp = ln.deletionTime
			row.Finalizers = obj.GetFinalizers()
		}

		var rows []RevisionRow
		if p.RevisionsListed {
			rows = buildRevisionRows(revs, matched[i], pkgRef, row.CurrentRevision)
			count := len(rows)
			row.RevisionCount = &count
			if includeRevisions(state, rows) {
				row.Revisions, row.RevisionsTruncated = capRevisionRows(rows)
			}
		}
		row.Skew = skewSentences(obj, &row, rows, p.RevisionsListed)

		items = append(items, row)
	}

	sort.SliceStable(items, func(a, b int) bool {
		x, y := items[a], items[b]
		if x.State != y.State {
			return stateRank(x.State) < stateRank(y.State)
		}
		if x.Kind != y.Kind {
			return x.Kind < y.Kind
		}
		return x.Name < y.Name
	})

	if p.Limit > 0 && len(items) > p.Limit {
		items = items[:p.Limit]
		res.Truncated = true
	}

	// Full enrichment is reserved for the rows an agent acts on first. In a
	// mass failure (a registry outage breaks every provider at once) enriching
	// every row would blow the response to hundreds of KiB; beyond the first
	// maxDetailedPackages failing rows the row goes compact — identity,
	// statuses, lifecycle, and counts stay; reasons/skew/revisions are dropped
	// whole (never clipped) and the note names the drill-down path. Runs after
	// the Limit cut so the note counts rows actually shipped.
	detailed, compact := 0, 0
	for i := range items {
		if items[i].State == StateReady {
			continue
		}
		if detailed < maxDetailedPackages {
			detailed++
			continue
		}
		compact++
		items[i].Reasons = nil
		items[i].Skew = nil
		items[i].Revisions = nil
		items[i].RevisionsTruncated = false
	}
	if compact > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf(
			"full detail (reasons, skew, revisions) included for the first %d failing packages only; %d more failing rows are compact — re-call with the name filter for one package's full detail",
			maxDetailedPackages, compact))
	}

	res.Items = items
	return res
}

// classifyAll reduces a condition set to a state by folding over ALL
// conditions, not just the Ready/Synced/Healthy trio Classify reads. Packages
// express health through Installed/Healthy — and revisions through
// version-dependent types — so gating on a type list would re-create the very
// blind spot this fixes (Classify reports Installed:False as Ready). Every
// condition type ever shipped on pkg.crossplane.io objects is health-semantic,
// and a hypothetical future informational False still renders its verbatim
// reason line via blockingMessages.
func classifyAll(cs []Condition) string {
	state := StateReady
	sawTrue := false
	for _, c := range cs {
		switch c.Status {
		case "False":
			return StateBlocked
		case "Unknown":
			state = StatePending
		case "True":
			sawTrue = true
		}
	}
	// No conditions at all (just created, manager down, or wiped on unpause):
	// we can't assert readiness.
	if !sawTrue {
		return StatePending
	}
	return state
}

// parentIndex finds the parent package of a revision: primarily the
// pkg.crossplane.io/package label (the package manager's own lookup key),
// falling back to the controller ownerReference matched by UID, then by
// kind+name.
func parentIndex(rev *unstructured.Unstructured, pkgs []k8s.Listed, byName, byUID map[string]int) (int, bool) {
	if name := rev.GetLabels()[parentPackageLabel]; name != "" {
		if i, ok := byName[name]; ok {
			return i, true
		}
	}
	for _, ref := range rev.GetOwnerReferences() {
		if ref.Controller == nil || !*ref.Controller {
			continue
		}
		if i, ok := byUID[string(ref.UID)]; ok {
			return i, true
		}
		if i, ok := byName[ref.Name]; ok && pkgs[i].Object.GetKind() == ref.Kind {
			return i, true
		}
	}
	return 0, false
}

// buildRevisionRows projects the matched revision objects into rows, newest
// (highest ordinal) first — an upgrade question is about the failing new
// revision.
func buildRevisionRows(revs []k8s.Listed, idxs []int, parentPkg, currentRevision string) []RevisionRow {
	rows := make([]RevisionRow, 0, len(idxs))
	for _, j := range idxs {
		obj := &revs[j].Object
		conds := Conditions(obj)
		r := RevisionRow{
			Name:              obj.GetName(),
			Revision:          nestedI64(obj, "spec", "revision"),
			DesiredState:      nestedStr(obj, "spec", "desiredState"),
			Current:           currentRevision != "" && obj.GetName() == currentRevision,
			State:             classifyAll(conds),
			Healthy:           byType(conds, TypeHealthy),
			RevisionHealthy:   byType(conds, TypeRevisionHealthy),
			RuntimeHealthy:    byType(conds, TypeRuntimeHealthy),
			Verified:          byType(conds, TypeVerified),
			Paused:            IsPaused(obj),
			DeletionTimestamp: deletionTime(obj),
			Reasons:           blockingMessages(conds),
			uid:               string(obj.GetUID()),
		}
		if img := nestedStr(obj, "spec", "image"); img != "" && img != parentPkg {
			r.Image = img
		}
		found := nestedI64(obj, "status", "foundDependencies")
		installed := nestedI64(obj, "status", "installedDependencies")
		invalid := nestedI64(obj, "status", "invalidDependencies")
		if invalid > 0 || installed < found {
			r.Deps = &DepCounts{Found: found, Installed: installed, Invalid: invalid}
		}
		rows = append(rows, r)
	}
	sort.SliceStable(rows, func(a, b int) bool { return rows[a].Revision > rows[b].Revision })
	return rows
}

// includeRevisions decides whether a package's revision rows carry signal
// worth their tokens. The builder always examines all revisions (counts and
// skew need them); the rows render only for: a non-Ready package, a non-Ready
// revision (catches the documented one-reconcile window where a stale package
// Healthy=True hides a failing new revision), a paused/terminating revision,
// an Active count other than exactly one (0 = outage in progress, >1 =
// anomalous), or a current revision that is not the Active one (upgrade in
// flight or wedged).
func includeRevisions(pkgState string, rows []RevisionRow) bool {
	if len(rows) == 0 {
		return false
	}
	if pkgState != StateReady {
		return true
	}
	active := 0
	for _, r := range rows {
		if r.State != StateReady || r.Paused || r.DeletionTimestamp != "" {
			return true
		}
		if r.DesiredState == "Active" {
			active++
		}
		if r.Current && r.DesiredState != "Active" {
			return true
		}
	}
	return active != 1
}

// maxRevisionRows caps the revision rows rendered per package.
const maxRevisionRows = 5

// capRevisionRows keeps at most maxRevisionRows rows, prioritising the current
// revision, then Active ones, then non-Ready ones, then the newest. Whole rows
// are dropped and flagged — nothing is ever clipped.
func capRevisionRows(rows []RevisionRow) ([]RevisionRow, bool) {
	if len(rows) <= maxRevisionRows {
		return rows, false
	}
	prio := func(r RevisionRow) int {
		switch {
		case r.Current:
			return 0
		case r.DesiredState == "Active":
			return 1
		case r.State != StateReady:
			return 2
		default:
			return 3
		}
	}
	idxs := make([]int, len(rows))
	for i := range idxs {
		idxs[i] = i
	}
	// Stable on input order, which is newest-first — so equal priorities keep
	// the newest.
	sort.SliceStable(idxs, func(a, b int) bool { return prio(rows[idxs[a]]) < prio(rows[idxs[b]]) })
	keep := map[int]bool{}
	for _, i := range idxs[:maxRevisionRows] {
		keep[i] = true
	}
	out := make([]RevisionRow, 0, maxRevisionRows)
	for i := range rows {
		if keep[i] {
			out = append(out, rows[i])
		}
	}
	return out, true
}

// skewSentences derives upgrade/rollout anomalies as full sentences from
// stable spec/status fields only — never from reason strings or messages,
// whose wording drifts across Crossplane versions. Sentences needing revision
// data are evaluated only when revisions were actually listed.
func skewSentences(obj *unstructured.Unstructured, row *PackageRow, rows []RevisionRow, revisionsListed bool) []string {
	var skew []string
	// CurrentIdentifier is set only when it differs from spec.package: the
	// edit was never successfully unpacked. A first install (no identifier
	// yet) emits nothing; an ImageConfig rewrite is ResolvedPackage, not skew.
	if row.CurrentIdentifier != "" {
		skew = append(skew, fmt.Sprintf(
			"upgrade not unpacked: spec.package is %s but the last successfully unpacked source is %s — see the Installed condition message and UnpackPackage events",
			row.Package, row.CurrentIdentifier))
	}
	if !revisionsListed {
		return skew
	}

	active := 0
	activeName := ""
	var current *RevisionRow
	for i := range rows {
		if rows[i].DesiredState == "Active" {
			active++
			activeName = rows[i].Name
		}
		if rows[i].Current {
			current = &rows[i]
		}
	}

	if row.ActivationPolicy == "Manual" && current != nil && current.DesiredState == "Inactive" {
		s := fmt.Sprintf("awaiting manual approval: current revision %s is Inactive (revisionActivationPolicy: Manual)", current.Name)
		if active == 0 {
			// The manager deactivates old revisions on a package change even
			// under Manual policy, so this is a real outage, not a transition.
			s += " — no revision is active, the package is not serving"
		}
		skew = append(skew, s)
	}
	if current != nil && (current.State != StateReady || current.DesiredState != "Active") &&
		active > 0 && activeName != current.Name {
		st := current.State
		if current.DesiredState != "Active" {
			st = "Inactive"
		}
		skew = append(skew, fmt.Sprintf(
			"upgrade wedged or in flight: current revision %s is %s; older revision %s is still Active",
			current.Name, st, activeName))
	}
	if active > 1 {
		skew = append(skew, fmt.Sprintf("anomalous: %d revisions are Active simultaneously", active))
	}
	limit, hasLimit := nestedI64OK(obj, "spec", "revisionHistoryLimit")
	if !hasLimit {
		limit = 1 // the API default
	}
	// limit 0 disables GC on purpose — piling up revisions is then expected.
	if limit > 0 && int64(len(rows)) > limit+1 {
		skew = append(skew, fmt.Sprintf(
			"revision garbage collection lagging: %d revisions exceed revisionHistoryLimit+1 — check GarbageCollect warning events",
			len(rows)))
	}
	// The manager-documented one-reconcile lag: package health mirrors the OLD
	// revision until the next reconcile after a new revision appears.
	if row.Healthy == "True" && current != nil && current.State != StateReady {
		skew = append(skew, fmt.Sprintf(
			"package health may lag: current revision %s is %s while the package still reports Healthy=True",
			current.Name, current.State))
	}
	return skew
}

// maxDetailedPackages caps how many failing packages get the full treatment —
// enrichment (reasons/skew/revisions) in BuildPackages and event decoration in
// DecoratePackageEvents — mirroring diagnose's maxSuspects. Beyond it, rows go
// compact and the agent drills down per package via the name filter.
const maxDetailedPackages = 10

// DecoratePackageEvents fetches and attaches events to the first
// maxDetailedPackages rows with a failing side: a non-Ready package gets its
// own events, and every rendered non-Ready revision row gets its events even
// when the parent package still reports Ready — the health-lag window
// (trigger 2 renders the rows precisely then) must not ship a failing
// revision blind. Both objects matter: package events carry the
// unpack/transition/GC story (UnpackPackage holds the verbatim registry
// error), revision events the parse/lint/dependency story — and recurrence
// counts live only in events. Fetches are uncapped like diagnose's (the
// fetcher lists everything regardless; a cap would only trim the slice and
// could evict the recurring event trimEvents must preserve), then trimmed for
// output. Cluster-scoped packages pass an empty namespace — the fetcher
// queries "default", which is where the event recorder files events for
// cluster-scoped objects. A healthy result costs zero event calls; the first
// fetch error stops all further fetches and is reported once in the returned
// notes.
func DecoratePackageEvents(ctx context.Context, ef EventFetcher, items []PackageRow) []string {
	if ef == nil {
		return nil
	}
	decorated := 0
	for i := range items {
		if !needsEvents(&items[i]) {
			continue
		}
		if decorated >= maxDetailedPackages {
			return []string{fmt.Sprintf("events fetched for the first %d failing packages only", maxDetailedPackages)}
		}
		decorated++
		// The package object's own events only when the package itself is
		// failing — a Ready package's events are just Normal install history.
		if items[i].State != StateReady {
			ev, err := ef.Events(ctx, "", items[i].uid, allEvents)
			if err != nil {
				return []string{"package events unavailable: " + err.Error()}
			}
			items[i].Events = trimEvents(ev)
		}
		for j := range items[i].Revisions {
			r := &items[i].Revisions[j]
			if r.State == StateReady {
				continue
			}
			rev, err := ef.Events(ctx, "", r.uid, allEvents)
			if err != nil {
				return []string{"package events unavailable: " + err.Error()}
			}
			r.Events = trimEvents(rev)
		}
	}
	return nil
}

// needsEvents reports whether a row has a failing side worth an event fetch:
// the package itself, or any rendered (already signal-bearing) revision.
func needsEvents(row *PackageRow) bool {
	if row.State != StateReady {
		return true
	}
	for _, r := range row.Revisions {
		if r.State != StateReady {
			return true
		}
	}
	return false
}

func nestedStr(obj *unstructured.Unstructured, fields ...string) string {
	s, _, _ := unstructured.NestedString(obj.Object, fields...)
	return s
}

func nestedI64(obj *unstructured.Unstructured, fields ...string) int64 {
	v, _, _ := unstructured.NestedInt64(obj.Object, fields...)
	return v
}

func nestedI64OK(obj *unstructured.Unstructured, fields ...string) (int64, bool) {
	v, ok, err := unstructured.NestedInt64(obj.Object, fields...)
	return v, ok && err == nil
}
