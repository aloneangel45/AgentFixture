package chaos

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestConfigValidation(t *testing.T) {
	if (Config{}).Enabled() {
		t.Fatalf("empty config should not be enabled")
	}
	if err := (Config{}).Validate(); err != nil {
		t.Fatalf("empty config should validate: %v", err)
	}

	invalid := []Config{
		{RateLimitFirstN: -1},
		{OverloadFirstN: -2},
		{DropSSEAfter: -1},
		{LatencyMs: -5},
		{LatencyJitterMs: -1},
		{ErrorRate: -0.1},
		{ErrorRate: 1.5},
	}
	for i, cfg := range invalid {
		if _, err := NewInjector(cfg); err == nil {
			t.Errorf("case %d (%+v): expected validation error, got nil", i, cfg)
		}
	}
}

func TestInjectorDeterministicRateLimitAndOverload(t *testing.T) {
	inj, err := NewInjector(Config{
		RateLimitFirstN: 2,
		OverloadFirstN:  1,
	})
	if err != nil {
		t.Fatal(err)
	}

	want := []FaultKind{FaultRateLimit, FaultRateLimit, FaultOverload, FaultNone, FaultNone}
	for i, w := range want {
		dec := inj.Evaluate()
		if dec.Fault != w {
			t.Fatalf("call %d: got fault %q, want %q", i+1, dec.Fault, w)
		}
		if dec.CallIndex != i+1 {
			t.Fatalf("call %d: got CallIndex %d", i+1, dec.CallIndex)
		}
	}

	st := inj.Stats()
	if st.TotalRequests != 5 || st.RateLimitsInjected != 2 || st.OverloadsInjected != 1 || st.ErrorsInjected != 0 {
		t.Fatalf("unexpected stats: %+v", st)
	}
}

func TestInjectorDeterministicSeededErrorRateAndJitter(t *testing.T) {
	cfg := Config{
		Seed:            1337,
		LatencyMs:       10,
		LatencyJitterMs: 25,
		ErrorRate:       0.4,
	}
	inj1, err := NewInjector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	inj2, err := NewInjector(cfg)
	if err != nil {
		t.Fatal(err)
	}

	var faults1, faults2 []FaultKind
	var lat1, lat2 []time.Duration
	for i := 0; i < 20; i++ {
		d1 := inj1.Evaluate()
		d2 := inj2.Evaluate()
		faults1 = append(faults1, d1.Fault)
		faults2 = append(faults2, d2.Fault)
		lat1 = append(lat1, d1.Latency)
		lat2 = append(lat2, d2.Latency)
		if d1.Latency < 10*time.Millisecond || d1.Latency > 35*time.Millisecond {
			t.Fatalf("latency out of bounds [10ms, 35ms]: %v", d1.Latency)
		}
	}

	for i := range faults1 {
		if faults1[i] != faults2[i] || lat1[i] != lat2[i] {
			t.Fatalf("non-deterministic decision at index %d: (%v,%v) vs (%v,%v)", i, faults1[i], lat1[i], faults2[i], lat2[i])
		}
	}
	if inj1.Stats().ErrorsInjected == 0 {
		t.Fatalf("expected at least one error with ErrorRate=0.4 over 20 draws")
	}
}

func TestWriteHTTPFaultPayloads(t *testing.T) {
	tests := []struct {
		fault      FaultKind
		wantStatus int
		wantType   string
		wantRetry  string
	}{
		{FaultRateLimit, http.StatusTooManyRequests, "rate_limit_error", "1"},
		{FaultOverload, 529, "overloaded_error", "1"},
		{FaultError, http.StatusInternalServerError, "api_error", ""},
	}

	for _, tc := range tests {
		rec := httptest.NewRecorder()
		WriteHTTPFault(rec, tc.fault)
		if rec.Code != tc.wantStatus {
			t.Errorf("%s: status=%d, want %d", tc.fault, rec.Code, tc.wantStatus)
		}
		if got := rec.Header().Get("Retry-After"); got != tc.wantRetry {
			t.Errorf("%s: Retry-After=%q, want %q", tc.fault, got, tc.wantRetry)
		}
		var body map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: invalid json body: %v", tc.fault, err)
		}
		errObj, ok := body["error"].(map[string]interface{})
		if !ok || errObj["type"] != tc.wantType {
			t.Errorf("%s: unexpected error body: %+v", tc.fault, body)
		}
	}
}

func TestJSONRPCFaultError(t *testing.T) {
	for _, fk := range []FaultKind{FaultRateLimit, FaultOverload, FaultError} {
		errMap := JSONRPCFaultError(fk)
		if errMap["code"] != -32000 {
			t.Errorf("%s: expected code -32000, got %v", fk, errMap["code"])
		}
		if msg, ok := errMap["message"].(string); !ok || msg == "" {
			t.Errorf("%s: expected non-empty message, got %v", fk, errMap["message"])
		}
	}
}

func TestInjectorConcurrentSafety(t *testing.T) {
	inj, err := NewInjector(Config{
		Seed:            99,
		RateLimitFirstN: 5,
		OverloadFirstN:  3,
		ErrorRate:       0.2,
		LatencyJitterMs: 5,
	})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				_ = inj.Evaluate()
			}
		}()
	}
	wg.Wait()

	st := inj.Stats()
	if st.TotalRequests != 200 || st.RateLimitsInjected != 5 || st.OverloadsInjected != 3 {
		t.Fatalf("unexpected concurrent stats: %+v", st)
	}
}

func TestSleepContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := SleepContext(ctx, 5*time.Second); err == nil {
		t.Fatalf("expected context cancellation error")
	}
	if err := SleepContext(context.Background(), 0); err != nil {
		t.Fatalf("expected nil for zero duration, got %v", err)
	}
}

func TestConfigValidation_NaNInfAndLatencyOverflow(t *testing.T) {
	invalid := []Config{
		{ErrorRate: math.NaN()},
		{ErrorRate: math.Inf(1)},
		{ErrorRate: math.Inf(-1)},
		{LatencyMs: math.MaxInt64},
		{LatencyMs: MaxLatencyMs + 1},
		{LatencyJitterMs: math.MaxInt64},
		{LatencyJitterMs: MaxLatencyMs + 1},
	}
	for i, cfg := range invalid {
		if err := cfg.Validate(); err == nil {
			t.Errorf("case %d (%+v): expected validation error, got nil", i, cfg)
		}
		if _, err := NewInjector(cfg); err == nil {
			t.Errorf("case %d (%+v): expected NewInjector error, got nil", i, cfg)
		}
	}
}
