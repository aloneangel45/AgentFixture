package test

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
	"github.com/aloneangel45/AgentFixture/pkg/importer"
	"github.com/aloneangel45/AgentFixture/pkg/matcher"
	"github.com/aloneangel45/AgentFixture/pkg/proxy"
)

func TestEndToEnd_TracePortImportAndReplayLifecycle(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Realistic TracePort multi-step flight recording
	traceSession := `{
		"id": "trace_flight_e2e_889900",
		"command": "claude code 'Implement rate limiter'",
		"start_time": "2026-11-05T20:00:00Z",
		"status": "completed",
		"frames": [
			{
				"index": 1,
				"timestamp": "2026-11-05T20:00:01Z",
				"type": "checkpoint",
				"git_snapshot": "9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b"
			},
			{
				"index": 2,
				"timestamp": "2026-11-05T20:00:02Z",
				"type": "input",
				"content": "Please implement token bucket rate limiter in Go."
			},
			{
				"index": 3,
				"timestamp": "2026-11-05T20:00:03Z",
				"type": "output",
				"content": "I will inspect pkg/limiter/limiter.go first."
			},
			{
				"index": 4,
				"timestamp": "2026-11-05T20:00:04Z",
				"type": "tool_call",
				"tool": {
					"name": "view_file",
					"arguments": {
						"path": "pkg/limiter/limiter.go"
					},
					"result": "package limiter\n\ntype Limiter struct {}",
					"duration_ms": 35
				}
			}
		]
	}`

	traceFile := filepath.Join(tmpDir, "flight.json")
	if err := os.WriteFile(traceFile, []byte(traceSession), 0644); err != nil {
		t.Fatalf("failed to write flight file: %v", err)
	}

	// 2. Import into AgentFixture cassette
	casFile := filepath.Join(tmpDir, "cassette.yaml")
	cas, err := importer.ImportTracePortFile(traceFile, casFile)
	if err != nil {
		t.Fatalf("failed to import TracePort flight: %v", err)
	}

	if len(cas.Interactions) != 2 {
		t.Fatalf("expected 2 interactions imported, got %d", len(cas.Interactions))
	}

	// 3. Start deterministic proxy replay server
	srv, err := proxy.NewServer(proxy.Config{
		Mode:     proxy.ModeReplay,
		Cassette: cas,
		Matcher:  matcher.NewStrictMatcher(),
	})
	if err != nil {
		t.Fatalf("failed to create replay server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start replay server: %v", err)
	}
	defer srv.Close()

	// 4. Exercise Stdio tool call replay
	stdioReplayer := proxy.NewStdioReplayer(cas, matcher.NewStrictMatcher())
	toolCallJSON := `{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{"name":"view_file","arguments":{"path":"pkg/limiter/limiter.go"}}}` + "\n"
	var stdioOut bytes.Buffer
	if err := stdioReplayer.ProcessStream(strings.NewReader(toolCallJSON), &stdioOut); err != nil {
		t.Fatalf("stdio replayer failed: %v", err)
	}

	var rpcResp proxy.JSONRPCResponse
	if err := json.Unmarshal(stdioOut.Bytes(), &rpcResp); err != nil {
		t.Fatalf("failed to unmarshal JSON-RPC replay output: %v", err)
	}
	if rpcResp.Error != nil {
		t.Fatalf("unexpected RPC error: %v", rpcResp.Error)
	}

	// 5. Verify reload from disk
	reloaded, err := cassette.Load(casFile)
	if err != nil {
		t.Fatalf("failed to reload saved cassette: %v", err)
	}
	if reloaded.Metadata.CassetteID != "trace_flight_e2e_889900" {
		t.Errorf("expected cassette ID trace_flight_e2e_889900, got %s", reloaded.Metadata.CassetteID)
	}
	if reloaded.Metadata.Tags["git_snapshot"] != "9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b" {
		t.Errorf("expected git_snapshot tag preserved, got %s", reloaded.Metadata.Tags["git_snapshot"])
	}
}

