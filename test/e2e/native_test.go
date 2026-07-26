package e2e

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"

	"github.com/briferz/crossplane-mcp/internal/k8s"
	"github.com/briferz/crossplane-mcp/internal/xp"
)

// The native-readiness tier: a real cluster with a real kube-controller-manager
// and kubelet, but NO Crossplane. It exists to settle questions about how
// upstream controllers actually write status, which is what internal/xp/native.go
// encodes and what nothing cheaper can answer:
//
//   - unit tests assert against conditions THIS repo wrote, so they only prove
//     the rule reads its own fixture back;
//   - envtest has no kube-controller-manager and no kubelet, so no Deployment
//     controller ever runs and no pod ever fails to start.
//
// Only one external image (kindest/node) is on its critical path, which is why
// it is a separate job from the Crossplane tier rather than more assertions
// inside it.

const (
	// Short enough that a wedged rollout trips it promptly, long enough that
	// ordinary scheduling on a cold kind node does not.
	probeProgressDeadline int32 = 15

	// imagePullPolicy: Never + an image that cannot exist locally fails
	// immediately with ErrImageNeverPull and touches no registry at all, so the
	// "wedged" half of this probe is fully offline and has no backoff timing to
	// race against.
	probeBadImage = "crossplane-mcp.invalid/nonexistent:v0"
)

// probeGoodImage is the image the recovery step rolls forward to. It is
// preloaded into the kind node by the workflow (`kind load docker-image`), so
// IfNotPresent resolves locally; the policy is not Never so that a developer
// running this against their own cluster still works, at the cost of one pull.
func probeGoodImage() string {
	if img := os.Getenv("PROBE_IMAGE"); img != "" {
		return img
	}
	return "registry.k8s.io/pause:3.10"
}

// requireCluster skips unless a real cluster with controllers is available.
func requireCluster(t *testing.T) {
	t.Helper()
	if !clusterTier() {
		t.Skip("needs a real cluster with a kube-controller-manager; set CLUSTER_E2E=1 " +
			"with KUBECONFIG pointed at one")
	}
}

// writeClient builds a write-capable typed client for the harness. The server
// under test never gets one of these — it only ever sees k8s.Client, whose
// read-only surface is what the forbidigo rule and TestReadOnlyAgainstRealCluster
// police. The harness must write to build the fixture, which is exactly why this
// module is separate from the shipped one.
func writeClient(t *testing.T) kubernetes.Interface {
	t.Helper()
	loading := clientcmd.NewDefaultClientConfigLoadingRules()
	if kc := os.Getenv("KUBECONFIG"); kc != "" {
		loading.ExplicitPath = kc
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loading, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		t.Fatalf("loading kubeconfig: %v", err)
	}
	return kubernetes.NewForConfigOrDie(cfg)
}

