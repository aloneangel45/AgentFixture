package test

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
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
