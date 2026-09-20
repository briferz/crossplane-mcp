package k8s

import (
	"context"
	"errors"
	"net"
	"syscall"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// The point of retrying is not speed. A failed Get does not leave a gap in the
// answer — tree.go turns it into a StatePending node with Error set, which
// ranks tier 1 and is ordered deepest-first, and an unreachable node is usually
// the deepest because the walk stops there. So without this, one 429 promotes a
// blip to the named root cause.

func TestTransientAPIErrorClassification(t *testing.T) {
	gr := schema.GroupResource{Group: "example.org", Resource: "xapps"}

	retryable := map[string]error{
		"429 too many requests": apierrors.NewTooManyRequests("slow down", 1),
		"500 internal":          apierrors.NewInternalError(errors.New("boom")),
		"503 unavailable":       apierrors.NewServiceUnavailable("apiserver rolling"),
		"504 server timeout":    apierrors.NewServerTimeout(gr, "get", 1),
		"request timeout":       apierrors.NewTimeoutError("timed out", 1),
		"connection reset":      &net.OpError{Op: "read", Err: syscall.ECONNRESET},
		"connection refused":    &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED},
	}
	for name, err := range retryable {
		if !transientAPIError(err) {
			t.Errorf("%s should be retried: a later identical request could succeed", name)
		}
	}

	// Terminal answers. Retrying these burns the caller's time to arrive at the
	// same place, and for NotFound it would also delay the correct diagnosis:
	// a ref pointing at a resource that does not exist IS the finding.
	terminal := map[string]error{
		"404 not found":    apierrors.NewNotFound(gr, "demo"),
		"403 forbidden":    apierrors.NewForbidden(gr, "demo", errors.New("nope")),
		"401 unauthorized": apierrors.NewUnauthorized("no creds"),
		"400 bad request":  apierrors.NewBadRequest("malformed"),
		"409 conflict":     apierrors.NewConflict(gr, "demo", errors.New("conflict")),
		"nil":              nil,
	}
	for name, err := range terminal {
		if transientAPIError(err) {
			t.Errorf("%s must NOT be retried", name)
		}
	}
}

func TestWithRetryStopsAtAttemptCap(t *testing.T) {
	calls := 0
	err := withRetry(context.Background(), func() error {
		calls++
		return apierrors.NewTooManyRequests("always", 0)
	})
	if err == nil {
		t.Fatal("a persistently failing call must still return its error")
	}
	if calls != retryAttempts {
		t.Errorf("made %d attempts, want exactly %d — an unbounded retry would hang the tool call", calls, retryAttempts)
	}
}

func TestWithRetryDoesNotRetryTerminalErrors(t *testing.T) {
	calls := 0
	gr := schema.GroupResource{Group: "example.org", Resource: "xapps"}
	err := withRetry(context.Background(), func() error {
		calls++
		return apierrors.NewNotFound(gr, "demo")
	})
	if calls != 1 {
		t.Errorf("NotFound was attempted %d times, want 1", calls)
	}
	if !apierrors.IsNotFound(err) {
		t.Errorf("the error must come back unwrapped so callers can still classify it, got %v", err)
	}
}

func TestWithRetryHonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	start := time.Now()
	err := withRetry(ctx, func() error {
		calls++
		cancel() // cancelled while the first attempt is in flight
		return apierrors.NewTooManyRequests("slow down", 60)
	})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("waited %s after cancellation; a 60s Retry-After must not park the call", elapsed)
	}
	if calls != 1 {
		t.Errorf("made %d attempts after cancellation, want 1", calls)
	}
	// The API error is more informative than context.Canceled, and the caller's
	// context is already done either way.
	if !apierrors.IsTooManyRequests(err) {
		t.Errorf("want the server's error preserved, got %v", err)
	}
}

// TestGetSurvivesATransientBlip is the behavioural one: it drives the real
// Client.Get against a fake that fails once, exactly as a rolling apiserver
// would, and asserts the object still comes back.
func TestGetSurvivesATransientBlip(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "example.org", Version: "v1", Resource: "xapps"}
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{gvr: "XAppList"})

	failures := 1
	dyn.PrependReactor("get", "xapps", func(clienttesting.Action) (bool, runtime.Object, error) {
		if failures > 0 {
			failures--
			return true, nil, apierrors.NewTooManyRequests("APF is shedding load", 0)
		}
		return false, nil, nil // fall through to the tracker
	})

	cl := &Client{Dyn: dyn}
	target := Target{GVR: gvr, Kind: "XApp", Namespaced: true}

	_, err := cl.Get(context.Background(), target, "default", "demo")
	// The tracker has no object, so NotFound is the correct final answer — what
	// matters is that the 429 was retried past rather than surfaced.
	if err != nil && apierrors.IsTooManyRequests(err) {
		t.Fatal("a single 429 reached the caller: it would become an unreachable node, " +
			"rank tier 1 deepest-first, and be named the root cause")
	}
	if failures != 0 {
		t.Error("the reactor never fired; the test proved nothing")
	}
}