// TestNativeReadinessDeploymentRolloutRecovery is the probe that settles the
// question native.go's Deployment rule rests on but could not previously answer:
//
//	once the controller has written Progressing=False/ProgressDeadlineExceeded,
//	does it EVER go back to True after the rollout is fixed?
//
// It matters because deploymentReadiness treats that exact pair as Blocked with
// no "only if Available != True" guard — deliberately, since the canonical stuck
// rollout keeps Available=True while old pods serve under maxSurge. If the
// condition were terminal, that same rule would strand a fully recovered,
// serving Deployment at Blocked forever, and a permanent false suspect is the
// bug class this package spent a release removing (the Succeeded-Pod one).
//
// Upstream says it recovers, and the mechanism is worth writing down because one
// guard very nearly makes it terminal. In syncRolloutStatus
// (pkg/controller/deployment/progress.go) the whole re-evaluation is skipped when
//
//	isCompleteDeployment := newStatus.Replicas == newStatus.UpdatedReplicas &&
//	    currentCond != nil && currentCond.Reason == util.NewRSAvailableReason
//
// After a timeout the current reason is ProgressDeadlineExceeded, NOT
// NewRSAvailableReason — so the guard is false, the switch runs, and
// DeploymentComplete re-sets Progressing=True with reason NewReplicaSetAvailable.
//
// That is what the source says. This asserts it against the controller actually
// running in the cluster under test, and asserts that OUR rule reads the result
// correctly end to end — which source-reading cannot establish.
func TestNativeReadinessDeploymentRolloutRecovery(t *testing.T) {
	requireCluster(t)
	ctx := context.Background()
	core := writeClient(t)
	read := clusterClient(t)

	ns := mustProbeNamespace(t, core, "xpmcp-rollout")
	name := "rollout-probe"

	// Phase 1: a rollout that cannot complete.
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas:                ptr.To[int32](1),
			ProgressDeadlineSeconds: ptr.To(probeProgressDeadline),
			Selector:                &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:            "c",
						Image:           probeBadImage,
						ImagePullPolicy: corev1.PullNever,
					}},
				},
			},
		},
	}
	if _, err := core.AppsV1().Deployments(ns).Create(ctx, dep, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create deployment: %v", err)
	}

	wedged := waitForDeployment(t, ctx, core, ns, name, 3*time.Minute,
		"Progressing=False/ProgressDeadlineExceeded",
		func(d *appsv1.Deployment) bool {
			c := deploymentCondition(d, appsv1.DeploymentProgressing)
			return c != nil && c.Status == corev1.ConditionFalse && c.Reason == "ProgressDeadlineExceeded"
		})
	t.Logf("wedged: %s", describeConditions(wedged))

	// The rule must call this Blocked — it is a genuinely stuck rollout, and
	// missing it is the signal-drop half of the risk.
	if state, reasons := classifyVia(t, read, "apps/v1", "Deployment", ns, name); state != xp.StateBlocked {
		t.Errorf("a Deployment past its progress deadline classified %q, want %q; "+
			"deploymentReadiness no longer catches a stuck rollout.\nconditions: %s",
			state, xp.StateBlocked, describeConditions(wedged))
	} else if len(reasons) == 0 {
		t.Error("a Blocked verdict must explain itself; got no reasons")
	}

	// Phase 2: fix it, and let the rollout genuinely complete.
	patched, err := core.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("re-get deployment: %v", err)
	}
	patched.Spec.Template.Spec.Containers[0].Image = probeGoodImage()
	patched.Spec.Template.Spec.Containers[0].ImagePullPolicy = corev1.PullIfNotPresent
	if _, err := core.AppsV1().Deployments(ns).Update(ctx, patched, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update deployment image: %v", err)
	}

	// Wait for FULLY rolled out, not merely Available: the interesting sample is
	// the steady state after the new ReplicaSet is completely in place, because
	// that is where a stranded Progressing=False would be unambiguously wrong.
	recovered := waitForDeployment(t, ctx, core, ns, name, 5*time.Minute,
		"the replacement rollout to complete",
		func(d *appsv1.Deployment) bool {
			want := *d.Spec.Replicas
			return d.Status.ObservedGeneration >= d.Generation &&
				d.Status.UpdatedReplicas == want &&
				d.Status.AvailableReplicas == want &&
				d.Status.Replicas == want
		})
	t.Logf("recovered: %s", describeConditions(recovered))

	// The empirical answer, recorded explicitly so a future reader does not have
	// to re-derive it from the controller source.
	prog := deploymentCondition(recovered, appsv1.DeploymentProgressing)
	switch {
	case prog == nil:
		t.Error("no Progressing condition after recovery — deploymentReadiness keys on it")
	case prog.Status != corev1.ConditionTrue:
		t.Errorf("Progressing did NOT return to True after a completed rollout "+
			"(status=%s reason=%s). ProgressDeadlineExceeded is therefore terminal, and "+
			"deploymentReadiness strands a recovered Deployment at Blocked forever — a "+
			"permanent false suspect. The rule needs a completeness check, not the "+
			"Available guard its comment rules out.\nconditions: %s",
			prog.Status, prog.Reason, describeConditions(recovered))
	default:
		t.Logf("Progressing returned to True (reason=%s) after recovery — "+
			"ProgressDeadlineExceeded is transient, as deploymentReadiness assumes", prog.Reason)
	}

	// What the shipped rule actually says about a healthy, serving Deployment.
	if state, reasons := classifyVia(t, read, "apps/v1", "Deployment", ns, name); state != xp.StateReady {
		t.Errorf("a fully recovered Deployment classified %q (reasons: %v), want %q — "+
			"this is a live false positive: every diagnose against this cluster names a "+
			"working Deployment as a suspect.\nconditions: %s",
			state, reasons, xp.StateReady, describeConditions(recovered))
	}
}

