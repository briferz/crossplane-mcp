package xp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/briferz/crossplane-mcp/internal/k8s"
)

// pkgObj builds a package/revision fixture. spec/status entries are merged
// into the object; conds (if any) land at status.conditions.
func pkgObj(kind, name, uid string, spec, status map[string]any, conds ...map[string]any) k8s.Listed {
	meta := map[string]any{"name": name}
	if uid != "" {
		meta["uid"] = uid
	}
	o := map[string]any{"apiVersion": "pkg.crossplane.io/v1", "kind": kind, "metadata": meta}
	if spec != nil {
		o["spec"] = spec
	}
	st := map[string]any{}
	for k, v := range status {
		st[k] = v
	}
	if conds != nil {
		cs := make([]any, len(conds))
		for i, c := range conds {
			cs[i] = c
		}
		st["conditions"] = cs
	}
	if len(st) > 0 {
		o["status"] = st
	}
	return k8s.Listed{Category: "pkg", Object: unstructured.Unstructured{Object: o}}
}

// revObj builds a revision carrying the parent-package label, the default
// correlation path.
func revObj(kind, name, uid, parent string, revision int64, desiredState string, conds ...map[string]any) k8s.Listed {
	l := pkgObj(kind, name, uid, map[string]any{"revision": revision, "desiredState": desiredState}, nil, conds...)
	l.Category = "pkgrev"
	l.Object.SetLabels(map[string]string{"pkg.crossplane.io/package": parent})
	return l
}

func cndR(typ, status, reason, message string) map[string]any {
	return map[string]any{"type": typ, "status": status, "reason": reason, "message": message}
}

func pinClock(t *testing.T, at time.Time) {
	t.Helper()
	orig := nowFn
	nowFn = func() time.Time { return at }
	t.Cleanup(func() { nowFn = orig })
}

// TestClassifyAll pins the package classifier, including the headline
// regression: Classify (Ready/Synced/Healthy only) reports Installed:False +
// Healthy:True as Ready — classifyAll must not.
func TestClassifyAll(t *testing.T) {
	cases := []struct {
		name  string
		conds []Condition
		want  string
	}{
		{"installed false beats healthy true", []Condition{
			{Type: TypeInstalled, Status: "False"}, {Type: TypeHealthy, Status: "True"}}, StateBlocked},
		{"unpacking package", []Condition{{Type: TypeInstalled, Status: "False"}}, StateBlocked},
		{"healthy unknown", []Condition{
			{Type: TypeInstalled, Status: "True"}, {Type: TypeHealthy, Status: "Unknown"}}, StatePending},
		{"no conditions", nil, StatePending},
		{"paused synced false", []Condition{{Type: TypeSynced, Status: "False"}}, StateBlocked},
		{"steady state", []Condition{
			{Type: TypeInstalled, Status: "True"}, {Type: TypeHealthy, Status: "True"}}, StateReady},
		{"2.x crashloop: runtime false", []Condition{
			{Type: TypeRevisionHealthy, Status: "True"}, {Type: TypeRuntimeHealthy, Status: "False"}}, StateBlocked},
		{"configuration revision: contents only", []Condition{
			{Type: TypeRevisionHealthy, Status: "True"}}, StateReady},
		{"1.x revision healthy false", []Condition{{Type: TypeHealthy, Status: "False"}}, StateBlocked},
		{"verified false", []Condition{
			{Type: TypeRevisionHealthy, Status: "True"}, {Type: TypeVerified, Status: "False"}}, StateBlocked},
	}
	for _, c := range cases {
		if got := classifyAll(c.conds); got != c.want {
			t.Errorf("%s: classifyAll = %s, want %s", c.name, got, c.want)
		}
	}
	// Contrast pin: today's Classify really does mis-read the same condition
	// set — the reason classifyAll exists. If Classify ever learns Installed,
	// revisit whether classifyAll is still needed.
	if _, got := Classify([]Condition{{Type: TypeInstalled, Status: "False"}, {Type: TypeHealthy, Status: "True"}}); got != StateReady {
		t.Errorf("expected Classify to (wrongly) report Ready for Installed:False, got %s — classifyAll may be redundant now", got)
	}
}

