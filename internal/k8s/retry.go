package k8s

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilnet "k8s.io/apimachinery/pkg/util/net"
)

// Retrying a read is safe by construction: every request this package issues is
// a get or a list, so a replayed attempt cannot change cluster state. That is
// what makes this sound here and would not make it sound in a client that
// writes.
//
// Why it matters more than latency: a failed fetch does not merely go missing.
// tree.go turns it into a node with State=StatePending and Error set, which
// ranks at tier 1 and is ordered deepest-first — and an unreachable node is
// usually the deepest, because the walk stops there. So a single 429 or a
// rolling apiserver restart can promote a blip to the NAMED ROOT CAUSE, with
// `unreachable: …` as its lead reason. The tool is then confidently wrong,
// which is worse for a diagnostic than being slow.
const (
	// retryAttempts counts total tries, not retries: 1 initial + 2 more. The
	// aim is to ride out a blip, never to wait out an outage — a real outage
	// should surface as an unreachable node promptly, since that IS the answer.
	retryAttempts = 3

	retryBaseDelay = 250 * time.Millisecond

	// maxRetryDelay caps both the exponential backoff and any server-suggested
	// Retry-After. A 429 may legitimately ask for 60s; honouring that inside a
	// tool call would blow the caller's budget for no benefit, since the walk
	// still has every other node to fetch.
	maxRetryDelay = 2 * time.Second
)

// transientAPIError reports whether a failure is worth another attempt.
//
// The list is deliberately narrow: only failures that a later identical request
// could plausibly survive. NotFound, Forbidden, Unauthorized and the other
// terminal 4xx are the honest answer to the question asked, and retrying them
// would waste the caller's time to arrive at the same place.
func transientAPIError(err error) bool {
	if err == nil {
		return false
	}
	switch {
	case apierrors.IsTooManyRequests(err), // 429: APF or a client-side burst
		apierrors.IsInternalError(err),      // 500
		apierrors.IsServiceUnavailable(err), // 503, e.g. an aggregated APIService mid-rollout
		apierrors.IsServerTimeout(err),      // 504 / server-side timeout
		apierrors.IsTimeout(err),
		apierrors.IsUnexpectedServerError(err):
		return true
	}
	// Transport-level breakage, typically an apiserver or load balancer being
	// rolled. These surface as plain net errors rather than apierrors, so the
	// checks above never see them.
	return utilnet.IsConnectionReset(err) ||
		utilnet.IsConnectionRefused(err) ||
		utilnet.IsProbableEOF(err)
}

// withRetry runs do, retrying only transient failures, and stops the moment ctx
// is done.
//
// On cancellation it returns the last API error rather than ctx.Err(): the
// caller's context is already finished, so the useful thing to carry back is
// what the server actually said. The error is returned unwrapped so callers
// keep using apierrors.IsNotFound and friends on it.
func withRetry(ctx context.Context, do func() error) error {
	delay := retryBaseDelay
	var err error
	for attempt := 1; ; attempt++ {
		if err = do(); err == nil {
			return nil
		}
		if attempt >= retryAttempts || !transientAPIError(err) {
			return err
		}
		wait := delay
		// A server that says how long to wait knows better than the backoff,
		// up to the cap.
		if secs, ok := apierrors.SuggestsClientDelay(err); ok {
			wait = time.Duration(secs) * time.Second
		}
		wait = min(wait, maxRetryDelay)

		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}
		delay *= 2
	}
}