// TestNativeReadinessSucceededPodIsReady pins the regression that motivated
// native.go against a REAL terminated pod. A Pod in phase Succeeded carries
// Ready=False/PodCompleted forever, which Classify alone reads as Blocked —
// making every finished init or migration Pod the permanent top-ranked suspect.
// The unit test asserts this against a hand-written fixture; only a real kubelet
// proves the fixture matches what Kubernetes writes.
func TestNativeReadinessSucceededPodIsReady(t *testing.T) {
	requireCluster(t)
	ctx := context.Background()
	core := writeClient(t)
	read := clusterClient(t)

	ns := mustProbeNamespace(t, core, "xpmcp-pod")
	name := "succeeded-probe"

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:            "c",
				Image:           probeGoodImage(),
				ImagePullPolicy: corev1.PullIfNotPresent,
				// args, not command: this appends to the image's entrypoint
				// rather than hardcoding the binary's path, so the probe does
				// not depend on where pause lives in the image. pause.c parses
				// "-v" before any other initialisation, prints its version and
				// returns 0 (kubernetes/build/pause/linux/pause.c), so the pod
				// terminates Succeeded rather than running forever.
				Args: []string{"-v"},
			}},
		},
	}
	if _, err := core.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	var final *corev1.Pod
	waitUntil(t, 3*time.Minute, "the pod to terminate", func() bool {
		got, err := core.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		final = got
		return got.Status.Phase == corev1.PodSucceeded || got.Status.Phase == corev1.PodFailed
	})
	// Fatal, never Skip. If the image stops honouring "-v" the pod ends Failed,
	// and skipping would leave this tier permanently green while asserting
	// nothing — the exact rot this module's README warns about. Fixture rot must
	// be loud: noticing upstream shape changes is the tier's only job.
	if final.Status.Phase != corev1.PodSucceeded {
		t.Fatalf("probe pod ended in phase %q, want Succeeded — the fixture no longer "+
			"terminates cleanly on %s, so the assertions below would prove nothing. "+
			"Fix the fixture (or PROBE_IMAGE); do not relax this check.",
			final.Status.Phase, probeGoodImage())
	}

	// The fact the unit fixture encodes, confirmed against a real kubelet.
	if c := podCondition(final, corev1.PodReady); c == nil || c.Status != corev1.ConditionFalse {
		t.Errorf("a Succeeded pod no longer reports Ready=False (got %v) — native.go's Pod "+
			"rule is written for a status shape Kubernetes no longer produces", c)
	}

	if state, _ := classifyVia(t, read, "v1", "Pod", ns, name); state != xp.StateReady {
		t.Errorf("a Succeeded pod classified %q, want %q — the finished-Pod false positive "+
			"is back, and every completed init/migration Pod is a top-ranked suspect", state, xp.StateReady)
	}
}

// classifyVia reads through the SERVER's read-only client and runs the shipped
// classifier, so the probe exercises resolve -> get -> ClassifyObject rather than
// the typed object the harness already holds.
func classifyVia(t *testing.T, cl *k8s.Client, apiVersion, kind, ns, name string) (string, []string) {
	t.Helper()
	target, err := cl.Resolve(apiVersion, kind)
	if err != nil {
		t.Fatalf("resolve %s/%s: %v", apiVersion, kind, err)
	}
	obj, err := cl.Get(context.Background(), target, ns, name)
	if err != nil {
		t.Fatalf("get %s/%s: %v", kind, name, err)
	}
	_, state, reasons := xp.ClassifyObject(obj)
	return state, reasons
}

func mustProbeNamespace(t *testing.T, core kubernetes.Interface, prefix string) string {
	t.Helper()
	ctx := context.Background()
	ns, err := core.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{GenerateName: prefix + "-"},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	name := ns.GetName()
	t.Cleanup(func() {
		err := core.CoreV1().Namespaces().Delete(context.Background(), name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			t.Logf("cleanup namespace %s: %v", name, err)
		}
	})
	return name
}

func waitForDeployment(t *testing.T, ctx context.Context, core kubernetes.Interface,
	ns, name string, timeout time.Duration, what string,
	done func(*appsv1.Deployment) bool,
) *appsv1.Deployment {
	t.Helper()
	var last *appsv1.Deployment
	waitUntil(t, timeout, what, func() bool {
		got, err := core.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		last = got
		return done(got)
	})
	if last == nil {
		t.Fatalf("deployment %s/%s was never readable", ns, name)
	}
	return last
}

// waitUntil polls rather than watches: the condition here is a steady state, and
// a poll loop cannot miss an edge or wedge on a dropped watch.
func waitUntil(t *testing.T, timeout time.Duration, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

func deploymentCondition(d *appsv1.Deployment, typ appsv1.DeploymentConditionType) *appsv1.DeploymentCondition {
	for i := range d.Status.Conditions {
		if d.Status.Conditions[i].Type == typ {
			return &d.Status.Conditions[i]
		}
	}
	return nil
}

func podCondition(p *corev1.Pod, typ corev1.PodConditionType) *corev1.PodCondition {
	for i := range p.Status.Conditions {
		if p.Status.Conditions[i].Type == typ {
			return &p.Status.Conditions[i]
		}
	}
	return nil
}

// describeConditions renders the controller's own account of the object, so a
// failure here is diagnosable from the log alone — this tier runs weekly and
// nobody will have the cluster when they read it.
func describeConditions(d *appsv1.Deployment) string {
	out := fmt.Sprintf("gen=%d observed=%d replicas=%d updated=%d available=%d |",
		d.Generation, d.Status.ObservedGeneration, d.Status.Replicas,
		d.Status.UpdatedReplicas, d.Status.AvailableReplicas)
	for _, c := range d.Status.Conditions {
		out += fmt.Sprintf(" %s=%s(%s)", c.Type, c.Status, c.Reason)
	}
	return out
}