// TestBuildPackagesHealthyRow asserts the steady-state row is tiny: identity +
// statuses + currentRevision + revisionCount, nothing else.
func TestBuildPackagesHealthyRow(t *testing.T) {
	pkgs := []k8s.Listed{pkgObj("Provider", "provider-aws", "u1",
		map[string]any{"package": "xpkg.example.org/provider-aws:v1"},
		map[string]any{"currentRevision": "provider-aws-1234", "currentIdentifier": "xpkg.example.org/provider-aws:v1"},
		cndR(TypeInstalled, "True", "ActivePackageRevision", ""),
		cndR(TypeHealthy, "True", "HealthyPackageRevision", ""),
	)}
	revs := []k8s.Listed{revObj("ProviderRevision", "provider-aws-1234", "r1", "provider-aws", 1, "Active",
		cndR(TypeRevisionHealthy, "True", "HealthyPackageRevision", ""),
		cndR(TypeRuntimeHealthy, "True", "HealthyPackageRevision", ""),
	)}

	r := BuildPackages(pkgs, revs, PackagesParams{RevisionsListed: true})
	if r.Scanned != 1 || r.Summary.Ready != 1 || len(r.Items) != 1 {
		t.Fatalf("expected one Ready item, got %+v", r)
	}
	row := r.Items[0]
	if row.State != StateReady || row.Installed != "True" || row.Healthy != "True" {
		t.Errorf("unexpected statuses: %+v", row)
	}
	if row.CurrentRevision != "provider-aws-1234" {
		t.Errorf("currentRevision = %q", row.CurrentRevision)
	}
	if row.RevisionCount == nil || *row.RevisionCount != 1 {
		t.Errorf("revisionCount = %v, want 1", row.RevisionCount)
	}
	// Equal identifier must be omitted; no signal → no enrichment.
	if row.CurrentIdentifier != "" || row.ResolvedPackage != "" || row.ActivationPolicy != "" {
		t.Errorf("identifier/policy fields should be omitted on steady state: %+v", row)
	}
	if row.Revisions != nil || len(row.Skew) != 0 || len(row.Reasons) != 0 || row.Lifecycle != "" {
		t.Errorf("healthy row must carry no enrichment, got %+v", row)
	}
}

// TestBuildPackagesBlockedUnpack covers failure mode #1 (image pull / unpack):
// Installed=False with the full registry error preserved untruncated, skew
// sentence 1 from the identifier mismatch, and revisions rendered.
func TestBuildPackagesBlockedUnpack(t *testing.T) {
	longErr := "cannot unpack package: GET https://registry.example.org/v2/provider-aws/manifests/v2: " +
		"UNAUTHORIZED: authentication required " + strings.Repeat("x", 4096)
	pkgs := []k8s.Listed{pkgObj("Provider", "provider-aws", "u1",
		map[string]any{"package": "xpkg.example.org/provider-aws:v2"},
		map[string]any{"currentRevision": "provider-aws-old", "currentIdentifier": "xpkg.example.org/provider-aws:v1"},
		cndR(TypeInstalled, "False", "UnpackingPackage", longErr),
		cndR(TypeHealthy, "False", "UnhealthyPackageRevision", longErr),
	)}
	revs := []k8s.Listed{revObj("ProviderRevision", "provider-aws-old", "r1", "provider-aws", 1, "Active",
		cndR(TypeRevisionHealthy, "True", "HealthyPackageRevision", ""),
		cndR(TypeRuntimeHealthy, "True", "HealthyPackageRevision", ""),
	)}

	r := BuildPackages(pkgs, revs, PackagesParams{RevisionsListed: true})
	row := r.Items[0]
	if row.State != StateBlocked {
		t.Fatalf("state = %s, want Blocked", row.State)
	}
	if row.CurrentIdentifier != "xpkg.example.org/provider-aws:v1" {
		t.Errorf("currentIdentifier should surface the mismatch, got %q", row.CurrentIdentifier)
	}
	var found bool
	for _, m := range row.Reasons {
		if strings.Contains(m, longErr) {
			found = true
		}
	}
	if !found {
		t.Error("the full unpack error must be preserved untruncated in reasons")
	}
	if len(row.Skew) == 0 || !strings.Contains(row.Skew[0], "upgrade not unpacked") ||
		!strings.Contains(row.Skew[0], "provider-aws:v2") || !strings.Contains(row.Skew[0], "provider-aws:v1") {
		t.Errorf("expected the upgrade-not-unpacked skew sentence naming both refs, got %v", row.Skew)
	}
	if len(row.Revisions) != 1 {
		t.Errorf("a blocked package should render its revisions, got %+v", row.Revisions)
	}
}

