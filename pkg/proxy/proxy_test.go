package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
	"github.com/aloneangel45/AgentFixture/pkg/matcher"
)

func TestProxy_ReplayMode_MatchingAndMismatch(t *testing.T) {
	cas := cassette.NewCassette("cas_proxy_test")
	cas.Interactions = append(cas.Interactions, cassette.Interaction{
		ID:       "int_001",
		Sequence: 1,
		Type:     cassette.InteractionTypeHTTP,
		Request: cassette.Request{
			Method:   "POST",
			Endpoint: "/v1/chat/completions",
			RawBody:  `{"model":"gpt-4o","prompt":"hello world"}`,
			Matcher: cassette.MatcherMeta{
				StrictHash: matcher.ComputeRequestHash(&cassette.Request{
					Method:   "POST",
					Endpoint: "/v1/chat/completions",
					RawBody:  `{"model":"gpt-4o","prompt":"hello world"}`,
				}),
			},
		},
		Response: cassette.Response{
			StatusCode: 200,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
			RawBody: `{"id":"chatcmpl-123","choices":[{"message":{"content":"Hello from cassette!"}}]}`,
		},
	})

	srv, err := NewServer(Config{
		Mode:     ModeReplay,
		Cassette: cas,
		Matcher:  matcher.NewStrictMatcher(),
	})
	if err != nil {
		t.Fatalf("failed to create proxy server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start proxy server: %v", err)
	}
	defer srv.Close()

	// 1. Send matching request
	reqBody := `{"model":"gpt-4o","prompt":"hello world"}`
	resp, err := http.Post(srv.URL()+"/v1/chat/completions", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK, got %d", resp.StatusCode)
	}
	if resp.Header.Get("X-AgentFixture-Replay") != "true" {
		t.Errorf("expected X-AgentFixture-Replay header")
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Hello from cassette!") {
		t.Errorf("unexpected body: %s", string(body))
	}
	if srv.MatchedCount() != 1 {
		t.Errorf("expected 1 matched interaction, got %d", srv.MatchedCount())
	}

	// 2. Send mismatch request
	mismatchBody := `{"model":"gpt-4o","prompt":"unrecorded prompt"}`
	resp2, err := http.Post(srv.URL()+"/v1/chat/completions", "application/json", strings.NewReader(mismatchBody))
	if err != nil {
		t.Fatalf("mismatch request failed: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 NotFound on mismatch, got %d", resp2.StatusCode)
	}
	if resp2.Header.Get("X-AgentFixture-Mismatch") != "true" {
		t.Errorf("expected X-AgentFixture-Mismatch header")
	}

	if len(srv.UnmatchedRequests()) != 1 {
		t.Errorf("expected 1 recorded unmatched request, got %d", len(srv.UnmatchedRequests()))
	}
}

func TestProxy_RecordMode(t *testing.T) {
	// Mock upstream service
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":"live upstream response"}`))
	}))
	defer upstream.Close()

	srv, err := NewServer(Config{
		Mode:        ModeRecord,
		UpstreamURL: upstream.URL,
	})
	if err != nil {
		t.Fatalf("failed to create recording proxy: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start recording proxy: %v", err)
	}
	defer srv.Close()

	reqBody := `{"query":"test"}`
	resp, err := http.Post(srv.URL()+"/api/v1/test", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("record request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK from recorded upstream, got %d", resp.StatusCode)
	}

	cas := srv.GetCassette()
	if len(cas.Interactions) != 1 {
		t.Fatalf("expected 1 interaction recorded, got %d", len(cas.Interactions))
	}
	if !strings.Contains(cas.Interactions[0].Response.RawBody, "live upstream response") {
		t.Errorf("unexpected recorded response: %s", cas.Interactions[0].Response.RawBody)
	}
}

func TestStdioReplayer_JSONRPC(t *testing.T) {
	cas := cassette.NewCassette("cas_stdio_test")
	cas.Interactions = append(cas.Interactions, cassette.Interaction{
		ID:       "tool_001",
		Sequence: 1,
		Type:     cassette.InteractionTypeTool,
		Request: cassette.Request{
			ToolName: "view_file",
			ToolInput: map[string]interface{}{
				"path": "README.md",
			},
		},
		Response: cassette.Response{
			StatusCode: 0,
			ToolResult: "# AgentFixture Documentation",
		},
	})

	replayer := NewStdioReplayer(cas, matcher.NewStrictMatcher())

	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"view_file","arguments":{"path":"README.md"}}}` + "\n"
	inBuf := strings.NewReader(input)
	var outBuf bytes.Buffer

	if err := replayer.ProcessStream(inBuf, &outBuf); err != nil {
		t.Fatalf("ProcessStream failed: %v", err)
	}

	var resp JSONRPCResponse
	if err := json.Unmarshal(outBuf.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal JSON-RPC output: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error response: %v", resp.Error)
	}

	resMap := resp.Result.(map[string]interface{})
	content := resMap["content"].([]interface{})
	first := content[0].(map[string]interface{})
	if first["text"] != "# AgentFixture Documentation" {
		t.Errorf("expected tool result text, got %v", first["text"])
	}
}
