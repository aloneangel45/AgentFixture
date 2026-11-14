package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
	"github.com/aloneangel45/AgentFixture/pkg/junit"
)

func TestCLI_VersionFormattingNoDoubleV(t *testing.T) {
	if got := formatVersionString("v1.2.0"); !strings.Contains(got, "AgentFixture v1.2.0") || strings.Contains(got, "vv1.2.0") {
		t.Fatalf("unexpected version formatting for v1.2.0: %q", got)
	}
	if got := formatVersionString("1.2.0"); !strings.Contains(got, "AgentFixture v1.2.0") {
		t.Fatalf("unexpected version formatting for 1.2.0: %q", got)
	}
	if got := formatVersionString(""); !strings.Contains(got, "AgentFixture v1.2.0") {
		t.Fatalf("unexpected empty fallback version formatting: %q", got)
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
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			os.Exit(3)
		}
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

func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() {
		os.Stdout = origStdout
	}()

	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()

	fnErr := fn()
	_ = w.Close()
	<-done
	_ = r.Close()
	return buf.String(), fnErr
}

func TestCLI_VerifyBatchDirectory_JSONAndJUnit(t *testing.T) {
	rootDir := filepath.Join(t.TempDir(), "fixtures")
	if err := os.MkdirAll(filepath.Join(rootDir, "nested"), 0755); err != nil {
		t.Fatal(err)
	}

	gemEvents, err := cassette.DecodeSSE(strings.NewReader(
		"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Gemini OK\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":100,\"candidatesTokenCount\":25,\"thoughtsTokenCount\":15,\"totalTokenCount\":140},\"modelVersion\":\"gemini-2.5-pro\",\"responseId\":\"r1\"}\n\n",
	))
	if err != nil {
		t.Fatal(err)
	}

	casA := cassette.NewCassette("cas_alpha")
	casA.Interactions = []cassette.Interaction{
		{
			ID:       "int_1",
			Sequence: 1,
			Type:     cassette.InteractionTypeSSE,
			Request:  cassette.Request{Method: "POST", Endpoint: "/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse"},
			Response: cassette.Response{StatusCode: 200, SSEEvents: gemEvents},
		},
	}
	if err := cassette.Save(filepath.Join(rootDir, "01_alpha.yaml"), casA); err != nil {
		t.Fatal(err)
	}

	casB := cassette.NewCassette("cas_beta")
	casB.Interactions = []cassette.Interaction{
		{
			ID:       "int_1",
			Sequence: 1,
			Type:     cassette.InteractionTypeHTTP,
			Request:  cassette.Request{Method: "POST", Endpoint: "/v1beta/models/gemini-2.5-flash:generateContent"},
			Response: cassette.Response{
				StatusCode: 200,
				RawBody:    `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]}}],"usageMetadata":{"promptTokenCount":50,"candidatesTokenCount":10,"totalTokenCount":60}}`,
			},
		},
	}
	// Save both .yml and .json in nested subdirectory
	if err := cassette.Save(filepath.Join(rootDir, "nested", "02_beta.yml"), casB); err != nil {
		t.Fatal(err)
	}
	if err := cassette.Save(filepath.Join(rootDir, "nested", "03_gamma.json"), casB); err != nil {
		t.Fatal(err)
	}

	junitOK := filepath.Join(t.TempDir(), "verify-ok.xml")
	rawJSON, err := captureStdout(t, func() error {
		return handleVerify([]string{"-json", "-junit", junitOK, rootDir})
	})
	if err != nil {
		t.Fatalf("expected directory handleVerify to pass, got: %v", err)
	}

	var rep verifySummaryReport
	if err := json.Unmarshal([]byte(rawJSON), &rep); err != nil {
		t.Fatalf("failed to unmarshal verify -json output: %v\nOutput:\n%s", err, rawJSON)
	}
	if !rep.Passed || rep.TotalCassettes != 3 || rep.ValidCassettes != 3 || rep.FailedCassettes != 0 {
		t.Fatalf("unexpected verify report summary: %+v", rep)
	}
	if rep.TotalInteractions != 3 || rep.TotalSSEStreams != 1 || rep.EstimatedTokensSaved != 260 {
		t.Errorf("unexpected aggregate metrics: interactions=%d sse=%d tokens=%d", rep.TotalInteractions, rep.TotalSSEStreams, rep.EstimatedTokensSaved)
	}
	// Verify deterministic sorted file order
	if !strings.HasSuffix(rep.Results[0].Path, "01_alpha.yaml") ||
		!strings.HasSuffix(rep.Results[1].Path, "nested/02_beta.yml") ||
		!strings.HasSuffix(rep.Results[2].Path, "nested/03_gamma.json") {
		t.Errorf("expected deterministic sorted discovery order, got: %+v", rep.Results)
	}

	xmlBytes, err := os.ReadFile(junitOK)
	if err != nil {
		t.Fatalf("failed to read verify JUnit XML: %v", err)
	}
	var suites junit.TestSuites
	if err := xml.Unmarshal(xmlBytes, &suites); err != nil {
		t.Fatalf("invalid JUnit XML: %v", err)
	}
	if suites.Tests != 3 || suites.Failures != 0 {
		t.Errorf("expected JUnit tests=3 failures=0, got tests=%d failures=%d", suites.Tests, suites.Failures)
	}

	// Now inject a leaky cassette in directory and verify non-zero exit + failure captured in both JSON and JUnit XML!
	leakyPath := filepath.Join(rootDir, "99_leaky.yaml")
	leakyYAML := "version: agentfixture/v1\nmetadata:\n  cassette_id: leaky_gem\ninteractions:\n  - id: int_1\n    sequence: 1\n    type: http\n    request:\n      method: POST\n      endpoint: /v1beta/models/gemini-2.5-pro:generateContent\n      headers:\n        x-goog-api-key: AIzaSyDaGmWKa4JsXZ-HjGw7ISLn_3namBGewQe\n    response:\n      status_code: 200\n"
	if err := os.WriteFile(leakyPath, []byte(leakyYAML), 0644); err != nil {
		t.Fatal(err)
	}

	junitFail := filepath.Join(t.TempDir(), "verify-fail.xml")
	failJSON, failErr := captureStdout(t, func() error {
		return handleVerify([]string{"-json", "-junit", junitFail, rootDir})
	})
	if failErr == nil {
		t.Fatalf("expected batch handleVerify to return error when 1 cassette has unredacted Google API key")
	}

	var failRep verifySummaryReport
	if err := json.Unmarshal([]byte(failJSON), &failRep); err != nil {
		t.Fatalf("expected clean JSON on stdout even when batch verify fails: %v\n%s", err, failJSON)
	}
	if failRep.Passed || failRep.TotalCassettes != 4 || failRep.ValidCassettes != 3 || failRep.FailedCassettes != 1 {
		t.Fatalf("unexpected failed batch verify summary: %+v", failRep)
	}

	failXMLBytes, err := os.ReadFile(junitFail)
	if err != nil {
		t.Fatalf("expected JUnit XML written even on batch verify failure: %v", err)
	}
	var failSuites junit.TestSuites
	if err := xml.Unmarshal(failXMLBytes, &failSuites); err != nil {
		t.Fatalf("invalid JUnit XML on failure: %v", err)
	}
	if failSuites.Tests != 4 || failSuites.Failures != 1 {
		t.Errorf("expected JUnit tests=4 failures=1, got tests=%d failures=%d", failSuites.Tests, failSuites.Failures)
	}
}