// TestBuildPackagesManualApproval covers the approval trap: Installed=False,
// current revision Inactive, no Active revision at all.
func TestBuildPackagesManualApproval(t *testing.T) {
	pkgs := []k8s.Listed{pkgObj("Provider", "provider-gcp", "u1",
		map[string]any{"package": "xpkg.example.org/provider-gcp:v2", "revisionActivationPolicy": "Manual"},
		map[string]any{"currentRevision": "provider-gcp-new"},
		cndR(TypeInstalled, "False", "InactivePackageRevision", "Package is inactive"),
		cndR(TypeHealthy, "True", "HealthyPackageRevision", ""),
	)}
	revs := []k8s.Listed{
		revObj("ProviderRevision", "provider-gcp-new", "r2", "provider-gcp", 2, "Inactive",
			cndR(TypeRevisionHealthy, "True", "HealthyPackageRevision", "")),
		revObj("ProviderRevision", "provider-gcp-old", "r1", "provider-gcp", 1, "Inactive",
			cndR(TypeRevisionHealthy, "True", "HealthyPackageRevision", "")),
	}

	r := BuildPackages(pkgs, revs, PackagesParams{RevisionsListed: true})
	row := r.Items[0]
	if row.State != StateBlocked {
		t.Fatalf("Installed=False must classify Blocked, got %s", row.State)
	}
	if row.ActivationPolicy != "Manual" {
		t.Errorf("Manual policy should surface, got %q", row.ActivationPolicy)
	}
	var approval string
	for _, s := range row.Skew {
		if strings.Contains(s, "awaiting manual approval") {
			approval = s
		}
	}
	if approval == "" {
		t.Fatalf("expected the awaiting-manual-approval skew sentence, got %v", row.Skew)
	}
	if !strings.Contains(approval, "provider-gcp-new") || !strings.Contains(approval, "no revision is active") {
		t.Errorf("approval sentence should name the revision and the outage: %q", approval)
	}
	if len(row.Revisions) != 2 {
		t.Errorf("expected both revisions rendered, got %d", len(row.Revisions))
	}
	if !row.Revisions[0].Current || row.Revisions[0].Name != "provider-gcp-new" {
		t.Errorf("newest-first ordering with the current flagged, got %+v", row.Revisions[0])
	}
}

// TestBuildPackagesHealthLag covers the manager-documented one-reconcile lag:
// package still Healthy=True while the current (new) revision is failing —
// the revisions must render and the lag must be named.
func TestBuildPackagesHealthLag(t *testing.T) {
	pkgs := []k8s.Listed{pkgObj("Provider", "provider-aws", "u1",
		map[string]any{"package": "xpkg.example.org/provider-aws:v2"},
		map[string]any{"currentRevision": "provider-aws-new", "currentIdentifier": "xpkg.example.org/provider-aws:v2"},
		cndR(TypeInstalled, "True", "ActivePackageRevision", ""),
		cndR(TypeHealthy, "True", "HealthyPackageRevision", ""),
	)}
	revs := []k8s.Listed{
		revObj("ProviderRevision", "provider-aws-new", "r2", "provider-aws", 2, "Active",
			cndR(TypeRevisionHealthy, "True", "HealthyPackageRevision", ""),
			cndR(TypeRuntimeHealthy, "False", "UnhealthyPackageRevision",
				"post establish runtime hook failed for package: provider package deployment is unavailable with message: Deployment does not have minimum availability.")),
		revObj("ProviderRevision", "provider-aws-old", "r1", "provider-aws", 1, "Inactive",
			cndR(TypeRevisionHealthy, "True", "HealthyPackageRevision", "")),
	}

	r := BuildPackages(pkgs, revs, PackagesParams{RevisionsListed: true})
	row := r.Items[0]
	if row.State != StateReady {
		t.Fatalf("API-reported package state must not be overridden, got %s", row.State)
	}
	if len(row.Revisions) == 0 {
		t.Fatal("a failing current revision must force the revision rows even on a Healthy=True package")
	}
	if row.Revisions[0].RuntimeHealthy != "False" || row.Revisions[0].State != StateBlocked {
		t.Errorf("crashloop revision should be Blocked with runtimeHealthy False, got %+v", row.Revisions[0])
	}
	var lag bool
	for _, s := range row.Skew {
		if strings.Contains(s, "package health may lag") && strings.Contains(s, "provider-aws-new") {
			lag = true
		}
	}
	if !lag {
		t.Errorf("expected the health-lag skew sentence, got %v", row.Skew)
	}
}

