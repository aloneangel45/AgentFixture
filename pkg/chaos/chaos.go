// Package chaos provides deterministic fault injection for testing AI agent
// retry, backoff, overload, latency, and SSE stream recovery behavior offline.
package chaos

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"time"
)

// FaultKind identifies the category of fault selected by an Injector.
type FaultKind string

const (
	FaultNone      FaultKind = ""
	FaultRateLimit FaultKind = "rate_limit"
	FaultOverload  FaultKind = "overload"
	FaultError     FaultKind = "internal_error"
)

// Config configures deterministic fault injection rules.
// When all fault fields are zero, the injector is inactive and introduces zero behavioral change.
type Config struct {
	Seed            int64   `json:"seed,omitempty" yaml:"seed,omitempty"`
	RateLimitFirstN int     `json:"rate_limit,omitempty" yaml:"rate_limit,omitempty"`
	OverloadFirstN  int     `json:"overload,omitempty" yaml:"overload,omitempty"`
	DropSSEAfter    int     `json:"drop_sse_after,omitempty" yaml:"drop_sse_after,omitempty"`
	LatencyMs       int64   `json:"latency_ms,omitempty" yaml:"latency_ms,omitempty"`
	LatencyJitterMs int64   `json:"jitter_ms,omitempty" yaml:"jitter_ms,omitempty"`
	ErrorRate       float64 `json:"error_rate,omitempty" yaml:"error_rate,omitempty"`
}

// Enabled reports whether any chaos rule is active.
func (c Config) Enabled() bool {
	return c.RateLimitFirstN > 0 ||
		c.OverloadFirstN > 0 ||
		c.DropSSEAfter > 0 ||
		c.LatencyMs > 0 ||
		c.LatencyJitterMs > 0 ||
		c.ErrorRate > 0
}

// Validate checks that all chaos configuration bounds are valid.
func (c Config) Validate() error {
	if c.RateLimitFirstN < 0 {
		return fmt.Errorf("chaos rate-limit count must be >= 0 (got %d)", c.RateLimitFirstN)
	}
	if c.OverloadFirstN < 0 {
		return fmt.Errorf("chaos overload count must be >= 0 (got %d)", c.OverloadFirstN)
	}
	if c.DropSSEAfter < 0 {
		return fmt.Errorf("chaos drop-sse-after must be >= 0 (got %d)", c.DropSSEAfter)
	}
	if c.LatencyMs < 0 {
		return fmt.Errorf("chaos latency-ms must be >= 0 (got %d)", c.LatencyMs)
	}
	if c.LatencyJitterMs < 0 {
		return fmt.Errorf("chaos jitter-ms must be >= 0 (got %d)", c.LatencyJitterMs)
	}
	if c.ErrorRate < 0 || c.ErrorRate > 1 {
		return fmt.Errorf("chaos error-rate must be between 0.0 and 1.0 (got %v)", c.ErrorRate)
	}
	return nil
}

// Decision describes the deterministic chaos actions to apply to a single request.
type Decision struct {
	Fault        FaultKind
	Latency      time.Duration
	DropSSEAfter int
	CallIndex    int
}

// Stats tracks cumulative chaos events injected across a session.
type Stats struct {
	TotalRequests       int   `json:"total_requests"`
	RateLimitsInjected  int   `json:"rate_limits_injected"`
	OverloadsInjected   int   `json:"overloads_injected"`
	ErrorsInjected      int   `json:"errors_injected"`
	SSEDropsInjected    int   `json:"sse_drops_injected"`
	TotalAddedLatencyMs int64 `json:"total_added_latency_ms"`
}

// Injector evaluates deterministic chaos rules across concurrent HTTP and MCP stdio requests.
type Injector struct {
	mu                 sync.Mutex
	cfg                Config
	rng                *rand.Rand
	rateLimitRemaining int
	overloadRemaining  int
	stats              Stats
}

// NewInjector constructs a validated deterministic Injector seeded by cfg.Seed.
func NewInjector(cfg Config) (*Injector, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Injector{
		cfg:                cfg,
		rng:                rand.New(rand.NewSource(cfg.Seed)),
		rateLimitRemaining: cfg.RateLimitFirstN,
		overloadRemaining:  cfg.OverloadFirstN,
	}, nil
}

// Enabled reports whether the injector is non-nil and has at least one active rule.
func (inj *Injector) Enabled() bool {
	if inj == nil {
		return false
	}
	return inj.cfg.Enabled()
}

// Config returns a copy of the injector's configuration.
func (inj *Injector) Config() Config {
	if inj == nil {
		return Config{}
	}
	return inj.cfg
}