func TestCLI_DiffCommand_GeminiSSESemanticComparison(t *testing.T) {
	tmpDir := t.TempDir()
	p1 := filepath.Join(tmpDir, "gem1.yaml")
	p2 := filepath.Join(tmpDir, "gem2.yaml")
	p3 := filepath.Join(tmpDir, "gem3_drifted.yaml")

	// gem1 and gem2 split the exact same thought + text + functionCall across different SSE chunk boundaries!
	ev1, _ := cassette.DecodeSSE(strings.NewReader(
		"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Think \",\"thought\":true}]}}],\"modelVersion\":\"gemini-2.5-pro\"}\n\n" +
			"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"deeply.\",\"thought\":true},{\"text\":\"Hello Gemini!\"},{\"functionCall\":{\"id\":\"fc1\",\"name\":\"search\",\"args\":{\"q\":\"sse\"}}}]},\"finishReason\":\"STOP\"}],\"modelVersion\":\"gemini-2.5-pro\"}\n\n",
	))
	ev2, _ := cassette.DecodeSSE(strings.NewReader(
		"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Think deeply.\",\"thought\":true},{\"text\":\"Hello \"}]}}],\"modelVersion\":\"gemini-2.5-pro\"}\n\n" +
			"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Gemini!\"},{\"functionCall\":{\"id\":\"fc1\",\"name\":\"search\",\"args\":{\"q\":\"sse\"}}}]},\"finishReason\":\"STOP\"}],\"modelVersion\":\"gemini-2.5-pro\"}\n\n",
	))
	ev3, _ := cassette.DecodeSSE(strings.NewReader(
		"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Think deeply.\",\"thought\":true},{\"text\":\"Hello Gemini!\"},{\"functionCall\":{\"id\":\"fc1\",\"name\":\"search\",\"args\":{\"q\":\"DIFFERENT\"}}}]},\"finishReason\":\"STOP\"}],\"modelVersion\":\"gemini-2.5-pro\"}\n\n",
	))

	mkCas := func(id string, ev []cassette.SSEEvent) *cassette.Cassette {
		c := cassette.NewCassette(id)
		c.Interactions = []cassette.Interaction{
			{
				ID:       "int_1",
				Sequence: 1,
				Type:     cassette.InteractionTypeSSE,
				Request:  cassette.Request{Method: "POST", Endpoint: "/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse"},
				Response: cassette.Response{StatusCode: 200, SSEEvents: ev},
			},
		}
		return c
	}

	_ = cassette.Save(p1, mkCas("g1", ev1))
	_ = cassette.Save(p2, mkCas("g2", ev2))
	_ = cassette.Save(p3, mkCas("g3", ev3))

	if err := handleDiff([]string{"-exit-code", p1, p2}); err != nil {
		t.Fatalf("expected semantically identical Gemini SSE streams with different chunk splits to have zero diff, got: %v", err)
	}
	if err := handleDiff([]string{"-exit-code", p1, p3}); !errors.Is(err, errDiffDetected) {
		t.Fatalf("expected errDiffDetected when Gemini functionCall args drifted, got: %v", err)
	}
}