// TestBuildPackagesWedgedUpgrade: current revision blocked while the old one
// still serves.
func TestBuildPackagesWedgedUpgrade(t *testing.T) {
	pkgs := []k8s.Listed{pkgObj("Provider", "provider-sql", "u1",
		map[string]any{"package": "xpkg.example.org/provider-sql:v3"},
		map[string]any{"currentRevision": "provider-sql-new"},
		cndR(TypeInstalled, "True", "ActivePackageRevision", ""),
		cndR(TypeHealthy, "False", "UnhealthyPackageRevision",
			`Package revision health is "False" with message: incompatible Crossplane version: package is not compatible with Crossplane version (v1.16.0)`),
	)}
	revs := []k8s.Listed{
		revObj("ProviderRevision", "provider-sql-new", "r2", "provider-sql", 2, "Inactive",
			cndR(TypeRevisionHealthy, "False", "UnhealthyPackageRevision", "incompatible Crossplane version: ...")),
		revObj("ProviderRevision", "provider-sql-old", "r1", "provider-sql", 1, "Active",
			cndR(TypeRevisionHealthy, "True", "HealthyPackageRevision", "")),
	}

	r := BuildPackages(pkgs, revs, PackagesParams{RevisionsListed: true})
	row := r.Items[0]
	var wedged bool
	for _, s := range row.Skew {
		if strings.Contains(s, "upgrade wedged or in flight") &&
			strings.Contains(s, "provider-sql-new") && strings.Contains(s, "provider-sql-old") {
			wedged = true
		}
	}
	if !wedged {
		t.Errorf("expected the wedged-upgrade skew sentence naming both revisions, got %v", row.Skew)
	}
}

// TestBuildPackagesSkewAnomalies covers multi-Active and GC lag.
func TestBuildPackagesSkewAnomalies(t *testing.T) {
	mk := func(historyLimit any, revCount int, actives int) PackageRow {
		t.Helper()
		spec := map[string]any{"package": "xpkg.example.org/p:v1"}
		if historyLimit != nil {
			spec["revisionHistoryLimit"] = historyLimit
		}
		pkgs := []k8s.Listed{pkgObj("Provider", "p", "u1", spec,
			map[string]any{"currentRevision": fmt.Sprintf("p-%d", revCount)},
			cndR(TypeInstalled, "True", "", ""), cndR(TypeHealthy, "True", "", ""))}
		var revs []k8s.Listed
		for i := 1; i <= revCount; i++ {
			state := "Inactive"
			if i > revCount-actives {
				state = "Active"
			}
			revs = append(revs, revObj("ProviderRevision", fmt.Sprintf("p-%d", i), fmt.Sprintf("r%d", i), "p", int64(i), state,
				cndR(TypeRevisionHealthy, "True", "", "")))
		}
		r := BuildPackages(pkgs, revs, PackagesParams{RevisionsListed: true})
		return r.Items[0]
	}

	if row := mk(nil, 2, 2); !containsSub(row.Skew, "2 revisions are Active simultaneously") {
		t.Errorf("expected the multi-Active anomaly, got %v", row.Skew)
	}
	// Default history limit 1 → more than 2 revisions means GC lags.
	if row := mk(nil, 4, 1); !containsSub(row.Skew, "garbage collection lagging") {
		t.Errorf("expected the GC-lag sentence with the defaulted limit, got %v", row.Skew)
	}
	// Limit 0 disables GC on purpose: no sentence however many pile up.
	if row := mk(int64(0), 6, 1); containsSub(row.Skew, "garbage collection lagging") {
		t.Errorf("limit 0 must suppress the GC sentence, got %v", row.Skew)
	}
}

