package test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
	"github.com/aloneangel45/AgentFixture/pkg/matcher"
	"github.com/aloneangel45/AgentFixture/pkg/proxy"
)

func TestSSEDeterminism_10xReplayAndZeroOutboundNetwork(t *testing.T) {
	var upstreamHits atomic.Int64

	// 1. Live mock Anthropic Messages API SSE upstream emitting thinking, citations, text, and tool_use JSON deltas
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		frames := []string{
			": keep-alive\n\n",
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_det_001\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-20250514\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":512,\"cache_creation_input_tokens\":64,\"cache_read_input_tokens\":128,\"output_tokens\":1}}}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\",\"signature\":\"\"}}\n\n",
			"event: ping\ndata: {\"type\":\"ping\"}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"Checking repository files \"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"for deterministic replay.\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig_deterministic_v1\"}}\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"Executing search_code tool now.\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"citations_delta\",\"citation\":{\"type\":\"char_location\",\"cited_text\":\"ReplaySSEStream\",\"document_index\":0,\"start_char_index\":0,\"end_char_index\":15}}}\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_det_99\",\"name\":\"search_code\",\"input\":{}}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"pattern\\\": \\\"ReplaySSE\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"Stream\\\", \\\"max_results\\\": 25}\"}}\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":2}\n\n",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":118}}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		}
		for _, f := range frames {
			_, _ = w.Write([]byte(f))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))

	// 2. Record the streaming interaction into a YAML cassette
	casPath := filepath.Join(t.TempDir(), "anthropic_sse_determinism.yaml")
	recordSrv, err := proxy.NewServer(proxy.Config{
		Mode:        proxy.ModeRecord,
		UpstreamURL: upstream.URL,
	})
	if err != nil {
		t.Fatalf("failed to create record server: %v", err)
	}
	if err := recordSrv.Start(); err != nil {
		t.Fatalf("failed to start record server: %v", err)
	}

	reqPayload := `{"model":"claude-sonnet-4-20250514","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"Search for ReplaySSEStream"}]}`
	recReq, _ := http.NewRequest("POST", recordSrv.URL()+"/v1/messages?beta=true", strings.NewReader(reqPayload))
	recReq.Header.Set("Content-Type", "application/json")
	recReq.Header.Set("X-Api-Key", "sk-ant-api03-liveSecretKeyForRecording123456")

	recResp, err := http.DefaultClient.Do(recReq)
	if err != nil {
		t.Fatalf("record request failed: %v", err)
	}
	liveWireBytes, err := io.ReadAll(recResp.Body)
	_ = recResp.Body.Close()
	if err != nil {
		t.Fatalf("failed reading live stream: %v", err)
	}

	if err := cassette.Save(casPath, recordSrv.GetCassette()); err != nil {
		t.Fatalf("failed saving cassette: %v", err)
	}
	_ = recordSrv.Close()

	// Permanently shut down upstream so any accidental network leak fails immediately
	upstream.Close()
	if upstreamHits.Load() != 1 {
		t.Fatalf("expected exactly 1 upstream hit during recording, got %d", upstreamHits.Load())
	}

	// 3. Reload cassette and verify zero unredacted secrets
	loadedCas, err := cassette.Load(casPath)
	if err != nil {
		t.Fatalf("failed loading cassette: %v", err)
	}
	if hasSec, reason := cassette.CheckForUnredactedSecrets(loadedCas); hasSec {
		t.Fatalf("unredacted secret found in saved SSE cassette: %s", reason)
	}

	// 4. Start deterministic offline replay server
	replaySrv, err := proxy.NewServer(proxy.Config{
		Mode:     proxy.ModeReplay,
		Cassette: loadedCas,
		Matcher:  matcher.NewStrictMatcher(),
		SSETiming: cassette.SSETimingConfig{
			Mode: cassette.SSETimingInstant,
		},
	})
	if err != nil {
		t.Fatalf("failed creating replay server: %v", err)
	}
	if err := replaySrv.Start(); err != nil {
		t.Fatalf("failed starting replay server: %v", err)
	}
	defer replaySrv.Close()

	offlineClient := proxy.NewOfflineHTTPClient(replaySrv.URL())

	// 5. Execute 10 consecutive replays and assert 100% byte-for-byte & semantic determinism
	var baselineBytes []byte
	var baselineSummary cassette.StreamSummary

	for run := 1; run <= 10; run++ {
		// Reorder JSON keys and pass dummy CI API key to verify canonical secret-safe matching simultaneously
		reorderedPayload := `{"stream":true,"messages":[{"content":"Search for ReplaySSEStream","role":"user"}],"max_tokens":1024,"model":"claude-sonnet-4-20250514"}`
		req, _ := http.NewRequest("POST", replaySrv.URL()+"/v1/messages?beta=true", strings.NewReader(reorderedPayload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Api-Key", "af-ci-dummy-key")

		resp, err := offlineClient.Do(req)
		if err != nil {
			t.Fatalf("replay #%d failed: %v", run, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("replay #%d status=%d, expected 200", run, resp.StatusCode)
		}
		if resp.Header.Get("X-AgentFixture-Replay") != "true" {
			t.Fatalf("replay #%d missing X-AgentFixture-Replay header", run)
		}

		bodyBytes, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("replay #%d body read failed: %v", run, err)
		}

		if !bytes.Equal(bodyBytes, liveWireBytes) {
			t.Fatalf("replay #%d wire bytes diverged from live recorded stream", run)
		}

		events, decErr := cassette.DecodeSSE(bytes.NewReader(bodyBytes))
		if decErr != nil {
			t.Fatalf("replay #%d DecodeSSE failed: %v", run, decErr)
		}
		summary := cassette.ReconstructStreamSummary(events)

		if run == 1 {
			baselineBytes = bodyBytes
			baselineSummary = summary
		} else {
			if !bytes.Equal(bodyBytes, baselineBytes) {
				t.Fatalf("replay #%d bytes differed from replay #1", run)
			}
			if !reflect.DeepEqual(summary, baselineSummary) {
				t.Fatalf("replay #%d StreamSummary differed from replay #1", run)
			}
		}
	}

	// Verify reconstructed fields
	if baselineSummary.Text != "Executing search_code tool now." {
		t.Errorf("unexpected reconstructed text: %q", baselineSummary.Text)
	}
	if len(baselineSummary.ThinkingBlocks) != 1 || baselineSummary.ThinkingBlocks[0] != "Checking repository files for deterministic replay." {
		t.Errorf("unexpected thinking blocks: %+v", baselineSummary.ThinkingBlocks)
	}
	if len(baselineSummary.ToolCalls) != 1 || baselineSummary.ToolCalls[0].Name != "search_code" ||
		baselineSummary.ToolCalls[0].Input["pattern"] != "ReplaySSEStream" ||
		baselineSummary.ToolCalls[0].Input["max_results"] != int64(25) {
		t.Errorf("unexpected reconstructed tool call: %+v", baselineSummary.ToolCalls)
	}
	if baselineSummary.Usage.InputTokens != 512 || baselineSummary.Usage.OutputTokens != 118 ||
		baselineSummary.Usage.CacheCreationInputTokens != 64 || baselineSummary.Usage.CacheReadInputTokens != 128 {
		t.Errorf("unexpected reconstructed usage: %+v", baselineSummary.Usage)
	}

	// 6. Verify Zero Outbound Network Dials during replay
	directOffline := proxy.NewOfflineHTTPClient("")
	if _, err := directOffline.Get("http://api.anthropic.com/v1/messages"); !errors.Is(err, proxy.ErrOutboundNetworkBlocked) {
		t.Fatalf("expected direct external dial to be blocked with ErrOutboundNetworkBlocked, got: %v", err)
	}
	if _, err := offlineClient.Get("https://api.anthropic.com/v1/messages"); err == nil {
		t.Fatalf("expected HTTPS CONNECT tunnel through replay proxy to be rejected")
	}
	if upstreamHits.Load() != 1 {
		t.Fatalf("expected zero upstream hits during 10x replay, total=%d", upstreamHits.Load())
	}
}
