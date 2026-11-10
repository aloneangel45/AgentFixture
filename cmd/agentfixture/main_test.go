package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
)

func TestCLI_VersionFormattingNoDoubleV(t *testing.T) {
	if got := formatVersionString("v1.1.0"); !strings.Contains(got, "AgentFixture v1.1.0") || strings.Contains(got, "vv1.1.0") {
		t.Fatalf("unexpected version formatting for v1.1.0: %q", got)
	}
	if got := formatVersionString("1.1.0"); !strings.Contains(got, "AgentFixture v1.1.0") {
		t.Fatalf("unexpected version formatting for 1.1.0: %q", got)
	}
}

func TestCLI_VerifyInspectAndRedactCommands(t *testing.T) {
	tmpDir := t.TempDir()
	validCasPath := filepath.Join(tmpDir, "valid.yaml")

	events, err := cassette.DecodeSSE(strings.NewReader(
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-sonnet-4-20250514\",\"usage\":{\"input_tokens\":50,\"output_tokens\":1}}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"verified\"}}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":10}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	))
	if err != nil {
		t.Fatal(err)
	}

	cas := cassette.NewCassette("cas_verify_test")
	cas.Interactions = append(cas.Interactions, cassette.Interaction{
		ID:       "int_001",
		Sequence: 1,
		Type:     cassette.InteractionTypeSSE,
		Request: cassette.Request{
			Method:   "POST",
			Endpoint: "/v1/messages",
		},
		Response: cassette.Response{
			StatusCode: 200,
			SSEEvents:  events,
		},
	})

	if err := cassette.Save(validCasPath, cas); err != nil {
		t.Fatalf("failed to save test cassette: %v", err)
	}

	if err := handleVerify([]string{validCasPath}); err != nil {
		t.Fatalf("expected handleVerify to succeed, got: %v", err)
	}
	if err := handleInspect([]string{validCasPath}); err != nil {
		t.Fatalf("expected handleInspect text mode to succeed, got: %v", err)
	}
	if err := handleInspect([]string{"-json", validCasPath}); err != nil {
		t.Fatalf("expected handleInspect -json mode to succeed, got: %v", err)
	}

	// Write raw unredacted YAML directly to disk and verify fails until `redact` runs
	leakyPath := filepath.Join(tmpDir, "leaky.yaml")
	rawLeakyYAML := `version: agentfixture/v1
metadata:
  cassette_id: leaky_01
interactions:
  - id: int_leak
    sequence: 1
    type: http
    request:
      method: POST
      endpoint: /v1/messages
      headers:
        Authorization: Bearer sk-ant-api03-1234567890abcdef1234567890
      raw_body: '{"prompt":"secret sk-proj-1234567890abcdef1234567890"}'
    response:
      status_code: 200
`
	if err := os.WriteFile(leakyPath, []byte(rawLeakyYAML), 0644); err != nil {
		t.Fatal(err)
	}
	if err := handleVerify([]string{leakyPath}); err == nil {
		t.Fatalf("expected handleVerify to reject unredacted cassette")
	}
	if err := handleRedact([]string{leakyPath}); err != nil {
		t.Fatalf("handleRedact failed: %v", err)
	}
	if err := handleVerify([]string{leakyPath}); err != nil {
		t.Fatalf("expected handleVerify to pass after handleRedact, got: %v", err)
	}
}

func TestCLI_DiffCommand_ExitCodeAndSSESemanticDiff(t *testing.T) {
	tmpDir := t.TempDir()
	casPath1 := filepath.Join(tmpDir, "cas1.yaml")
	casPath2 := filepath.Join(tmpDir, "cas2.yaml")

	ev1, _ := cassette.DecodeSSE(strings.NewReader(
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello A\"}}\n\n",
	))
	ev2, _ := cassette.DecodeSSE(strings.NewReader(
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello B\"}}\n\n",
	))

	c1 := cassette.NewCassette("c1")
	c1.Interactions = append(c1.Interactions, cassette.Interaction{
		ID:       "int_01",
		Sequence: 1,
		Type:     cassette.InteractionTypeSSE,
		Request:  cassette.Request{Method: "POST", Endpoint: "/v1/messages"},
		Response: cassette.Response{StatusCode: 200, SSEEvents: ev1},
	})
	_ = cassette.Save(casPath1, c1)

	c2 := cassette.NewCassette("c2")
	c2.Interactions = append(c2.Interactions, cassette.Interaction{
		ID:       "int_01",
		Sequence: 1,
		Type:     cassette.InteractionTypeSSE,
		Request:  cassette.Request{Method: "POST", Endpoint: "/v1/messages"},
		Response: cassette.Response{StatusCode: 200, SSEEvents: ev2},
	})
	_ = cassette.Save(casPath2, c2)

	// Without -exit-code returns nil
	if err := handleDiff([]string{casPath1, casPath2}); err != nil {
		t.Fatalf("handleDiff without -exit-code returned unexpected error: %v", err)
	}
	// With -exit-code returns errDiffDetected on SSE semantic drift
	if err := handleDiff([]string{"-exit-code", casPath1, casPath2}); !errors.Is(err, errDiffDetected) {
		t.Fatalf("expected errDiffDetected with -exit-code, got: %v", err)
	}
	// Identical files with -exit-code return nil
	if err := handleDiff([]string{"-exit-code", casPath1, casPath1}); err != nil {
		t.Fatalf("expected nil on identical cassettes with -exit-code, got: %v", err)
	}
}

