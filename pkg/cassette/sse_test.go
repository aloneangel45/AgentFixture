package cassette

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type testFlusher struct {
	buf     bytes.Buffer
	flushes atomic.Int64
}

func (tf *testFlusher) Write(p []byte) (int, error) {
	return tf.buf.Write(p)
}

func (tf *testFlusher) Flush() {
	tf.flushes.Add(1)
}

const sampleAnthropicSSE = `: keep-alive

event: message_start
data: {"type":"message_start","message":{"id":"msg_019X7z","type":"message","role":"assistant","model":"claude-sonnet-4-20250514","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":412,"cache_creation_input_tokens":100,"cache_read_input_tokens":50,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}

event: ping
data: {"type":"ping"}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me inspect "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"pkg/auth/token.go first."}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig_part_1_"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig_part_2"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"I will read "}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"the token validator."}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"citations_delta","citation":{"type":"char_location","cited_text":"ValidateToken","document_index":0,"start_char_index":10,"end_char_index":23}}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_01ABC","name":"read_file","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"path\": \"pkg/auth/"}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"token.go\", \"limit\": 9007199254740993}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":94}}

event: message_stop
data: {"type":"message_stop"}

`

func TestSSE_AnthropicDecodeEncodeAndSummary(t *testing.T) {
	events, err := DecodeSSE(strings.NewReader(sampleAnthropicSSE))
	if err != nil {
		t.Fatalf("DecodeSSE failed: %v", err)
	}
	if len(events) != 20 {
		t.Fatalf("expected 20 SSE events, got %d", len(events))
	}
	if events[0].Comment != "keep-alive" {
		t.Errorf("expected first event comment 'keep-alive', got %q", events[0].Comment)
	}

	summary := ReconstructStreamSummary(events)
	if summary.Provider != "anthropic" {
		t.Errorf("expected provider 'anthropic', got %q", summary.Provider)
	}
	if summary.MessageID != "msg_019X7z" {
		t.Errorf("expected message_id 'msg_019X7z', got %q", summary.MessageID)
	}
	if summary.Model != "claude-sonnet-4-20250514" {
		t.Errorf("expected model 'claude-sonnet-4-20250514', got %q", summary.Model)
	}
	if summary.Text != "I will read the token validator." {
		t.Errorf("unexpected reconstructed text: %q", summary.Text)
	}
	if len(summary.ThinkingBlocks) != 1 || summary.ThinkingBlocks[0] != "Let me inspect pkg/auth/token.go first." {
		t.Errorf("unexpected thinking blocks: %+v", summary.ThinkingBlocks)
	}
	if len(summary.Signatures) != 1 || summary.Signatures[0] != "sig_part_1_sig_part_2" {
		t.Errorf("unexpected signatures: %+v", summary.Signatures)
	}
	if len(summary.Citations) != 1 {
		t.Errorf("expected 1 citation, got %d", len(summary.Citations))
	}
	if len(summary.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(summary.ToolCalls))
	}
	tc := summary.ToolCalls[0]
	if tc.ID != "toolu_01ABC" || tc.Name != "read_file" {
		t.Errorf("unexpected tool call metadata: %+v", tc)
	}
	if tc.Input["path"] != "pkg/auth/token.go" {
		t.Errorf("unexpected tool input path: %v", tc.Input["path"])
	}
	// Verify 64-bit integer precision preserved via UseNumber
	if tc.Input["limit"] != int64(9007199254740993) {
		t.Errorf("expected exact int64 9007199254740993, got %T(%v)", tc.Input["limit"], tc.Input["limit"])
	}
	if summary.StopReason != "tool_use" {
		t.Errorf("expected stop_reason 'tool_use', got %q", summary.StopReason)
	}
	if summary.Usage.InputTokens != 412 || summary.Usage.OutputTokens != 94 ||
		summary.Usage.CacheCreationInputTokens != 100 || summary.Usage.CacheReadInputTokens != 50 {
		t.Errorf("unexpected usage metrics: %+v", summary.Usage)
	}

	// Verify wire round-trip
	encoded := EncodeSSEStream(events)
	redecoded, err := DecodeSSE(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("re-decode failed: %v", err)
	}
	if len(redecoded) != len(events) {
		t.Fatalf("expected %d events after round-trip, got %d", len(events), len(redecoded))
	}
	for i := range events {
		if redecoded[i] != events[i] {
			t.Errorf("event %d differed after round-trip: expected %+v, got %+v", i, events[i], redecoded[i])
		}
	}
}