// Evaluate computes the deterministic fault and latency decision for the next incoming request.
// Order of precedence:
//  1. First N requests fail with HTTP 429 / JSON-RPC -32000 (RateLimitFirstN)
//  2. Next M requests fail with HTTP 529 / JSON-RPC -32000 (OverloadFirstN)
//  3. Remaining requests sample seeded ErrorRate probability (HTTP 500 / JSON-RPC -32000)
//
// Latency (LatencyMs + seeded [0, LatencyJitterMs]) and DropSSEAfter are attached deterministically.
func (inj *Injector) Evaluate() Decision {
	if inj == nil || !inj.cfg.Enabled() {
		return Decision{}
	}

	inj.mu.Lock()
	defer inj.mu.Unlock()

	inj.stats.TotalRequests++
	dec := Decision{
		DropSSEAfter: inj.cfg.DropSSEAfter,
		CallIndex:    inj.stats.TotalRequests,
	}

	delayMs := inj.cfg.LatencyMs
	if inj.cfg.LatencyJitterMs > 0 {
		delayMs += inj.rng.Int63n(inj.cfg.LatencyJitterMs + 1)
	}
	if delayMs > 0 {
		dec.Latency = time.Duration(delayMs) * time.Millisecond
		inj.stats.TotalAddedLatencyMs += delayMs
	}

	switch {
	case inj.rateLimitRemaining > 0:
		inj.rateLimitRemaining--
		inj.stats.RateLimitsInjected++
		dec.Fault = FaultRateLimit
	case inj.overloadRemaining > 0:
		inj.overloadRemaining--
		inj.stats.OverloadsInjected++
		dec.Fault = FaultOverload
	case inj.cfg.ErrorRate > 0:
		if inj.rng.Float64() < inj.cfg.ErrorRate {
			inj.stats.ErrorsInjected++
			dec.Fault = FaultError
		}
	}

	return dec
}

// RecordSSEDrop increments the SSE truncation counter.
func (inj *Injector) RecordSSEDrop() {
	if inj == nil {
		return
	}
	inj.mu.Lock()
	inj.stats.SSEDropsInjected++
	inj.mu.Unlock()
}

// Stats returns a thread-safe snapshot of injected chaos counters.
func (inj *Injector) Stats() Stats {
	if inj == nil {
		return Stats{}
	}
	inj.mu.Lock()
	defer inj.mu.Unlock()
	return inj.stats
}

// SleepContext waits for duration d or until ctx is canceled.
func SleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// WriteHTTPFault writes a realistic provider-compatible HTTP error response for the given fault kind.
func WriteHTTPFault(w http.ResponseWriter, fault FaultKind) {
	w.Header().Set("Content-Type", "application/json")
	switch fault {
	case FaultRateLimit:
		w.Header().Set("Retry-After", "1")
		w.Header().Set("X-AgentFixture-Chaos", "rate_limit")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"type": "error",
			"error": map[string]interface{}{
				"type":    "rate_limit_error",
				"message": "Rate limit exceeded (AgentFixture deterministic chaos injection). Please retry after 1 second.",
			},
		})
	case FaultOverload:
		w.Header().Set("Retry-After", "1")
		w.Header().Set("X-AgentFixture-Chaos", "overload")
		w.WriteHeader(529)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"type": "error",
			"error": map[string]interface{}{
				"type":    "overloaded_error",
				"message": "Overloaded (AgentFixture deterministic chaos injection). Upstream API is temporarily overloaded.",
			},
		})
	default:
		w.Header().Set("X-AgentFixture-Chaos", "internal_error")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"type": "error",
			"error": map[string]interface{}{
				"type":    "api_error",
				"message": "Internal server error (AgentFixture deterministic chaos injection).",
			},
		})
	}
}

// JSONRPCFaultError builds a JSON-RPC 2.0 error object (-32000) for MCP stdio chaos injection.
func JSONRPCFaultError(fault FaultKind) map[string]interface{} {
	switch fault {
	case FaultRateLimit:
		return map[string]interface{}{
			"code":    -32000,
			"message": "Rate limit exceeded (AgentFixture deterministic chaos injection); retry after 1s",
			"data": map[string]interface{}{
				"chaos":               "rate_limit",
				"http_status":         429,
				"retry_after_seconds": 1,
			},
		}
	case FaultOverload:
		return map[string]interface{}{
			"code":    -32000,
			"message": "Server overloaded (AgentFixture deterministic chaos injection); retry after 1s",
			"data": map[string]interface{}{
				"chaos":               "overload",
				"http_status":         529,
				"retry_after_seconds": 1,
			},
		}
	default:
		return map[string]interface{}{
			"code":    -32000,
			"message": "Internal tool error (AgentFixture deterministic chaos injection)",
			"data": map[string]interface{}{
				"chaos":       "internal_error",
				"http_status": 500,
			},
		}
	}
}
