package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestHTTPProxy_RecordGeminiQueryKeyAndReplayWithDummySecrets(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("key") != "AIzaSyLiveSecretKey1234567890123456789" {
			http.Error(w, "missing live upstream key", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Gemini OK\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":12,\"candidatesTokenCount\":4,\"totalTokenCount\":16},\"modelVersion\":\"gemini-2.5-flash\"}\n\n")
	}))
	defer upstream.Close()

	recCas := cassette.NewCassette("cas_gem_proxy")
	recSrv, err := NewServer(Config{
		Mode:        ModeRecord,
		UpstreamURL: upstream.URL,
		Cassette:    recCas,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := recSrv.Start(); err != nil {
		t.Fatal(err)
	}

	reqBody := `{"contents":[{"role":"user","parts":[{"text":"Ping Gemini"}]}],"api_key":"live-inline-secret"}`
	liveResp, err := http.Post(
		recSrv.URL()+"/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse&key=AIzaSyLiveSecretKey1234567890123456789",
		"application/json",
		strings.NewReader(reqBody),
	)
	if err != nil {
		_ = recSrv.Close()
		t.Fatalf("live record request failed: %v", err)
	}
	_, _ = io.ReadAll(liveResp.Body)
	_ = liveResp.Body.Close()
	_ = recSrv.Close()

	savedCas := recSrv.GetCassette()
	if hasSec, reason := cassette.CheckForUnredactedSecrets(savedCas); hasSec {
		t.Fatalf("unredacted secret in recorded Gemini cassette: %s", reason)
	}
	if len(savedCas.Interactions) != 1 {
		t.Fatalf("expected 1 recorded interaction, got %d", len(savedCas.Interactions))
	}
	sig := savedCas.Interactions[0].Request.Matcher.FuzzySignature
	if sig.NormalizedPrompt != "user: Ping Gemini" || sig.Model != "gemini-2.5-flash" {
		t.Errorf("expected FuzzySignature populated (prompt='user: Ping Gemini', model='gemini-2.5-flash'), got %+v", sig)
	}

	// Replay with StrictMatcher using completely different CI dummy ?key= AND different inline api_key!
	replaySrv, err := NewServer(Config{
		Mode:     ModeReplay,
		Cassette: savedCas,
		Matcher:  matcher.NewStrictMatcher(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := replaySrv.Start(); err != nil {
		t.Fatal(err)
	}
	defer replaySrv.Close()

	ciBody := `{"contents":[{"role":"user","parts":[{"text":"Ping Gemini"}]}],"api_key":"ci-dummy-offline-secret"}`
	repResp, err := http.Post(
		replaySrv.URL()+"/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse&key=af-offline-replay-key",
		"application/json",
		strings.NewReader(ciBody),
	)
	if err != nil {
		t.Fatalf("replay request failed: %v", err)
	}
	repBytes, _ := io.ReadAll(repResp.Body)
	_ = repResp.Body.Close()
	if repResp.StatusCode != http.StatusOK || !strings.Contains(string(repBytes), "Gemini OK") {
		t.Fatalf("expected 200 OK Gemini SSE replay with dummy CI keys, got status=%d body=%s", repResp.StatusCode, string(repBytes))
	}
}

func TestStdioProxy_MIMEHeaderOrder64BitIDMCPIsErrorAndTruncatedEOF(t *testing.T) {
	// 1. Record MCP tool call with Content-Type BEFORE Content-Length, 64-bit ID (2^53 + 1), and isError: true response
	rec := NewStdioRecorder(cassette.NewCassette("mcp_edge_cases"))
	serverInR, serverInW := io.Pipe()
	serverOutR, serverOutW := io.Pipe()

	const bigID = "9007199254740993"
	reqJSON := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"tools/call","params":{"name":"run_sql","arguments":{"sql":"DROP TABLE prod"}}}`, bigID)
	// Put Content-Type header BEFORE Content-Length header!
	framedReq := fmt.Sprintf("Content-Type: application/vscode-jsonrpc; charset=utf-8\r\nContent-Length: %d\r\n\r\n%s", len(reqJSON), reqJSON)
	respJSON := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"permission denied: read-only role"}],"isError":true}}`, bigID)
	framedResp := fmt.Sprintf("Content-Type: application/vscode-jsonrpc; charset=utf-8\r\nContent-Length: %d\r\n\r\n%s", len(respJSON), respJSON)

	go func() {
		defer serverOutW.Close()
		buf := make([]byte, len(framedReq))
		_, _ = io.ReadFull(serverInR, buf)
		_, _ = io.WriteString(serverOutW, framedResp)
	}()

	var liveOut bytes.Buffer
	recCas, err := rec.RecordStreams(strings.NewReader(framedReq), serverInW, serverOutR, &liveOut)
	if err != nil {
		t.Fatalf("RecordStreams failed with Content-Type before Content-Length: %v", err)
	}
	if len(recCas.Interactions) != 1 {
		t.Fatalf("expected 1 recorded interaction matched across 64-bit ID %s, got %d", bigID, len(recCas.Interactions))
	}

	// Save & reload from YAML to ensure full round-trip fidelity of MCP isError: true
	casFile := filepath.Join(t.TempDir(), "mcp_is_error.yaml")
	if err := cassette.Save(casFile, recCas); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	loadedCas, err := cassette.Load(casFile)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	replayer := NewStdioReplayer(loadedCas, matcher.NewStrictMatcher())
	var replayOut bytes.Buffer
	if err := replayer.ProcessStream(strings.NewReader(framedReq), &replayOut); err != nil {
		t.Fatalf("StdioReplayer.ProcessStream failed: %v", err)
	}
	replayStr := replayOut.String()
	if !strings.Contains(replayStr, bigID) {
		t.Errorf("expected exact 64-bit JSON-RPC id %s preserved in replay output, got: %s", bigID, replayStr)
	}
	if !strings.Contains(replayStr, `"isError":true`) {
		t.Errorf("expected MCP isError:true preserved in replay output, got: %s", replayStr)
	}
	if replayer.MatchedCount() != 1 {
		t.Errorf("expected MatchedCount() == 1, got %d", replayer.MatchedCount())
	}

	// 2. Truncated Content-Length frame MUST return io.ErrUnexpectedEOF (not nil!)
	truncated := "Content-Length: 200\r\n\r\n{\"jsonrpc\":\"2.0\",\"id\":1"
	var truncOut bytes.Buffer
	errTrunc := replayer.ProcessStream(strings.NewReader(truncated), &truncOut)
	if !errors.Is(errTrunc, io.ErrUnexpectedEOF) {
		t.Fatalf("expected io.ErrUnexpectedEOF on truncated Content-Length frame, got: %v", errTrunc)
	}
}

func TestStdioProxy_NullIDBatchRawJSONCustomMIMEDeclassificationAndConcurrency(t *testing.T) {
	// 1. Record a batch of [tools/list (with params!), tools/call] starting with X-Request-Id header
	existingCas := cassette.NewCassette("mcp_v122")
	// Seed with mcp_004 to verify collision-free nextInteractionID -> mcp_005, mcp_006!
	existingCas.Interactions = append(existingCas.Interactions, cassette.Interaction{
		ID:       "mcp_004",
		Sequence: 4,
		Type:     cassette.InteractionTypeStdio,
		Request:  cassette.Request{Method: "ping"},
		Response: cassette.Response{Body: map[string]interface{}{}},
	})

	rec := NewStdioRecorder(existingCas)
	serverInR, serverInW := io.Pipe()
	serverOutR, serverOutW := io.Pipe()

	batchReq := `[{"jsonrpc":"2.0","id":10,"method":"tools/list","params":{"name":"should_not_become_tool_name"}},{"jsonrpc":"2.0","method":"notifications/progress","params":{"step":1}},{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"echo_tool","arguments":{"msg":"hi"}}}]`
	framedBatchReq := fmt.Sprintf("X-Request-Id: trace-abc-99\r\nContent-Type: application/vscode-jsonrpc; charset=utf-8\r\nContent-Length: %d\r\n\r\n%s", len(batchReq), batchReq)

	batchResp := `[{"jsonrpc":"2.0","id":10,"result":{"tools":[{"name":"echo_tool"}]}},{"jsonrpc":"2.0","id":11,"result":{"content":[{"type":"text","text":"echo: hi"}]}}]`
	framedBatchResp := fmt.Sprintf("X-Trace-Id: srv-1\r\nContent-Length: %d\r\n\r\n%s", len(batchResp), batchResp)

	go func() {
		defer serverOutW.Close()
		buf := make([]byte, len(framedBatchReq))
		_, _ = io.ReadFull(serverInR, buf)
		_, _ = io.WriteString(serverOutW, framedBatchResp)
	}()

	var liveClientOut bytes.Buffer
	recCas, err := rec.RecordStreams(strings.NewReader(framedBatchReq), serverInW, serverOutR, &liveClientOut)
	if err != nil {
		t.Fatalf("RecordStreams batch with X-Request-Id MIME header failed: %v", err)
	}
	if err := recCas.Validate(); err != nil {
		t.Fatalf("expected collision-free IDs passing Validate(), got: %v", err)
	}
	if len(recCas.Interactions) != 3 {
		t.Fatalf("expected 3 total interactions (1 seeded + 2 recorded from batch), got %d", len(recCas.Interactions))
	}
	if recCas.Interactions[1].ID != "mcp_005" || recCas.Interactions[2].ID != "mcp_006" {
		t.Fatalf("expected collision-free IDs mcp_005 & mcp_006 after mcp_004, got %s & %s",
			recCas.Interactions[1].ID, recCas.Interactions[2].ID)
	}
	// Verify tools/list was NOT misclassified as InteractionTypeTool!
	if recCas.Interactions[1].Type != cassette.InteractionTypeStdio || recCas.Interactions[1].Request.ToolName != "" {
		t.Fatalf("tools/list must be InteractionTypeStdio with empty ToolName, got type=%s toolName=%q",
			recCas.Interactions[1].Type, recCas.Interactions[1].Request.ToolName)
	}
	if recCas.Interactions[2].Type != cassette.InteractionTypeTool || recCas.Interactions[2].Request.ToolName != "echo_tool" {
		t.Fatalf("tools/call must be InteractionTypeTool with ToolName='echo_tool', got %+v", recCas.Interactions[2])
	}

	// Add an interaction with Body == nil && valid JSON RawBody to verify zero double-encoding!
	rawJSONReq := cassette.Request{
		Method:  "resources/list",
		RawBody: `{}`,
	}
	rawJSONReq.Matcher.StrictHash = matcher.ComputeRequestHash(&rawJSONReq)
	recCas.Interactions = append(recCas.Interactions, cassette.Interaction{
		ID:       "mcp_007",
		Sequence: 7,
		Type:     cassette.InteractionTypeStdio,
		Request:  rawJSONReq,
		Response: cassette.Response{
			RawBody: `{"resources":[{"uri":"file:///README.md","count":2}]}`,
		},
	})

	replayer := NewStdioReplayer(recCas, matcher.NewStrictMatcher())

	// 2a. Omitted "id" (notification) -> zero bytes written; Explicit "id": null -> -32600 error with "id": null!
	notifAndNullInput := strings.Join([]string{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":null,"method":"tools/list"}`,
		`[]`,
		`[{"jsonrpc":"2.0","method":"notifications/only"}]`,
		`{"jsonrpc":"2.0","id":99,"method":"resources/list","params":{}}`,
	}, "\n") + "\n"

	var outBuf bytes.Buffer
	if err := replayer.ProcessStream(strings.NewReader(notifAndNullInput), &outBuf); err != nil {
		t.Fatalf("ProcessStream failed: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(outBuf.String()), "\n")
	// Expect exactly 3 lines: (1) explicit id:null -32600, (2) empty [] -32600, (3) id:99 resources/list structured JSON!
	// Both notifications (single and notification-only batch) MUST emit zero lines!
	if len(lines) != 3 {
		t.Fatalf("expected exactly 3 response lines (notifications silent), got %d:\n%s", len(lines), outBuf.String())
	}

	var nullIDResp map[string]interface{}
	if err := json.Unmarshal([]byte(lines[0]), &nullIDResp); err != nil {
		t.Fatalf("invalid JSON on explicit null id response: %v (%s)", err, lines[0])
	}
	if _, hasIDKey := nullIDResp["id"]; !hasIDKey || nullIDResp["id"] != nil {
		t.Errorf("expected explicit \"id\": null in -32600 response, got: %s", lines[0])
	}
	if errObj, _ := nullIDResp["error"].(map[string]interface{}); errObj == nil || errObj["code"] != float64(-32600) {
		t.Errorf("expected error.code == -32600 on explicit null id request, got: %s", lines[0])
	}

	var emptyBatchResp map[string]interface{}
	if err := json.Unmarshal([]byte(lines[1]), &emptyBatchResp); err != nil {
		t.Fatalf("invalid JSON on empty batch [] response: %v (%s)", err, lines[1])
	}
	if errObj, _ := emptyBatchResp["error"].(map[string]interface{}); errObj == nil || errObj["code"] != float64(-32600) {
		t.Errorf("expected -32600 on empty batch [], got: %s", lines[1])
	}

	// Verify RawBody JSON was NOT double-encoded as a string literal!
	var resListResp struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Result  struct {
			Resources []struct {
				URI   string `json:"uri"`
				Count int    `json:"count"`
			} `json:"resources"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[2]), &resListResp); err != nil {
		t.Fatalf("RawBody JSON was double-encoded or invalid instead of JSON object: %v\nLine: %s", err, lines[2])
	}
	if len(resListResp.Result.Resources) != 1 || resListResp.Result.Resources[0].URI != "file:///README.md" {
		t.Errorf("unexpected decoded RawBody result: %+v", resListResp)
	}

	// 2b. Replay batch request framed with leading X-Request-Id header + concurrent goroutines
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var bOut bytes.Buffer
			if pErr := replayer.ProcessStream(strings.NewReader(framedBatchReq), &bOut); pErr != nil {
				t.Errorf("concurrent batch ProcessStream failed: %v", pErr)
				return
			}
			s := bOut.String()
			if !strings.HasPrefix(s, "Content-Length:") || !strings.Contains(s, `"echo: hi"`) {
				t.Errorf("unexpected batch replay frame: %s", s)
			}
		}()
	}
	wg.Wait()
}

func TestHTTPProxy_AbortedSSEStreamNotRecordedAndCollisionFreeIDs(t *testing.T) {
	mode := "abort_mid_stream"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "abort_mid_stream":
			// Hijack TCP socket to send HTTP 200 chunked SSE header + partial frame, then close abruptly WITHOUT terminal 0-length chunk!
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("ResponseWriter does not support Hijacker")
				return
			}
			conn, buf, err := hj.Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			part := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"
			_, _ = fmt.Fprintf(buf, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n", len(part), part)
			_ = buf.Flush()
			// Abruptly close socket without 0\r\n\r\n chunk terminator -> triggers io.ErrUnexpectedEOF!
		case "slow_stream":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if fl, ok := w.(http.Flusher); ok {
				_, _ = fmt.Fprint(w, "event: ping\ndata: {}\n\n")
				fl.Flush()
			}
			<-r.Context().Done()
		case "clean_json":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer upstream.Close()

	// Seed cassette with int_005 (only 1 element, so naive len+1=2 would collide if int_002 existed or produce out-of-order ID!)
	seeded := cassette.NewCassette("cas_sse_abort_and_ids")
	seeded.Interactions = []cassette.Interaction{
		{
			ID:       "int_002",
			Sequence: 2,
			Type:     cassette.InteractionTypeHTTP,
			Request:  cassette.Request{Method: "GET", Endpoint: "/seed"},
			Response: cassette.Response{StatusCode: 200, RawBody: "seed"},
		},
	}

	srv, err := NewServer(Config{
		Mode:        ModeRecord,
		UpstreamURL: upstream.URL,
		Cassette:    seeded,
	})
	if err != nil {
		t.Fatal(err)
	}
	if srv.httpServer.ReadHeaderTimeout != 15*time.Second {
		t.Errorf("expected ReadHeaderTimeout=15s, got %v", srv.httpServer.ReadHeaderTimeout)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	// 1. Upstream drops TCP mid-SSE stream (chunked framing truncated -> ErrUnexpectedEOF): MUST NOT be recorded!
	if resp, reqErr := http.Get(srv.URL() + "/v1/messages"); reqErr == nil {
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	if got := len(srv.GetCassette().Interactions); got != 1 {
		t.Fatalf("expected truncated upstream SSE stream NOT to be recorded (interactions=%d, want 1)", got)
	}

	// 2. Client cancels context mid-SSE stream: MUST NOT be recorded!
	mode = "slow_stream"
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	reqSlow, _ := http.NewRequestWithContext(ctx, "POST", srv.URL()+"/v1/messages", strings.NewReader(`{"stream":true}`))
	if resp, reqErr := http.DefaultClient.Do(reqSlow); reqErr == nil {
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	cancel()
	if got := len(srv.GetCassette().Interactions); got != 1 {
		t.Fatalf("expected client-cancelled SSE stream NOT to be recorded (interactions=%d, want 1)", got)
	}

	// 3. Clean HTTP request appends collision-free int_003 (NOT duplicate int_002!)
	mode = "clean_json"
	respOK, err := http.Get(srv.URL() + "/v1/ok")
	if err != nil {
		t.Fatalf("clean request failed: %v", err)
	}
	_, _ = io.ReadAll(respOK.Body)
	_ = respOK.Body.Close()

	finalCas := srv.GetCassette()
	if err := finalCas.Validate(); err != nil {
		t.Fatalf("expected collision-free cassette to pass Validate(), got: %v", err)
	}
	if len(finalCas.Interactions) != 2 || finalCas.Interactions[1].ID != "int_003" || finalCas.Interactions[1].Sequence != 3 {
		t.Fatalf("expected second interaction to have ID='int_003' Sequence=3, got %+v", finalCas.Interactions[1])
	}

	// 4. Verify readBoundedBody rejects payloads exceeding limit
	_, bErr := readBoundedBody(strings.NewReader("0123456789"), 5)
	if !errors.Is(bErr, errBodyTooLarge) {
		t.Errorf("expected errBodyTooLarge from readBoundedBody, got %v", bErr)
	}
}