func TestSSE_OpenAIStreamCompatibility(t *testing.T) {
	openAISSE := strings.Join([]string{
		`data: {"id":"chatcmpl-99","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello "}}]}`,
		"",
		`data: {"id":"chatcmpl-99","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"world!"}}]}`,
		"",
		`data: {"id":"chatcmpl-99","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"run_sql","arguments":"{\"q\":"}}]}}]}`,
		"",
		`data: {"id":"chatcmpl-99","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"SELECT 1\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":30,"completion_tokens":12,"total_tokens":42}}`,
		"",
		`data: [DONE]`,
		"",
	}, "\r\n")

	events, err := DecodeSSE(strings.NewReader(openAISSE))
	if err != nil {
		t.Fatalf("DecodeSSE failed on OpenAI CRLF stream: %v", err)
	}
	if len(events) != 5 {
		t.Fatalf("expected 5 OpenAI SSE events, got %d", len(events))
	}

	summary := ReconstructStreamSummary(events)
	if summary.Provider != "openai" {
		t.Errorf("expected provider openai, got %q", summary.Provider)
	}
	if summary.Text != "Hello world!" {
		t.Errorf("expected 'Hello world!', got %q", summary.Text)
	}
	if len(summary.ToolCalls) != 1 || summary.ToolCalls[0].Name != "run_sql" || summary.ToolCalls[0].Input["q"] != "SELECT 1" {
		t.Errorf("unexpected OpenAI tool call summary: %+v", summary.ToolCalls)
	}
	if summary.Usage.InputTokens != 30 || summary.Usage.OutputTokens != 12 || summary.Usage.TotalTokens != 42 {
		t.Errorf("unexpected OpenAI usage: %+v", summary.Usage)
	}
}

func TestSSE_LargeLineExceeding64KBScannerLimit(t *testing.T) {
	hugeText := strings.Repeat("A", 200*1024) // 200 KB single line (> 64KB bufio.Scanner limit)
	stream := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"" + hugeText + "\"}}\n\n"

	var flushed int
	events, err := RecordSSEStream(strings.NewReader(stream), func(ev SSEEvent) error {
		flushed++
		return nil
	})
	if err != nil {
		t.Fatalf("RecordSSEStream failed on >64KB line: %v", err)
	}
	if len(events) != 1 || flushed != 1 {
		t.Fatalf("expected 1 event flushed live, got len=%d flushed=%d", len(events), flushed)
	}
	summary := ReconstructStreamSummary(events)
	if len(summary.Text) != 200*1024 {
		t.Errorf("expected 204800 bytes reconstructed, got %d", len(summary.Text))
	}
}

