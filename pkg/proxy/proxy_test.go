package proxy

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
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

func TestProxy_MultiTurnStatefulSequentialReplay(t *testing.T) {
	cas := cassette.NewCassette("cas_stateful")
	for i, content := range []string{"turn_1_state", "turn_2_state"} {
		cas.Interactions = append(cas.Interactions, cassette.Interaction{
			ID:       fmt.Sprintf("int_%02d", i+1),
			Sequence: i + 1,
			Type:     cassette.InteractionTypeHTTP,
			Request: cassette.Request{
				Method:   "GET",
				Endpoint: "/v1/state",
			},
			Response: cassette.Response{
				StatusCode: 200,
				RawBody:    content,
			},
		})
	}

	srv, err := NewServer(Config{Mode: ModeReplay, Cassette: cas})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	for _, want := range []string{"turn_1_state", "turn_2_state", "turn_1_state"} {
		resp, err := http.Get(srv.URL() + "/v1/state")
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if string(got) != want {
			t.Fatalf("expected %q, got %q", want, string(got))
		}
	}
	if srv.MatchedCount() != 2 {
		t.Errorf("expected both interactions consumed across 2 turns, got %d", srv.MatchedCount())
	}
}

func TestProxy_RecordMode_GzipAndBasePathJoin(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/base/v1/messages" {
			t.Errorf("expected joined path /base/v1/messages, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		var gzBuf bytes.Buffer
		gz := gzip.NewWriter(&gzBuf)
		_, _ = gz.Write([]byte(`{"result":"decompressed clean json","usage":{"input_tokens":15,"output_tokens":5}}`))
		_ = gz.Close()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(gzBuf.Bytes())
	}))
	defer upstream.Close()

	srv, err := NewServer(Config{
		Mode:        ModeRecord,
		UpstreamURL: upstream.URL + "/base",
	})
	if err != nil {
		t.Fatalf("failed to create recording proxy: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start recording proxy: %v", err)
	}
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL()+"/v1/messages", strings.NewReader(`{"query":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	req.Header.Set("Authorization", "Bearer sk-ant-api03-1234567890abcdef1234567890")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("record request failed: %v", err)
	}
	defer resp.Body.Close()

	cas := srv.GetCassette()
	if len(cas.Interactions) != 1 {
		t.Fatalf("expected 1 interaction recorded, got %d", len(cas.Interactions))
	}
	inter := cas.Interactions[0]
	if !strings.Contains(inter.Response.RawBody, "decompressed clean json") {
		t.Fatalf("expected decompressed plaintext JSON in cassette, got: %q", inter.Response.RawBody)
	}
	if _, hasCE := inter.Response.Headers["Content-Encoding"]; hasCE {
		t.Errorf("expected Content-Encoding header stripped from stored cassette headers")
	}
	if inter.Response.Usage.InputTokens != 15 || inter.Response.Usage.OutputTokens != 5 {
		t.Errorf("expected extracted usage 15/5, got %+v", inter.Response.Usage)
	}
	if hasSec, reason := cassette.CheckForUnredactedSecrets(cas); hasSec {
		t.Errorf("expected recorded cassette to be auto-redacted, got: %s", reason)
	}
}

func TestProxy_OfflineGuard_BlocksConnectAndExternalDial(t *testing.T) {
	srv, err := NewServer(Config{Mode: ModeReplay, Cassette: cassette.NewCassette("offline_test")})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	// 1. Direct HTTP CONNECT request must be rejected with 403 Forbidden
	connectReq, _ := http.NewRequest(http.MethodConnect, srv.URL(), nil)
	connectReq.Host = "api.anthropic.com:443"
	resp, err := http.DefaultClient.Do(connectReq)
	if err != nil {
		t.Fatalf("CONNECT request transport error: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden on HTTP CONNECT, got %d", resp.StatusCode)
	}

	// 2. Offline HTTP client without proxy blocks external TCP dials immediately
	offlineDirect := NewOfflineHTTPClient("")
	_, dialErr := offlineDirect.Get("http://example.com/test")
	if dialErr == nil || !errors.Is(dialErr, ErrOutboundNetworkBlocked) {
		t.Fatalf("expected ErrOutboundNetworkBlocked on external dial, got: %v", dialErr)
	}

	// 3. Offline HTTP client with replay proxy rejects HTTPS outbound tunnel
	offlineProxied := NewOfflineHTTPClient(srv.URL())
	_, httpsErr := offlineProxied.Get("https://api.anthropic.com/v1/messages")
	if httpsErr == nil {
		t.Fatalf("expected HTTPS outbound tunnel via replay proxy to be blocked")
	}
}

func TestStdioReplayer_ContentLengthNotificationsSynthesisAndRecorder(t *testing.T) {
	// 1. Record live MCP stdio session via StdioRecorder (in-memory streams)
	rec := NewStdioRecorder(cassette.NewCassette("mcp_rec_01"))
	serverInR, serverInW := io.Pipe()
	serverOutR, serverOutW := io.Pipe()

	hugeArg := strings.Repeat("X", 80*1024) // 80KB (>64KB default scanner buffer!)
	clientReq := fmt.Sprintf(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"analyze_blob","arguments":{"blob":%q,"api_key":"sk-ant-api03-1234567890abcdef1234567890"}}}`, hugeArg)
	framedReq := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(clientReq), clientReq)

	go func() {
		defer serverOutW.Close()
		br := NewStdioReplayer(nil, nil) // just to verify unused; read raw frame via helper
		_ = br
		// Read request arriving at mock MCP server and reply with Content-Length frame
		buf := make([]byte, len(framedReq))
		_, _ = io.ReadFull(serverInR, buf)
		reply := `{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":"blob_ok"}]}}`
		_, _ = fmt.Fprintf(serverOutW, "Content-Length: %d\r\n\r\n%s", len(reply), reply)
	}()

	var clientLiveOut bytes.Buffer
	recCas, err := rec.RecordStreams(strings.NewReader(framedReq), serverInW, serverOutR, &clientLiveOut)
	if err != nil {
		t.Fatalf("StdioRecorder.RecordStreams failed: %v", err)
	}
	if len(recCas.Interactions) != 1 {
		t.Fatalf("expected 1 recorded MCP interaction, got %d", len(recCas.Interactions))
	}
	if recCas.Interactions[0].Response.ToolResult != "blob_ok" {
		t.Errorf("expected recorded tool_result 'blob_ok', got %q", recCas.Interactions[0].Response.ToolResult)
	}
	if hasSec, reason := cassette.CheckForUnredactedSecrets(recCas); hasSec {
		t.Fatalf("unredacted secret in recorded MCP cassette: %s", reason)
	}

	// 2. Replay session testing: initialize + notification (ignored!) + tools/list + Content-Length tools/call + escaped error
	replayer := NewStdioReplayer(recCas, matcher.NewStrictMatcher())

	initLine := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n"
	notifLine := `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"
	listLine := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n"
	replayCallJSON := fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"analyze_blob","arguments":{"blob":%q,"api_key":"dummy"}}}`, hugeArg)
	replayCallFramed := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(replayCallJSON), replayCallJSON)
	badRawLine := `raw "quoted" unmatched line` + "\n"

	combinedInput := initLine + notifLine + listLine + replayCallFramed + badRawLine
	var replayOut bytes.Buffer
	if err := replayer.ProcessStream(strings.NewReader(combinedInput), &replayOut); err != nil {
		t.Fatalf("StdioReplayer.ProcessStream failed: %v", err)
	}

	outStr := replayOut.String()
	// Verify initialize synthesized, tools/list includes analyze_blob, Content-Length response emitted, and raw error is valid JSON
	if !strings.Contains(outStr, `"protocolVersion":"2024-11-05"`) {
		t.Errorf("expected synthesized initialize response, got: %s", outStr)
	}
	if !strings.Contains(outStr, `"analyze_blob"`) {
		t.Errorf("expected synthesized tools/list to expose analyze_blob, got: %s", outStr)
	}
	if !strings.Contains(outStr, "Content-Length:") || !strings.Contains(outStr, `"blob_ok"`) {
		t.Errorf("expected Content-Length framed tool response with blob_ok, got: %s", outStr)
	}
	if !strings.Contains(outStr, `\"quoted\"`) {
		t.Errorf("expected properly JSON-escaped quotes in fallback error output, got: %s", outStr)
	}
}
