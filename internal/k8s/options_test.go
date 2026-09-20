package k8s

import (
	"testing"
	"time"

	"k8s.io/client-go/rest"
)

// client-go reads a zero QPS/Burst as "unset" and silently substitutes
// DefaultQPS=5 / DefaultBurst=10 (rest/config.go). Those are a pre-APF
// client-side guard and are badly matched to a bounded diagnostic walk: ~210
// requests for a full diagnose is about 40s of pure throttling. So the zero
// value has to be mapped here — passing it through is what produced the
// throttling in the first place, invisibly.
func TestApplyOptionsZeroValueDoesNotInheritClientGoDefaults(t *testing.T) {
	var cfg rest.Config
	applyOptions(&cfg, Options{})

	if cfg.QPS != DefaultQPS {
		t.Errorf("QPS = %v, want %v", cfg.QPS, DefaultQPS)
	}
	if cfg.Burst != DefaultBurst {
		t.Errorf("Burst = %d, want %d", cfg.Burst, DefaultBurst)
	}
	if cfg.QPS == 5 || cfg.Burst == 10 {
		t.Error("client-go's 5/10 defaults leaked through; the zero value was passed straight to rest.Config")
	}
	if cfg.Timeout != DefaultRequestTimeout {
		t.Errorf("Timeout = %v, want %v", cfg.Timeout, DefaultRequestTimeout)
	}
}

func TestApplyOptionsExplicitValuesWin(t *testing.T) {
	var cfg rest.Config
	applyOptions(&cfg, Options{
		RequestTimeout: 5 * time.Second,
		QPS:            7,
		Burst:          9,
		UserAgent:      "crossplane-mcp/1.2.3",
	})
	if cfg.Timeout != 5*time.Second || cfg.QPS != 7 || cfg.Burst != 9 {
		t.Errorf("explicit values not applied: %+v", cfg)
	}
	if cfg.UserAgent != "crossplane-mcp/1.2.3" {
		t.Errorf("UserAgent = %q; without it the audit log shows a generic client-go string", cfg.UserAgent)
	}
}

// Negative is the only way to express "none" once zero means "unset". The
// --request-timeout=0 flag has always meant unbounded, and main translates it.
func TestApplyOptionsNegativeDisables(t *testing.T) {
	var cfg rest.Config
	applyOptions(&cfg, Options{RequestTimeout: -1, QPS: -1, Burst: -1})

	if cfg.Timeout != 0 {
		t.Errorf("Timeout = %v, want 0 (unbounded)", cfg.Timeout)
	}
	// client-go disables its limiter only for a strictly negative QPS; 0 would
	// silently reinstate 5/s, which is the opposite of what was asked.
	if cfg.QPS >= 0 {
		t.Errorf("QPS = %v, want negative so client-go installs no rate limiter", cfg.QPS)
	}
}

// An empty UserAgent must leave rest.Config alone rather than blanking a value
// client-go would otherwise populate itself.
func TestApplyOptionsEmptyUserAgentIsNotWritten(t *testing.T) {
	cfg := rest.Config{UserAgent: "preset"}
	applyOptions(&cfg, Options{})
	if cfg.UserAgent != "preset" {
		t.Errorf("UserAgent = %q, want it untouched when Options leaves it empty", cfg.UserAgent)
	}
}