func TestSSE_ReplayTimingModesAndContextCancel(t *testing.T) {
	events := []SSEEvent{
		{Index: 0, Event: SSEEventMessageStart, Data: `{"type":"message_start"}`, DeltaMs: 40},
		{Index: 1, Event: SSEEventContentBlockDelta, Data: `{"type":"content_block_delta"}`, DeltaMs: 40},
		{Index: 2, Event: SSEEventMessageStop, Data: `{"type":"message_stop"}`, DeltaMs: 40},
	}

	// 1. Instant mode (~0ms)
	tf := &testFlusher{}
	start := time.Now()
	if err := ReplaySSEStream(context.Background(), tf, tf, events, SSETimingConfig{Mode: SSETimingInstant}); err != nil {
		t.Fatalf("instant replay failed: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("instant replay took too long: %v", elapsed)
	}
	if tf.flushes.Load() != 3 {
		t.Errorf("expected 3 flushes, got %d", tf.flushes.Load())
	}

	// 2. Scaled mode with MaxChunkDelay cap
	tfScaled := &testFlusher{}
	startScaled := time.Now()
	err := ReplaySSEStream(context.Background(), tfScaled, tfScaled, events, SSETimingConfig{
		Mode:          SSETimingScaled,
		Scale:         0.25, // 40ms * 0.25 = 10ms per event (~30ms total)
		MaxChunkDelay: 15 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("scaled replay failed: %v", err)
	}
	if elapsed := time.Since(startScaled); elapsed < 20*time.Millisecond {
		t.Errorf("expected scaled replay delay >= 20ms, got %v", elapsed)
	}

	// 3. Context cancellation mid-stream without blocking
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	longEvents := []SSEEvent{
		{Index: 0, Event: SSEEventPing, Data: `{"type":"ping"}`, DeltaMs: 5000},
	}
	startCancel := time.Now()
	err = ReplaySSEStream(ctx, &bytes.Buffer{}, nil, longEvents, SSETimingConfig{Mode: SSETimingExact})
	if err == nil {
		t.Fatalf("expected context deadline error during exact replay")
	}
	if elapsed := time.Since(startCancel); elapsed > 250*time.Millisecond {
		t.Errorf("expected prompt cancellation on ctx.Done(), took %v", elapsed)
	}
}

func TestSSE_CassetteSerializationRedactionAndMetrics(t *testing.T) {
	tmpDir := t.TempDir()
	cas := NewCassette("cas_sse_metrics")

	events, err := DecodeSSE(strings.NewReader(sampleAnthropicSSE))
	if err != nil {
		t.Fatalf("DecodeSSE failed: %v", err)
	}
	// Inject secret in an SSE event to verify redaction
	events = append(events, SSEEvent{
		Index: len(events),
		Event: SSEEventContentBlockDelta,
		Data:  `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"key sk-ant-api03-1234567890abcdef1234567890"}}`,
	})

	cas.Interactions = append(cas.Interactions, Interaction{
		ID:       "sse_001",
		Sequence: 1,
		Type:     InteractionTypeSSE,
		Request: Request{
			Method:   "POST",
			Endpoint: "/v1/messages",
		},
		Response: Response{
			StatusCode: 200,
			LatencyMs:  350,
			SSEEvents:  events,
		},
	})

	casPath := filepath.Join(tmpDir, "sse_cassette.yaml")
	if err := Save(casPath, cas); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Ensure original in-memory cassette was NOT mutated by Save/ToYAML
	if strings.Contains(cas.Interactions[0].Response.SSEEvents[len(events)-1].Data, "[REDACTED_ANTHROPIC_KEY]") {
		t.Errorf("Save mutated caller cassette in-place instead of cloning")
	}

	loaded, err := Load(casPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	hasSecrets, reason := CheckForUnredactedSecrets(loaded)
	if hasSecrets {
		t.Fatalf("expected loaded SSE cassette to have all secrets redacted, got: %s", reason)
	}

	// Anthropic tokens: 412 in + 94 out + 100 cache_create + 50 cache_read = 656 tokens
	if loaded.Metadata.Metrics.EstimatedTokensSaved != 656 {
		t.Errorf("expected 656 estimated tokens saved from Anthropic SSE stream, got %d", loaded.Metadata.Metrics.EstimatedTokensSaved)
	}
	if loaded.Metadata.Metrics.EstimatedCostSavedUSD <= 0 {
		t.Errorf("expected positive cost saved USD, got %f", loaded.Metadata.Metrics.EstimatedCostSavedUSD)
	}
}

func TestSSE_GoogleGeminiStreamReconstructionAndMetrics(t *testing.T) {
	geminiSSE := strings.Join([]string{
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"First, inspect ","thought":true}]},"index":0}],"modelVersion":"gemini-2.5-pro","responseId":"resp_gem_001"}`,
		"",
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"the SSE parser.","thought":true}]},"index":0}],"modelVersion":"gemini-2.5-pro","responseId":"resp_gem_001"}`,
		"",
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"I will read ","thoughtSignature":"sig_gem_part_1"}]},"index":0}],"modelVersion":"gemini-2.5-pro","responseId":"resp_gem_001"}`,
		"",
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"pkg/cassette/sse.go."},{"functionCall":{"id":"fc_gem_01","name":"read_file","args":{"path":"pkg/cassette/sse.go","limit":9007199254740993}},"thoughtSignature":"sig_gem_part_2"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":300,"candidatesTokenCount":60,"thoughtsTokenCount":40,"cachedContentTokenCount":50,"totalTokenCount":400},"modelVersion":"gemini-2.5-pro","responseId":"resp_gem_001"}`,
		"",
	}, "\r\n")

	events, err := DecodeSSE(strings.NewReader(geminiSSE))
	if err != nil {
		t.Fatalf("DecodeSSE failed on Gemini SSE stream: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("expected 4 Gemini SSE events, got %d", len(events))
	}

	summary := ReconstructStreamSummary(events)
	if summary.Provider != "gemini" {
		t.Errorf("expected provider 'gemini', got %q", summary.Provider)
	}
	if summary.Model != "gemini-2.5-pro" {
		t.Errorf("expected model 'gemini-2.5-pro', got %q", summary.Model)
	}
	if summary.MessageID != "resp_gem_001" {
		t.Errorf("expected message_id 'resp_gem_001', got %q", summary.MessageID)
	}
	if summary.Role != "model" {
		t.Errorf("expected role 'model', got %q", summary.Role)
	}
	if summary.Text != "I will read pkg/cassette/sse.go." {
		t.Errorf("unexpected Gemini text: %q", summary.Text)
	}
	if len(summary.ThinkingBlocks) != 1 || summary.ThinkingBlocks[0] != "First, inspect the SSE parser." {
		t.Errorf("unexpected Gemini thinking blocks: %+v", summary.ThinkingBlocks)
	}
	if len(summary.Signatures) != 2 || summary.Signatures[0] != "sig_gem_part_1" || summary.Signatures[1] != "sig_gem_part_2" {
		t.Errorf("unexpected Gemini signatures: %+v", summary.Signatures)
	}
	if len(summary.ToolCalls) != 1 {
		t.Fatalf("expected 1 Gemini functionCall, got %d", len(summary.ToolCalls))
	}
	fc := summary.ToolCalls[0]
	if fc.ID != "fc_gem_01" || fc.Name != "read_file" {
		t.Errorf("unexpected Gemini functionCall metadata: %+v", fc)
	}
	if fc.Input["path"] != "pkg/cassette/sse.go" || fc.Input["limit"] != int64(9007199254740993) {
		t.Errorf("unexpected Gemini functionCall args: %+v", fc.Input)
	}
	if summary.StopReason != "STOP" {
		t.Errorf("expected stop_reason 'STOP', got %q", summary.StopReason)
	}
	// OutputTokens = candidatesTokenCount (60) + thoughtsTokenCount (40) = 100
	if summary.Usage.InputTokens != 300 || summary.Usage.OutputTokens != 100 ||
		summary.Usage.CacheReadInputTokens != 50 || summary.Usage.TotalTokens != 400 {
		t.Errorf("unexpected Gemini usage: %+v", summary.Usage)
	}

	// Verify ComputeMetrics on BOTH streaming Gemini SSE and non-streaming Gemini JSON responses
	cas := NewCassette("cas_gemini_metrics")
	cas.Interactions = []Interaction{
		{
			ID:       "gem_sse_1",
			Sequence: 1,
			Type:     InteractionTypeSSE,
			Request:  Request{Method: "POST", Endpoint: "/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse"},
			Response: Response{StatusCode: 200, LatencyMs: 120, SSEEvents: events},
		},
		{
			ID:       "gem_http_2",
			Sequence: 2,
			Type:     InteractionTypeHTTP,
			Request:  Request{Method: "POST", Endpoint: "/v1beta/models/gemini-2.5-flash:generateContent"},
			Response: Response{
				StatusCode: 200,
				LatencyMs:  80,
				RawBody:    `{"candidates":[{"content":{"role":"model","parts":[{"text":"Non-streaming reply"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":150,"candidatesTokenCount":30,"thoughtsTokenCount":20,"totalTokenCount":200},"modelVersion":"gemini-2.5-flash"}`,
			},
		},
	}

	cas.ComputeMetrics()
	// Total tokens = 400 (SSE) + 200 (HTTP) = 600
	if cas.Metadata.Metrics.EstimatedTokensSaved != 600 {
		t.Errorf("expected 600 total Gemini tokens saved, got %d", cas.Metadata.Metrics.EstimatedTokensSaved)
	}
	if cas.Metadata.Metrics.EstimatedCostSavedUSD <= 0 {
		t.Errorf("expected positive Gemini cost saved USD, got %f", cas.Metadata.Metrics.EstimatedCostSavedUSD)
	}
}