func containsSub(haystack []string, sub string) bool {
	for _, s := range haystack {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// TestBuildPackagesRevisionCap: the cap keeps current + Active + non-Ready
// first, drops whole healthy old rows, and says so.
func TestBuildPackagesRevisionCap(t *testing.T) {
	pkgs := []k8s.Listed{pkgObj("Provider", "p", "u1",
		map[string]any{"package": "xpkg.example.org/p:v9", "revisionHistoryLimit": int64(0)},
		map[string]any{"currentRevision": "p-9"},
		cndR(TypeInstalled, "False", "UnpackingPackage", "boom"),
	)}
	var revs []k8s.Listed
	for i := 1; i <= 9; i++ {
		state, cond := "Inactive", cndR(TypeRevisionHealthy, "True", "", "")
		switch i {
		case 1:
			cond = cndR(TypeRevisionHealthy, "False", "UnhealthyPackageRevision", "old failure") // non-Ready, oldest
		case 8:
			state = "Active"
		}
		revs = append(revs, revObj("ProviderRevision", fmt.Sprintf("p-%d", i), fmt.Sprintf("r%d", i), "p", int64(i), state, cond))
	}

	r := BuildPackages(pkgs, revs, PackagesParams{RevisionsListed: true})
	row := r.Items[0]
	if row.RevisionCount == nil || *row.RevisionCount != 9 {
		t.Fatalf("pre-cap revisionCount must stay honest, got %v", row.RevisionCount)
	}
	if len(row.Revisions) != 5 || !row.RevisionsTruncated {
		t.Fatalf("expected 5 capped rows with revisionsTruncated, got %d (truncated=%v)", len(row.Revisions), row.RevisionsTruncated)
	}
	names := map[string]bool{}
	for _, rr := range row.Revisions {
		names[rr.Name] = true
	}
	for _, must := range []string{"p-9", "p-8", "p-1"} { // current, Active, non-Ready
		if !names[must] {
			t.Errorf("cap must keep %s (current/Active/non-Ready), kept %v", must, names)
		}
	}
}

// TestBuildPackagesRBACGuard: revisions unlistable → count nil, no rows, and
// every revision-derived skew sentence suppressed (only the identifier
// sentence may fire).
func TestBuildPackagesRBACGuard(t *testing.T) {
	pkgs := []k8s.Listed{pkgObj("Provider", "p", "u1",
		map[string]any{"package": "xpkg.example.org/p:v2", "revisionActivationPolicy": "Manual"},
		map[string]any{"currentRevision": "p-2", "currentIdentifier": "xpkg.example.org/p:v1"},
		cndR(TypeInstalled, "False", "InactivePackageRevision", "Package is inactive"),
	)}

	r := BuildPackages(pkgs, nil, PackagesParams{RevisionsListed: false})
	row := r.Items[0]
	if row.RevisionCount != nil {
		t.Errorf("revisionCount must be nil when revisions were not listed, got %v", *row.RevisionCount)
	}
	if row.Revisions != nil {
		t.Errorf("no revision rows without revision data, got %+v", row.Revisions)
	}
	if len(row.Skew) != 1 || !strings.Contains(row.Skew[0], "upgrade not unpacked") {
		t.Errorf("only the identifier skew sentence may fire without revision data, got %v", row.Skew)
	}
}

// TestBuildPackagesZeroRevisions: a package whose unpack never produced a
// revision renders revisionCount 0 — meaningfully different from nil.
func TestBuildPackagesZeroRevisions(t *testing.T) {
	pkgs := []k8s.Listed{pkgObj("Provider", "p", "u1",
		map[string]any{"package": "xpkg.example.org/p:v1"}, nil,
		cndR(TypeInstalled, "False", "UnpackingPackage", "cannot unpack package: ..."),
	)}
	r := BuildPackages(pkgs, nil, PackagesParams{RevisionsListed: true})
	row := r.Items[0]
	if row.RevisionCount == nil || *row.RevisionCount != 0 {
		t.Errorf("revisionCount should be 0 (not nil) when listing worked, got %v", row.RevisionCount)
	}
	if row.CurrentRevision != "" || row.Revisions != nil {
		t.Errorf("no revision artifacts expected, got %+v", row)
	}
}

// TestBuildPackagesCorrelation exercises the ownerRef fallbacks and the orphan
// note.
func TestBuildPackagesCorrelation(t *testing.T) {
	pkgs := []k8s.Listed{pkgObj("Provider", "p", "pkg-uid",
		map[string]any{"package": "xpkg.example.org/p:v1"}, nil,
		cndR(TypeInstalled, "False", "UnpackingPackage", "x"),
	)}

	ctrl := true
	byUID := revObj("ProviderRevision", "p-by-uid", "r1", "", 1, "Active", cndR(TypeRevisionHealthy, "True", "", ""))
	byUID.Object.SetLabels(nil)
	byUID.Object.SetOwnerReferences([]metav1.OwnerReference{{Kind: "Provider", Name: "renamed", UID: "pkg-uid", Controller: &ctrl}})
	byName := revObj("ProviderRevision", "p-by-name", "r2", "", 2, "Inactive", cndR(TypeRevisionHealthy, "True", "", ""))
	byName.Object.SetLabels(nil)
	byName.Object.SetOwnerReferences([]metav1.OwnerReference{{Kind: "Provider", Name: "p", Controller: &ctrl}})
	orphan := revObj("ProviderRevision", "stray", "r3", "", 3, "Inactive", cndR(TypeRevisionHealthy, "True", "", ""))
	orphan.Object.SetLabels(nil)

	r := BuildPackages(pkgs, []k8s.Listed{byUID, byName, orphan}, PackagesParams{RevisionsListed: true})
	row := r.Items[0]
	if row.RevisionCount == nil || *row.RevisionCount != 2 {
		t.Errorf("both ownerRef fallback paths should correlate, got count %v", row.RevisionCount)
	}
	if !containsSub(r.Notes, "stray") || !containsSub(r.Notes, "matched no listed package") {
		t.Errorf("the orphan must be noted, got %v", r.Notes)
	}
	for _, rr := range row.Revisions {
		if rr.Name == "stray" {
			t.Error("an orphan revision must never become a row")
		}
	}
}

// TestBuildPackagesFiltersAndCounting pins the counting semantics: the name
// filter excludes from Scanned/Summary; unhealthyOnly filters after counting;
// the cap keeps pre-cap totals honest.
func TestBuildPackagesFiltersAndCounting(t *testing.T) {
	pkgs := []k8s.Listed{
		pkgObj("Provider", "provider-aws-s3", "u1",
			map[string]any{"package": "xpkg.example.org/upbound/provider-aws-s3:v1"}, nil,
			cndR(TypeInstalled, "True", "", ""), cndR(TypeHealthy, "True", "", "")),
		pkgObj("Provider", "provider-gcp", "u2",
			map[string]any{"package": "xpkg.example.org/upbound/provider-gcp:v1"}, nil,
			cndR(TypeInstalled, "False", "UnpackingPackage", "x")),
		pkgObj("Provider", "provider-azure", "u3",
			map[string]any{"package": "xpkg.example.org/upbound/provider-azure:v1"}, nil),
	}

	// Substring matches the OCI ref, not just the object name.
	r := BuildPackages(pkgs, nil, PackagesParams{Name: "AWS-S3", RevisionsListed: true})
	if r.Scanned != 1 || len(r.Items) != 1 || r.Items[0].Name != "provider-aws-s3" {
		t.Errorf("name filter should exclude from scanned and match the ref case-insensitively, got %+v", r)
	}

	r = BuildPackages(pkgs, nil, PackagesParams{UnhealthyOnly: true, RevisionsListed: true})
	if r.Scanned != 3 || r.Summary.Ready != 1 || r.Summary.Blocked != 1 || r.Summary.Pending != 1 {
		t.Errorf("unhealthyOnly must count everything first, got scanned=%d summary=%+v", r.Scanned, r.Summary)
	}
	if len(r.Items) != 2 {
		t.Errorf("unhealthyOnly should drop the Ready row, got %d items", len(r.Items))
	}
	// Blocked sorts before Pending.
	if r.Items[0].State != StateBlocked || r.Items[1].State != StatePending {
		t.Errorf("ordering wrong: %s then %s", r.Items[0].State, r.Items[1].State)
	}

	r = BuildPackages(pkgs, nil, PackagesParams{Limit: 1, RevisionsListed: true})
	if len(r.Items) != 1 || !r.Truncated || r.Scanned != 3 {
		t.Errorf("cap must keep honest totals: items=%d truncated=%v scanned=%d", len(r.Items), r.Truncated, r.Scanned)
	}
}

// TestBuildPackagesPausedAndTerminating: pause and teardown get the same
// treatment packages' tree cousins get — explicit labels, never silence.
func TestBuildPackagesPausedAndTerminating(t *testing.T) {
	pinClock(t, time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC))

	paused := pkgObj("Provider", "frozen", "u1",
		map[string]any{"package": "xpkg.example.org/frozen:v1"}, nil,
		cndR(TypeSynced, "False", "ReconcilePaused", "Reconciliation is paused via the pause annotation"),
	)
	paused.Object.SetAnnotations(map[string]string{"crossplane.io/paused": "true"})
	_ = unstructured.SetNestedField(paused.Object.Object, "2026-06-06T00:00:00Z", "metadata", "creationTimestamp")

	wedged := pkgObj("Provider", "wedged", "u2",
		map[string]any{"package": "xpkg.example.org/wedged:v1"}, nil,
		cndR(TypeInstalled, "True", "", ""), cndR(TypeHealthy, "True", "", ""),
	)
	wedged.Object.SetFinalizers([]string{"pkg.crossplane.io"})
	_ = unstructured.SetNestedField(wedged.Object.Object, "2026-01-22T00:00:00Z", "metadata", "deletionTimestamp")

	r := BuildPackages([]k8s.Listed{paused, wedged}, nil, PackagesParams{RevisionsListed: true})
	for _, row := range r.Items {
		switch row.Name {
		case "frozen":
			if !row.Paused || row.State != StateBlocked || row.Synced != "False" {
				t.Errorf("paused package should be Blocked with paused+synced surfaced, got %+v", row)
			}
			if row.Lifecycle != "Paused (blocked, 5d)" {
				t.Errorf("lifecycle = %q, want Paused (blocked, 5d)", row.Lifecycle)
			}
		case "wedged":
			if row.DeletionTimestamp == "" || len(row.Finalizers) != 1 {
				t.Errorf("terminating package should carry deletion fields, got %+v", row)
			}
			if row.Lifecycle != "Terminating (stuck 140d)" {
				t.Errorf("lifecycle = %q, want Terminating (stuck 140d)", row.Lifecycle)
			}
		}
	}
}

// TestBuildPackagesInstallingLifecycle: a package that never came up reads
// "Installing", not "Creating".
func TestBuildPackagesInstallingLifecycle(t *testing.T) {
	pinClock(t, time.Date(2026, 6, 11, 3, 0, 0, 0, time.UTC))
	pkgs := []k8s.Listed{pkgObj("Provider", "p", "u1",
		map[string]any{"package": "xpkg.example.org/p:v1"}, nil,
		cndR(TypeInstalled, "False", "UnpackingPackage", "cannot unpack package: x"),
	)}
	pkgs[0].Object.Object["status"].(map[string]any)["conditions"].([]any)[0].(map[string]any)["lastTransitionTime"] = "2026-06-11T00:00:00Z"

	r := BuildPackages(pkgs, nil, PackagesParams{RevisionsListed: true})
	if got := r.Items[0].Lifecycle; got != "Installing (blocked, 3h)" {
		t.Errorf("lifecycle = %q, want Installing (blocked, 3h)", got)
	}
}

// TestRevisionRowDetails: image only when it differs, deps only when off.
func TestRevisionRowDetails(t *testing.T) {
	pkgs := []k8s.Listed{pkgObj("Provider", "p", "u1",
		map[string]any{"package": "xpkg.example.org/p:v2"},
		map[string]any{"currentRevision": "p-2"},
		cndR(TypeInstalled, "False", "UnpackingPackage", "x"),
	)}
	newRev := revObj("ProviderRevision", "p-2", "r2", "p", 2, "Active",
		cndR(TypeRevisionHealthy, "False", "UnhealthyPackageRevision", "cannot resolve package dependencies: boom"))
	_ = unstructured.SetNestedField(newRev.Object.Object, "xpkg.example.org/p:v2", "spec", "image")
	_ = unstructured.SetNestedField(newRev.Object.Object, int64(3), "status", "foundDependencies")
	_ = unstructured.SetNestedField(newRev.Object.Object, int64(1), "status", "installedDependencies")
	_ = unstructured.SetNestedField(newRev.Object.Object, int64(1), "status", "invalidDependencies")
	oldRev := revObj("ProviderRevision", "p-1", "r1", "p", 1, "Inactive",
		cndR(TypeRevisionHealthy, "True", "", ""))
	_ = unstructured.SetNestedField(oldRev.Object.Object, "xpkg.example.org/p:v1", "spec", "image")

	r := BuildPackages(pkgs, []k8s.Listed{newRev, oldRev}, PackagesParams{RevisionsListed: true})
	rows := r.Items[0].Revisions
	if len(rows) != 2 {
		t.Fatalf("expected 2 revision rows, got %d", len(rows))
	}
	if rows[0].Image != "" {
		t.Errorf("image equal to the parent's package must be omitted, got %q", rows[0].Image)
	}
	if rows[0].Deps == nil || rows[0].Deps.Invalid != 1 || rows[0].Deps.Found != 3 {
		t.Errorf("off dependency tallies should surface, got %+v", rows[0].Deps)
	}
	if rows[1].Image != "xpkg.example.org/p:v1" {
		t.Errorf("a differing old image disambiguates and must render, got %q", rows[1].Image)
	}
	if rows[1].Deps != nil {
		t.Errorf("healthy dep tallies must be omitted, got %+v", rows[1].Deps)
	}
}

// uidEvents is an EventFetcher keyed by uid, recording what it was asked.
type uidEvents struct {
	asked  []string
	events map[string][]k8s.Event
	err    error
}

func (s *uidEvents) Events(_ context.Context, _, uid string, _ int) ([]k8s.Event, error) {
	s.asked = append(s.asked, uid)
	if s.err != nil {
		return nil, s.err
	}
	return s.events[uid], nil
}

// TestDecoratePackageEvents pins the policy: failing sides only — a non-Ready
// package gets its own events, a rendered non-Ready revision gets its events
// even under a Ready package (the health-lag window), Ready sides never fetch,
// and an error stops everything with one note.
func TestDecoratePackageEvents(t *testing.T) {
	mkItems := func() []PackageRow {
		return []PackageRow{
			{State: StateBlocked, uid: "pkg-1", Revisions: []RevisionRow{
				{State: StateBlocked, uid: "rev-1"},
				{State: StateReady, uid: "rev-2"},
			}},
			{State: StateReady, uid: "pkg-2"},
			{State: StatePending, uid: "pkg-3"},
			// The health-lag shape: package still Ready, rendered revision failing.
			{State: StateReady, uid: "pkg-4", Revisions: []RevisionRow{
				{State: StateBlocked, uid: "rev-4"},
			}},
		}
	}

	st := &uidEvents{events: map[string][]k8s.Event{
		"pkg-1": {{Reason: "UnpackPackage", Message: "cannot unpack package: x", Type: "Warning", Count: 7}},
		"rev-1": {{Reason: "ResolveDependencies", Message: "cannot resolve package dependencies", Type: "Warning"}},
		"rev-4": {{Reason: "LintPackage", Message: "incompatible Crossplane version", Type: "Warning"}},
	}}
	items := mkItems()
	notes := DecoratePackageEvents(context.Background(), st, items)
	if len(notes) != 0 {
		t.Errorf("no notes expected on success, got %v", notes)
	}
	want := []string{"pkg-1", "rev-1", "pkg-3", "rev-4"}
	if fmt.Sprint(st.asked) != fmt.Sprint(want) {
		t.Errorf("asked %v, want %v (failing sides only; Ready package object skipped even when its revision fetches)", st.asked, want)
	}
	if len(items[0].Events) != 1 || items[0].Events[0].Reason != "UnpackPackage" {
		t.Errorf("package events should attach, got %+v", items[0].Events)
	}
	if len(items[0].Revisions[0].Events) != 1 {
		t.Errorf("failing revision events should attach, got %+v", items[0].Revisions[0].Events)
	}
	if items[0].Revisions[1].Events != nil || items[1].Events != nil {
		t.Error("Ready rows must not carry events")
	}
	if items[3].Events != nil {
		t.Error("a Ready package must not fetch its own events even when its revision does")
	}
	if len(items[3].Revisions[0].Events) != 1 || items[3].Revisions[0].Events[0].Reason != "LintPackage" {
		t.Errorf("the health-lag revision must carry its events, got %+v", items[3].Revisions[0].Events)
	}

	st = &uidEvents{err: errors.New("forbidden")}
	items = mkItems()
	notes = DecoratePackageEvents(context.Background(), st, items)
	if len(notes) != 1 || !strings.Contains(notes[0], "forbidden") {
		t.Errorf("one error note expected, got %v", notes)
	}
	if len(st.asked) != 1 {
		t.Errorf("the first error must stop all further fetches, asked %v", st.asked)
	}

	// More than maxPackageEventRows non-Ready rows → cap note, no extra asks.
	var many []PackageRow
	for i := 0; i < maxPackageEventRows+2; i++ {
		many = append(many, PackageRow{State: StateBlocked, uid: fmt.Sprintf("u%d", i)})
	}
	st = &uidEvents{}
	notes = DecoratePackageEvents(context.Background(), st, many)
	if len(st.asked) != maxPackageEventRows {
		t.Errorf("expected exactly %d fetches, got %d", maxPackageEventRows, len(st.asked))
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "first 10 failing packages") {
		t.Errorf("expected the cap note, got %v", notes)
	}
}