func TestEndToEnd_LiveRecordAndReplayHTTP(t *testing.T) {
	// Simulated upstream LLM server
	upstream := proxyUpstreamMock()
	defer upstream.Close()

	tmpDir := t.TempDir()
	casPath := filepath.Join(tmpDir, "recorded_chat.yaml")

	// Phase 1: Record live interaction
	recordSrv, err := proxy.NewServer(proxy.Config{
		Mode:        proxy.ModeRecord,
		UpstreamURL: upstream.URL,
	})
	if err != nil {
		t.Fatalf("failed to create recording server: %v", err)
	}
	if err := recordSrv.Start(); err != nil {
		t.Fatalf("failed to start recording server: %v", err)
	}

	client := &http.Client{}
	reqBody := `{"model":"claude-3-7-sonnet","prompt":"hello"}`
	resp, err := client.Post(recordSrv.URL()+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("recording request failed: %v", err)
	}
	respBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if !strings.Contains(string(respBytes), "claude-mock-answer") {
		t.Errorf("expected upstream mock response, got: %s", string(respBytes))
	}

	recCas := recordSrv.GetCassette()
	if err := cassette.Save(casPath, recCas); err != nil {
		t.Fatalf("failed to save recorded cassette: %v", err)
	}
	_ = recordSrv.Close()

	// Phase 2: Replay without touching upstream (upstream closed)
	upstream.Close()

	replayCas, err := cassette.Load(casPath)
	if err != nil {
		t.Fatalf("failed to reload cassette: %v", err)
	}

	replaySrv, err := proxy.NewServer(proxy.Config{
		Mode:     proxy.ModeReplay,
		Cassette: replayCas,
		Matcher:  matcher.NewStrictMatcher(),
	})
	if err != nil {
		t.Fatalf("failed to create replay server: %v", err)
	}
	if err := replaySrv.Start(); err != nil {
		t.Fatalf("failed to start replay server: %v", err)
	}
	defer replaySrv.Close()

	// Send same request to replay server
	replayResp, err := client.Post(replaySrv.URL()+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("replay request failed: %v", err)
	}
	replayBody, _ := io.ReadAll(replayResp.Body)
	_ = replayResp.Body.Close()

	if replayResp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200 from replay server, got %d", replayResp.StatusCode)
	}
	if !strings.Contains(string(replayBody), "claude-mock-answer") {
		t.Errorf("expected replayed body, got: %s", string(replayBody))
	}
	if replayResp.Header.Get("X-AgentFixture-Replay") != "true" {
		t.Errorf("expected X-AgentFixture-Replay header")
	}
}

func proxyUpstreamMock() *proxyMockServer {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_123","role":"assistant","content":"claude-mock-answer"}`))
	})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	s := &http.Server{Handler: mux}
	go func() { _ = s.Serve(l) }()
	return &proxyMockServer{
		URL:    "http://" + l.Addr().String(),
		server: s,
	}
}

type proxyMockServer struct {
	URL    string
	server *http.Server
}

func (p *proxyMockServer) Close() {
	_ = p.server.Close()
}