func TestGeminiSSE_TrailingZeroUsagePreservesTokensAndEncodeNormalizesCRLF(t *testing.T) {
	// 1. Trailing Gemini metadata chunk with 0 candidatesTokenCount/thoughtsTokenCount must not clobber earlier accumulated counts.
	rawSSE := strings.Join([]string{
		`data: {"candidates":[{"content":{"role":"model","parts":[{"thought":true,"text":"thinking..."},{"text":"Answer"}]}}],"usageMetadata":{"promptTokenCount":120,"candidatesTokenCount":45,"thoughtsTokenCount":15,"totalTokenCount":180},"modelVersion":"gemini-2.5-pro"}`,
		"",
		`data: {"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":120,"candidatesTokenCount":0,"thoughtsTokenCount":0},"modelVersion":"gemini-2.5-pro"}`,
		"",
		"",
	}, "\n")

	events, err := DecodeSSE(strings.NewReader(rawSSE))
	if err != nil {
		t.Fatalf("DecodeSSE failed: %v", err)
	}
	summary := ReconstructStreamSummary(events)
	if summary.Usage.OutputTokens != 60 {
		t.Fatalf("expected Gemini OutputTokens=60 (45+15) preserved despite trailing 0 usage chunk, got %d", summary.Usage.OutputTokens)
	}

	// 2. EncodeSSEEvent must normalize embedded CRLF (\r\n) in Data and Comment so it cannot forge blank-line frame boundaries.
	ev := SSEEvent{
		Comment: "keepalive\r\ninjected: bad",
		Event:   "message",
		Data:    "line1\r\n\r\nline3",
	}
	encoded := EncodeSSEEvent(ev)
	roundTrip, err := DecodeSSE(strings.NewReader(string(encoded)))
	if err != nil {
		t.Fatalf("round-trip DecodeSSE failed: %v", err)
	}
	if len(roundTrip) != 1 {
		t.Fatalf("expected exactly 1 SSE event (no forged frame boundary from CRLF), got %d", len(roundTrip))
	}
	if roundTrip[0].Data != "line1\n\nline3" {
		t.Errorf("unexpected decoded Data after CRLF normalization: %q", roundTrip[0].Data)
	}
}