func TestCLI_RecordCommand_RecordsSubprocessInteractions(t *testing.T) {
	// Helper subprocess entry point: when AGENTFIXTURE_CLI_HELPER=1, act as an agent making an HTTP call to ANTHROPIC_BASE_URL!
	if os.Getenv("AGENTFIXTURE_CLI_HELPER") == "1" {
		baseURL := os.Getenv("ANTHROPIC_BASE_URL")
		resp, err := http.Post(baseURL+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude-sonnet-4","stream":true}`))
		if err != nil {
			os.Exit(2)
		}
		_ = resp.Body.Close()
		os.Exit(0)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_cli\",\"model\":\"claude-sonnet-4\",\"usage\":{\"input_tokens\":10,\"output_tokens\":4}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer upstream.Close()

	outCas := filepath.Join(t.TempDir(), "recorded_cli.yaml")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("AGENTFIXTURE_CLI_HELPER", "1")
	err = handleRecord([]string{
		"-cassette", outCas,
		"-upstream", upstream.URL,
		"--",
		exe, "-test.run=^TestCLI_RecordCommand_RecordsSubprocessInteractions$",
	})
	if err != nil {
		t.Fatalf("handleRecord -- <cmd> failed: %v", err)
	}

	loaded, err := cassette.Load(outCas)
	if err != nil {
		t.Fatalf("failed to load recorded cassette: %v", err)
	}
	if len(loaded.Interactions) != 1 || loaded.Interactions[0].Type != cassette.InteractionTypeSSE {
		t.Fatalf("expected 1 recorded SSE interaction from subprocess, got: %+v", loaded.Interactions)
	}

	// Now replay the exact same subprocess with upstream CLOSED!
	upstream.Close()
	err = handleReplay([]string{
		"-cassette", outCas,
		"-timing", "instant",
		"--",
		exe, "-test.run=^TestCLI_RecordCommand_RecordsSubprocessInteractions$",
	})
	if err != nil {
		t.Fatalf("handleReplay -- <cmd> failed against closed upstream: %v", err)
	}
}

func TestCLI_ImportCommand(t *testing.T) {
	tmpDir := t.TempDir()
	sampleJSON := `{
		"id": "trace_test_import",
		"command": "test cmd",
		"start_time": "2026-11-05T20:00:00Z",
		"status": "completed",
		"frames": [
			{
				"index": 1,
				"timestamp": "2026-11-05T20:00:01Z",
				"type": "tool_call",
				"tool": {
					"name": "calc",
					"arguments": {"a": 1, "b": 2},
					"result": "3",
					"duration_ms": 10
				}
			}
		]
	}`

	jsonPath := filepath.Join(tmpDir, "flight.json")
	outPath := filepath.Join(tmpDir, "out.yaml")
	if err := os.WriteFile(jsonPath, []byte(sampleJSON), 0644); err != nil {
		t.Fatalf("failed to write flight json: %v", err)
	}

	if err := handleImport([]string{"-traceport", jsonPath, "-out", outPath}); err != nil {
		t.Fatalf("handleImport failed: %v", err)
	}

	imported, err := cassette.Load(outPath)
	if err != nil {
		t.Fatalf("failed to reload imported cassette: %v", err)
	}
	if len(imported.Interactions) != 1 {
		t.Errorf("expected 1 imported interaction, got %d", len(imported.Interactions))
	}
	if !strings.Contains(imported.Interactions[0].Response.ToolResult, "3") {
		t.Errorf("expected result '3', got %s", imported.Interactions[0].Response.ToolResult)
	}
}