func TestEndToEnd_GeminiSSERecordReplayAndMetrics(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		chunks := []string{
			"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Analyzing repo...\",\"thought\":true}]}}],\"modelVersion\":\"gemini-2.5-pro\",\"responseId\":\"gem_e2e_1\"}\r\n\r\n",
			"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Running tool.\"},{\"functionCall\":{\"id\":\"call_gem_1\",\"name\":\"list_files\",\"args\":{\"dir\":\"pkg\"}}},{\"thoughtSignature\":\"sig_gem_e2e\"}]}}],\"modelVersion\":\"gemini-2.5-pro\",\"responseId\":\"gem_e2e_1\"}\r\n\r\n",
			"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":220,\"candidatesTokenCount\":35,\"thoughtsTokenCount\":25,\"cachedContentTokenCount\":40,\"totalTokenCount\":280},\"modelVersion\":\"gemini-2.5-pro\",\"responseId\":\"gem_e2e_1\"}\r\n\r\n",
		}
		for _, ch := range chunks {
			_, _ = w.Write([]byte(ch))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))

	casPath := filepath.Join(t.TempDir(), "gemini_e2e.yaml")
	recordSrv, err := proxy.NewServer(proxy.Config{
		Mode:        proxy.ModeRecord,
		UpstreamURL: upstream.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := recordSrv.Start(); err != nil {
		t.Fatal(err)
	}

	reqBody := `{"contents":[{"role":"user","parts":[{"text":"List files in pkg"}]}]}`
	recReq, _ := http.NewRequest("POST", recordSrv.URL()+"/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse&key=AIzaSyDaGmWKa4JsXZ-HjGw7ISLn_3namBGewQe", strings.NewReader(reqBody))
	recReq.Header.Set("Content-Type", "application/json")
	recReq.Header.Set("x-goog-api-key", "AIzaSyDaGmWKa4JsXZ-HjGw7ISLn_3namBGewQe")

	recResp, err := http.DefaultClient.Do(recReq)
	if err != nil {
		t.Fatalf("Gemini record request failed: %v", err)
	}
	_, _ = io.ReadAll(recResp.Body)
	_ = recResp.Body.Close()

	if err := cassette.Save(casPath, recordSrv.GetCassette()); err != nil {
		t.Fatalf("failed saving Gemini cassette: %v", err)
	}
	_ = recordSrv.Close()
	upstream.Close()

	loaded, err := cassette.Load(casPath)
	if err != nil {
		t.Fatalf("failed loading Gemini cassette: %v", err)
	}
	if hasSec, reason := cassette.CheckForUnredactedSecrets(loaded); hasSec {
		t.Fatalf("expected Google API keys in header & URL query to be redacted, got: %s", reason)
	}
	if loaded.Metadata.Model != "gemini-2.5-pro" {
		t.Errorf("expected cassette metadata model 'gemini-2.5-pro', got %q", loaded.Metadata.Model)
	}
	if loaded.Metadata.Metrics.EstimatedTokensSaved != 280 {
		t.Errorf("expected 280 Gemini tokens saved, got %d", loaded.Metadata.Metrics.EstimatedTokensSaved)
	}

	replaySrv, err := proxy.NewServer(proxy.Config{
		Mode:      proxy.ModeReplay,
		Cassette:  loaded,
		Matcher:   matcher.NewStrictMatcher(),
		SSETiming: cassette.SSETimingConfig{Mode: cassette.SSETimingInstant},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := replaySrv.Start(); err != nil {
		t.Fatal(err)
	}
	defer replaySrv.Close()

	offlineClient := proxy.NewOfflineHTTPClient(replaySrv.URL())
	repReq, _ := http.NewRequest("POST", replaySrv.URL()+"/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse&key=AIzaSyAgentFixtureOfflineReplayKey00000", strings.NewReader(reqBody))
	repReq.Header.Set("Content-Type", "application/json")
	repReq.Header.Set("x-goog-api-key", "AIzaSyAgentFixtureOfflineReplayKey00000")

	repResp, err := offlineClient.Do(repReq)
	if err != nil {
		t.Fatalf("Gemini offline replay failed: %v", err)
	}
	defer repResp.Body.Close()
	if repResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on Gemini replay, got %d", repResp.StatusCode)
	}

	events, err := cassette.DecodeSSE(repResp.Body)
	if err != nil {
		t.Fatalf("DecodeSSE failed: %v", err)
	}
	summary := cassette.ReconstructStreamSummary(events)
	if summary.Provider != "gemini" || summary.StopReason != "STOP" || len(summary.ToolCalls) != 1 || summary.ToolCalls[0].Name != "list_files" {
		t.Fatalf("unexpected replayed Gemini StreamSummary: %+v", summary)
	}
	if summary.Usage.InputTokens != 220 || summary.Usage.OutputTokens != 60 || summary.Usage.CacheReadInputTokens != 40 || summary.Usage.TotalTokens != 280 {
		t.Fatalf("unexpected replayed Gemini usage: %+v", summary.Usage)
	}
}
