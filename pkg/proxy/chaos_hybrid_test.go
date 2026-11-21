package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