func TestCLI_ReplayWithJUnitXML(t *testing.T) {
	if os.Getenv("AGENTFIXTURE_JUNIT_REPLAY_HELPER") == "pass" {
		baseURL := os.Getenv("GOOGLE_GEMINI_BASE_URL")
		resp, err := http.Post(baseURL+"/v1beta/models/gemini-2.5-flash:generateContent", "application/json", strings.NewReader(`{"contents":[{"parts":[{"text":"ping"}]}]}`))
		if err != nil {
			os.Exit(2)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			os.Exit(3)
		}
		os.Exit(0)
	}
	if os.Getenv("AGENTFIXTURE_JUNIT_REPLAY_HELPER") == "noop" {
		// Does not call cassette -> triggers strict unmatched failure!
		os.Exit(0)
	}

	tmpDir := t.TempDir()
	casPath := filepath.Join(tmpDir, "gem_replay.yaml")
	cas := cassette.NewCassette("cas_gem_replay")
	cas.Interactions = []cassette.Interaction{
		{
			ID:       "int_001",
			Sequence: 1,
			Type:     cassette.InteractionTypeHTTP,
			Request: cassette.Request{
				Method:   "POST",
				Endpoint: "/v1beta/models/gemini-2.5-flash:generateContent",
				RawBody:  `{"contents":[{"parts":[{"text":"ping"}]}]}`,
			},
			Response: cassette.Response{
				StatusCode: 200,
				RawBody:    `{"candidates":[{"content":{"role":"model","parts":[{"text":"pong"}]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}`,
			},
		},
	}
	if err := cassette.Save(casPath, cas); err != nil {
		t.Fatal(err)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	// 1. Passing replay with -junit
	passXML := filepath.Join(tmpDir, "replay-pass.xml")
	t.Setenv("AGENTFIXTURE_JUNIT_REPLAY_HELPER", "pass")
	if err := handleReplay([]string{
		"-cassette", casPath,
		"-junit", passXML,
		"--",
		exe, "-test.run=^TestCLI_ReplayWithJUnitXML$",
	}); err != nil {
		t.Fatalf("expected passing handleReplay with -junit, got: %v", err)
	}
	passBytes, err := os.ReadFile(passXML)
	if err != nil {
		t.Fatalf("failed reading replay-pass.xml: %v", err)
	}
	var passSuites junit.TestSuites
	if err := xml.Unmarshal(passBytes, &passSuites); err != nil {
		t.Fatalf("invalid XML in replay-pass.xml: %v", err)
	}
	if passSuites.Tests != 1 || passSuites.Failures != 0 {
		t.Errorf("expected tests=1 failures=0 in replay-pass.xml, got tests=%d failures=%d", passSuites.Tests, passSuites.Failures)
	}

	// 2. Failing strict replay with -junit (must STILL write JUnit XML with failure node!)
	failXML := filepath.Join(tmpDir, "replay-fail.xml")
	t.Setenv("AGENTFIXTURE_JUNIT_REPLAY_HELPER", "noop")
	if err := handleReplay([]string{
		"-cassette", casPath,
		"-strict=true",
		"-junit", failXML,
		"--",
		exe, "-test.run=^TestCLI_ReplayWithJUnitXML$",
	}); err == nil {
		t.Fatalf("expected strict handleReplay to fail when interaction was uncalled")
	}
	failBytes, err := os.ReadFile(failXML)
	if err != nil {
		t.Fatalf("expected replay-fail.xml to be written on regression failure: %v", err)
	}
	var failSuites junit.TestSuites
	if err := xml.Unmarshal(failBytes, &failSuites); err != nil {
		t.Fatalf("invalid XML in replay-fail.xml: %v", err)
	}
	if failSuites.Failures != 1 {
		t.Errorf("expected failures=1 in replay-fail.xml, got %d", failSuites.Failures)
	}
}

func TestInstallers_IntegrityAndPowerShellSHA256Verification(t *testing.T) {
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	shPath := filepath.Join(repoRoot, "install.sh")
	ps1Path := filepath.Join(repoRoot, "install.ps1")

	shBytes, err := os.ReadFile(shPath)
	if err != nil {
		t.Fatalf("missing install.sh: %v", err)
	}
	if bytes.Contains(shBytes, []byte("\r")) {
		t.Fatalf("install.sh contains CR (\\r) bytes; POSIX shell scripts must use pure LF line endings")
	}
	shText := string(shBytes)
	if !strings.HasPrefix(shText, "#!/bin/sh\n") {
		t.Errorf("install.sh must start with #!/bin/sh")
	}
	for _, needle := range []string{"aloneangel45/AgentFixture", "checksums.txt", "agentfixture", "sha256sum", "shasum"} {
		if !strings.Contains(shText, needle) {
			t.Errorf("install.sh missing expected reference %q", needle)
		}
	}

	ps1Bytes, err := os.ReadFile(ps1Path)
	if err != nil {
		t.Fatalf("missing install.ps1: %v", err)
	}
	ps1Text := string(ps1Bytes)
	for _, needle := range []string{"aloneangel45/AgentFixture", "checksums.txt", "agentfixture-windows-", "Get-FileHash", "SHA256"} {
		if !strings.Contains(ps1Text, needle) {
			t.Errorf("install.ps1 missing expected reference %q", needle)
		}
	}

	if runtime.GOOS != "windows" {
		return
	}

	pwsh, err := exec.LookPath("powershell")
	if err != nil {
		t.Skip("powershell not found in PATH")
	}

	// Build a tiny real zip containing a compiled Windows helper binary that prints version
	tmpDir := t.TempDir()
	binSrc := filepath.Join(tmpDir, "stub.go")
	if err := os.WriteFile(binSrc, []byte("package main\nimport \"fmt\"\nfunc main(){ fmt.Println(\"AgentFixture v1.2.0 (installer-test)\") }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	builtExe := filepath.Join(tmpDir, "agentfixture.exe")
	buildCmd := exec.Command("go", "build", "-o", builtExe, binSrc)
	if out, bErr := buildCmd.CombinedOutput(); bErr != nil {
		t.Fatalf("failed building stub exe: %v (%s)", bErr, string(out))
	}
	exeBytes, err := os.ReadFile(builtExe)
	if err != nil {
		t.Fatal(err)
	}

	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	zf, err := zw.Create("agentfixture.exe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zf.Write(exeBytes); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zipData := zipBuf.Bytes()
	sum := sha256.Sum256(zipData)
	goodHex := hex.EncodeToString(sum[:])

	arch := "amd64"
	if runtime.GOARCH == "arm64" {
		arch = "arm64"
	}
	assetName := fmt.Sprintf("agentfixture-windows-%s.zip", arch)

	tamperChecksums := false
	releaseSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + assetName:
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(zipData)
		case "/checksums.txt":
			h := goodHex
			if tamperChecksums {
				h = strings.Repeat("0", 64)
			}
			_, _ = fmt.Fprintf(w, "%s  %s\n", h, assetName)
		default:
			http.NotFound(w, r)
		}
	}))
	defer releaseSrv.Close()

	// 1. Valid SHA-256 checksum -> installs cleanly
	installDirOK := filepath.Join(tmpDir, "installed_ok")
	cmdOK := exec.Command(pwsh, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", ps1Path)
	cmdOK.Env = append(os.Environ(),
		"AGENTFIXTURE_BASE_URL="+releaseSrv.URL,
		"AGENTFIXTURE_INSTALL_DIR="+installDirOK,
		"AGENTFIXTURE_NO_PATH_UPDATE=1",
	)
	outOK, err := cmdOK.CombinedOutput()
	if err != nil {
		t.Fatalf("install.ps1 failed on valid SHA-256 archive: %v\nOutput:\n%s", err, string(outOK))
	}
	if !strings.Contains(string(outOK), "SHA-256 verified") || !strings.Contains(string(outOK), "AgentFixture v1.2.0") {
		t.Errorf("unexpected install.ps1 output:\n%s", string(outOK))
	}
	if _, statErr := os.Stat(filepath.Join(installDirOK, "agentfixture.exe")); statErr != nil {
		t.Fatalf("expected agentfixture.exe installed in %s: %v", installDirOK, statErr)
	}

	// 2. Tampered SHA-256 checksum -> MUST abort with non-zero exit code and NOT install binary!
	tamperChecksums = true
	installDirBad := filepath.Join(tmpDir, "installed_tampered")
	cmdBad := exec.Command(pwsh, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", ps1Path)
	cmdBad.Env = append(os.Environ(),
		"AGENTFIXTURE_BASE_URL="+releaseSrv.URL,
		"AGENTFIXTURE_INSTALL_DIR="+installDirBad,
		"AGENTFIXTURE_NO_PATH_UPDATE=1",
	)
	outBad, errBad := cmdBad.CombinedOutput()
	if errBad == nil {
		t.Fatalf("SECURITY FAILURE: install.ps1 succeeded despite mismatched SHA-256 checksum!\nOutput:\n%s", string(outBad))
	}
	if _, statErr := os.Stat(filepath.Join(installDirBad, "agentfixture.exe")); !os.IsNotExist(statErr) {
		t.Fatalf("SECURITY FAILURE: tampered binary was written to %s", installDirBad)
	}
}
