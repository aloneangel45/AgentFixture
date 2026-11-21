package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
	"github.com/aloneangel45/AgentFixture/pkg/chaos"
	"github.com/aloneangel45/AgentFixture/pkg/matcher"
)

func TestHTTPProxy_ChaosRateLimitOverloadAndLatency(t *testing.T) {
	cas := cassette.NewCassette("cas_chaos_http")
	req := cassette.Request{
		Method:   "POST",
		Endpoint: "/v1/messages",
		RawBody:  `{"prompt":"ping"}`,
	}
	req.Matcher.StrictHash = matcher.ComputeRequestHash(&req)
	cas.Interactions = append(cas.Interactions, cassette.Interaction{
		ID:       "int_001",
		Sequence: 1,
		Type:     cassette.InteractionTypeHTTP,
		Request:  req,
		Response: cassette.Response{
			StatusCode: 200,
			Headers:    map[string]string{"Content-Type": "application/json"},
			RawBody:    `{"ok":true}`,
		},
	})

	inj, err := chaos.NewInjector(chaos.Config{
		Seed:            42,
		RateLimitFirstN: 2,
		OverloadFirstN:  1,
		LatencyMs:       15,
	})
	if err != nil {
		t.Fatal(err)
	}

	srv, err := NewServer(Config{
		Mode:     ModeReplay,
		Cassette: cas,
		Chaos:    inj,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	client := NewOfflineHTTPClient(srv.URL())
	post := func() (*http.Response, []byte, time.Duration) {
		t0 := time.Now()
		resp, err := client.Post(srv.URL()+"/v1/messages", "application/json", strings.NewReader(`{"prompt":"ping"}`))
		if err != nil {
			t.Fatalf("http post failed: %v", err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp, b, time.Since(t0)
	}

	// Call 1 & 2: HTTP 429 with Retry-After: 1
	for i := 1; i <= 2; i++ {
		resp, _, elapsed := post()
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("call %d: got status %d, want 429", i, resp.StatusCode)
		}
		if resp.Header.Get("Retry-After") != "1" {
			t.Fatalf("call %d: missing Retry-After: 1 header", i)
		}
		if elapsed < 12*time.Millisecond {
			t.Fatalf("call %d: expected >=15ms chaos latency, got %v", i, elapsed)
		}
	}

	// Call 3: HTTP 529 Overloaded
	resp3, body3, _ := post()
	if resp3.StatusCode != 529 {
		t.Fatalf("call 3: got status %d, want 529", resp3.StatusCode)
	}
	if !strings.Contains(string(body3), "overloaded_error") {
		t.Fatalf("call 3: expected overloaded_error body, got %s", string(body3))
	}

	// Call 4: Retry succeeds cleanly and matches int_001!
	resp4, body4, _ := post()
	if resp4.StatusCode != http.StatusOK || string(body4) != `{"ok":true}` {
		t.Fatalf("call 4: got status=%d body=%s, want 200 ok", resp4.StatusCode, string(body4))
	}
	if srv.MatchedCount() != 1 || len(srv.UnmatchedRequests()) != 0 {
		t.Fatalf("expected 1 matched and 0 unmatched after chaos retries, got matched=%d unmatched=%d",
			srv.MatchedCount(), len(srv.UnmatchedRequests()))
	}
	st := srv.ChaosStats()
	if st.RateLimitsInjected != 2 || st.OverloadsInjected != 1 {
		t.Fatalf("unexpected chaos stats: %+v", st)
	}
}

func TestHTTPProxy_ChaosDropSSEAfter(t *testing.T) {
	events, err := cassette.DecodeSSE(strings.NewReader(
		"event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"part1\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"part2\"}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	))
	if err != nil {
		t.Fatal(err)
	}

	cas := cassette.NewCassette("cas_chaos_sse")
	req := cassette.Request{
		Method:   "POST",
		Endpoint: "/v1/messages",
		RawBody:  `{"stream":true}`,
	}
	req.Matcher.StrictHash = matcher.ComputeRequestHash(&req)
	cas.Interactions = append(cas.Interactions, cassette.Interaction{
		ID:       "int_sse",
		Sequence: 1,
		Type:     cassette.InteractionTypeSSE,
		Request:  req,
		Response: cassette.Response{
			StatusCode: 200,
			Headers:    map[string]string{"Content-Type": "text/event-stream"},
			SSEEvents:  events,
		},
	})

	inj, err := chaos.NewInjector(chaos.Config{DropSSEAfter: 2})
	if err != nil {
		t.Fatal(err)
	}

	srv, err := NewServer(Config{
		Mode:     ModeReplay,
		Cassette: cas,
		Chaos:    inj,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	client := NewOfflineHTTPClient(srv.URL())
	resp, err := client.Post(srv.URL()+"/v1/messages", "application/json", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatalf("post failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.Header.Get("X-AgentFixture-Chaos") != "drop_sse" {
		t.Fatalf("expected X-AgentFixture-Chaos=drop_sse header, got %q", resp.Header.Get("X-AgentFixture-Chaos"))
	}

	decoded, decErr := cassette.DecodeSSE(resp.Body)
	if decErr == nil {
		t.Fatalf("expected stream interruption error (unexpected EOF) after hijacked SSE drop, got nil (events=%d)", len(decoded))
	}
	if len(decoded) != 2 {
		t.Fatalf("expected exactly 2 SSE events before abrupt drop, got %d", len(decoded))
	}
	if srv.ChaosStats().SSEDropsInjected != 1 {
		t.Fatalf("expected 1 SSE drop recorded in chaos stats, got %+v", srv.ChaosStats())
	}
}

func TestStdioReplayer_ChaosInjection(t *testing.T) {
	cas := cassette.NewCassette("cas_mcp_chaos")
	req := cassette.Request{
		Method:    "tools/call",
		ToolName:  "get_weather",
		ToolInput: map[string]interface{}{"city": "Istanbul"},
	}
	req.Matcher.StrictHash = matcher.ComputeToolHash("get_weather", req.ToolInput)
	cas.Interactions = append(cas.Interactions, cassette.Interaction{
		ID:       "mcp_001",
		Sequence: 1,
		Type:     cassette.InteractionTypeTool,
		Request:  req,
		Response: cassette.Response{
			StatusCode: 0,
			ToolResult: "Sunny 22C",
		},
	})

	inj, err := chaos.NewInjector(chaos.Config{
		RateLimitFirstN: 1,
		LatencyMs:       15,
	})
	if err != nil {
		t.Fatal(err)
	}

	replayer := NewStdioReplayer(cas, matcher.NewStrictMatcher())
	replayer.SetChaos(inj)

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_weather","arguments":{"city":"Istanbul"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_weather","arguments":{"city":"Istanbul"}}}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	start := time.Now()
	if err := replayer.ProcessStream(strings.NewReader(input), &out); err != nil {
		t.Fatalf("ProcessStream failed: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 25*time.Millisecond {
		t.Fatalf("expected >=30ms combined latency across 2 MCP calls, got %v", elapsed)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 JSON-RPC responses, got %d: %s", len(lines), out.String())
	}

	var r1, r2 JSONRPCResponse
	if err := json.Unmarshal([]byte(lines[0]), &r1); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &r2); err != nil {
		t.Fatal(err)
	}

	errObj, ok := r1.Error.(map[string]interface{})
	if !ok || errObj["code"] != float64(-32000) {
		t.Fatalf("expected first MCP call to receive JSON-RPC -32000 chaos error, got: %+v", r1)
	}
	if r2.Error != nil || r2.Result == nil {
		t.Fatalf("expected second MCP call (retry) to succeed cleanly, got: %+v", r2)
	}
	if replayer.MatchedCount() != 1 {
		t.Fatalf("expected MatchedCount=1, got %d", replayer.MatchedCount())
	}
}

func TestHTTPProxy_HybridRecordModeNewEpisodes(t *testing.T) {
	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"reply":"from_upstream_episode_2"}`))
	}))
	defer upstream.Close()

	// Seed cassette with episode 1 only
	cas := cassette.NewCassette("cas_hybrid")
	req1 := cassette.Request{
		Method:   "POST",
		Endpoint: "/v1/messages",
		RawBody:  `{"prompt":"episode_1"}`,
	}
	req1.Matcher.StrictHash = matcher.ComputeRequestHash(&req1)
	cas.Interactions = append(cas.Interactions, cassette.Interaction{
		ID:       "int_001",
		Sequence: 1,
		Type:     cassette.InteractionTypeHTTP,
		Request:  req1,
		Response: cassette.Response{
			StatusCode: 200,
			Headers:    map[string]string{"Content-Type": "application/json"},
			RawBody:    `{"reply":"offline_episode_1"}`,
		},
	})

	// Validation: new_episodes without UpstreamURL must fail
	if _, err := NewServer(Config{Mode: ModeReplay, RecordMode: RecordModeNewEpisodes, Cassette: cas}); err == nil {
		t.Fatalf("expected error when RecordModeNewEpisodes lacks UpstreamURL")
	}

	// Start hybrid new_episodes server
	srv, err := NewServer(Config{
		Mode:        ModeReplay,
		RecordMode:  RecordModeNewEpisodes,
		UpstreamURL: upstream.URL,
		Cassette:    cas,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}

	// 1. Matched episode 1 must replay OFFLINE (0 upstream calls)
	r1, err := client.Post(srv.URL()+"/v1/messages", "application/json", strings.NewReader(`{"prompt":"episode_1"}`))
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := io.ReadAll(r1.Body)
	_ = r1.Body.Close()
	if r1.StatusCode != 200 || string(b1) != `{"reply":"offline_episode_1"}` {
		t.Fatalf("episode 1 mismatch: status=%d body=%s", r1.StatusCode, string(b1))
	}
	if upstreamHits.Load() != 0 {
		t.Fatalf("matched episode 1 must NOT hit upstream, got %d hits", upstreamHits.Load())
	}

	// 2. Unmatched episode 2 forwards to upstream and appends to cassette
	r2, err := client.Post(srv.URL()+"/v1/messages", "application/json", strings.NewReader(`{"prompt":"episode_2"}`))
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := io.ReadAll(r2.Body)
	_ = r2.Body.Close()
	if r2.StatusCode != 200 || string(b2) != `{"reply":"from_upstream_episode_2"}` {
		t.Fatalf("episode 2 mismatch: status=%d body=%s", r2.StatusCode, string(b2))
	}
	if upstreamHits.Load() != 1 {
		t.Fatalf("expected exactly 1 upstream hit for unmatched episode 2, got %d", upstreamHits.Load())
	}
	if srv.NewEpisodesCount() != 1 || srv.MatchedCount() != 2 || len(srv.UnmatchedRequests()) != 0 {
		t.Fatalf("unexpected hybrid stats: new=%d matched=%d unmatched=%d",
			srv.NewEpisodesCount(), srv.MatchedCount(), len(srv.UnmatchedRequests()))
	}

	updatedCas := srv.GetCassette()
	if len(updatedCas.Interactions) != 2 {
		t.Fatalf("expected 2 interactions in updated cassette, got %d", len(updatedCas.Interactions))
	}

	// 3. Close upstream completely and verify strict RecordModeNone replays BOTH episodes offline!
	upstream.Close()
	offlineSrv, err := NewServer(Config{
		Mode:       ModeReplay,
		RecordMode: RecordModeNone,
		Cassette:   updatedCas,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := offlineSrv.Start(); err != nil {
		t.Fatal(err)
	}
	defer offlineSrv.Close()

	offlineClient := NewOfflineHTTPClient(offlineSrv.URL())
	for _, prompt := range []string{`{"prompt":"episode_1"}`, `{"prompt":"episode_2"}`} {
		resp, err := offlineClient.Post(offlineSrv.URL()+"/v1/messages", "application/json", strings.NewReader(prompt))
		if err != nil {
			t.Fatalf("offline replay failed for %s: %v", prompt, err)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200 offline for %s, got %d", prompt, resp.StatusCode)
		}
	}
}

func TestHTTPProxy_ChaosDropSSE_NonHijackableAndClientCancel(t *testing.T) {
	events, err := cassette.DecodeSSE(strings.NewReader(
		"event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"a\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"b\"}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	))
	if err != nil {
		t.Fatal(err)
	}
	for i := range events {
		events[i].DeltaMs = 50
	}

	cas := cassette.NewCassette("cas_sse_non_hijack")
	req := cassette.Request{
		Method:   "POST",
		Endpoint: "/v1/messages",
		RawBody:  `{"stream":true}`,
	}
	req.Matcher.StrictHash = matcher.ComputeRequestHash(&req)
	cas.Interactions = append(cas.Interactions, cassette.Interaction{
		ID:       "int_sse_1",
		Sequence: 1,
		Type:     cassette.InteractionTypeSSE,
		Request:  req,
		Response: cassette.Response{
			StatusCode: 200,
			Headers:    map[string]string{"Content-Type": "text/event-stream"},
			SSEEvents:  events,
		},
	})

	// 1. Non-hijackable ResponseRecorder fallback emits deterministic stream_interrupted SSE error frame.
	inj1, err := chaos.NewInjector(chaos.Config{DropSSEAfter: 2})
	if err != nil {
		t.Fatal(err)
	}
	srv1, err := NewServer(Config{
		Mode:     ModeReplay,
		Cassette: cas,
		Chaos:    inj1,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"stream":true}`))
	srv1.httpServer.Handler.ServeHTTP(rec, httpReq)

	if rec.Header().Get("X-AgentFixture-Chaos") != "drop_sse" {
		t.Fatalf("expected X-AgentFixture-Chaos=drop_sse header, got %q", rec.Header().Get("X-AgentFixture-Chaos"))
	}
	decoded, decErr := cassette.DecodeSSE(bytes.NewReader(rec.Body.Bytes()))
	if decErr != nil {
		t.Fatalf("failed to decode fallback SSE events: %v", decErr)
	}
	if len(decoded) != 3 || decoded[2].Event != cassette.SSEEventError || !strings.Contains(decoded[2].Data, "stream_interrupted") {
		t.Fatalf("expected 2 events + stream_interrupted error frame on non-hijackable writer, got %+v", decoded)
	}
	if srv1.ChaosStats().SSEDropsInjected != 1 {
		t.Fatalf("expected SSEDropsInjected=1, got %+v", srv1.ChaosStats())
	}

	// 2. Client context canceled before dropSSEAfter completes -> must NOT falsely record SSE drop.
	inj2, err := chaos.NewInjector(chaos.Config{DropSSEAfter: 2})
	if err != nil {
		t.Fatal(err)
	}
	srv2, err := NewServer(Config{
		Mode:      ModeReplay,
		Cassette:  cas,
		Chaos:     inj2,
		SSETiming: cassette.SSETimingConfig{Mode: cassette.SSETimingExact, MaxChunkDelay: time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec2 := httptest.NewRecorder()
	httpReq2 := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"stream":true}`)).WithContext(ctx)
	srv2.httpServer.Handler.ServeHTTP(rec2, httpReq2)

	if srv2.ChaosStats().SSEDropsInjected != 0 {
		t.Fatalf("expected SSEDropsInjected=0 when client canceled before drop point, got %+v", srv2.ChaosStats())
	}
}

func TestHTTPProxy_NewEpisodes_SecretRedactionConcurrencyAndUpstreamValidation(t *testing.T) {
	// Malformed scheme-less upstream URL must be rejected cleanly.
	if _, err := NewServer(Config{
		Mode:        ModeReplay,
		RecordMode:  RecordModeNewEpisodes,
		UpstreamURL: "api.anthropic.com",
	}); err == nil {
		t.Fatalf("expected error for scheme-less UpstreamURL")
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "session=super-secret-cookie-value")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"reply":"ok","leaked":"sk-proj-1234567890abcdef1234567890","api_key":"inline-secret"}`))
	}))
	defer upstream.Close()

	srv, err := NewServer(Config{
		Mode:        ModeReplay,
		RecordMode:  RecordModeNewEpisodes,
		UpstreamURL: upstream.URL,
		Cassette:    cassette.NewCassette("cas_parallel_new_ep"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	const workers = 8
	var wg sync.WaitGroup
	client := &http.Client{Timeout: 5 * time.Second}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			body := `{"prompt":"parallel_ep_` + string(rune('A'+idx)) + `","api_key":"client-inline-secret"}`
			req, _ := http.NewRequest("POST", srv.URL()+"/v1/messages?key=AIzaSyDaGmWKa4JsXZ-HjGw7ISLn_3namBGewQe", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer sk-ant-api03-1234567890abcdef1234567890")
			resp, rErr := client.Do(req)
			if rErr != nil {
				t.Errorf("worker %d request failed: %v", idx, rErr)
				return
			}
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("worker %d status=%d, want 200", idx, resp.StatusCode)
			}
		}(i)
	}
	wg.Wait()

	cas := srv.GetCassette()
	if len(cas.Interactions) != workers {
		t.Fatalf("expected %d recorded interactions, got %d", workers, len(cas.Interactions))
	}
	if err := cas.Validate(); err != nil {
		t.Fatalf("recorded parallel cassette failed Validate(): %v", err)
	}
	if hasSec, reason := cassette.CheckForUnredactedSecrets(cas); hasSec {
		t.Fatalf("SECURITY FAILURE: unredacted secret found in new_episodes cassette BEFORE save: %s", reason)
	}

	outPath := filepath.Join(t.TempDir(), "hybrid_redacted.yaml")
	if err := cassette.Save(outPath, cas); err != nil {
		t.Fatalf("atomic cassette.Save failed: %v", err)
	}
	reloaded, err := cassette.Load(outPath)
	if err != nil {
		t.Fatalf("reloading saved cassette failed: %v", err)
	}
	if hasSec, reason := cassette.CheckForUnredactedSecrets(reloaded); hasSec {
		t.Fatalf("SECURITY FAILURE: unredacted secret persisted to disk: %s", reason)
	}
}

func TestTestServer_NewEpisodes_CreatesNonExistentCassette(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	casPath := filepath.Join(t.TempDir(), "brand_new_hybrid.yaml")
	ts := NewTestServer(t, casPath, WithTestHybridRecordMode(RecordModeNewEpisodes, upstream.URL))

	resp, err := ts.HTTPClient().Post(ts.URL()+"/v1/messages", "application/json", strings.NewReader(`{"prompt":"first"}`))
	if err != nil {
		t.Fatalf("request through NewTestServer failed: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ts.Server().NewEpisodesCount() != 1 {
		t.Fatalf("expected 1 new episode, got %d", ts.Server().NewEpisodesCount())
	}
}

func TestStdioReplayer_ProcessStreamContextCancelDuringChaosLatency(t *testing.T) {
	cas := cassette.NewCassette("cas_mcp_cancel")
	inj, err := chaos.NewInjector(chaos.Config{LatencyMs: 10000})
	if err != nil {
		t.Fatal(err)
	}
	replayer := NewStdioReplayer(cas, matcher.NewStrictMatcher())
	replayer.SetChaos(inj)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	start := time.Now()
	var out bytes.Buffer
	err = replayer.ProcessStreamContext(ctx, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n"), &out)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("ProcessStreamContext hung despite context cancel: elapsed=%v", elapsed)
	}
}
