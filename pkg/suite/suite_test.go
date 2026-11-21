package suite

import (
	"bufio"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aloneangel45/AgentFixture/pkg/budget"
	"github.com/aloneangel45/AgentFixture/pkg/cassette"
	"github.com/aloneangel45/AgentFixture/pkg/chaos"
	"github.com/aloneangel45/AgentFixture/pkg/matcher"
)

func saveTestCassette(t *testing.T, path string, cas *cassette.Cassette) {
	t.Helper()
	if err := cassette.Save(path, cas); err != nil {
		t.Fatalf("save cassette %s: %v", path, err)
	}
}

func makeHTTPCassette(id, model, endpoint, prompt string, inTok, outTok, cacheRead int, toolName string) *cassette.Cassette {
	cas := cassette.NewCassette(id)
	cas.Metadata.Model = model
	req := cassette.Request{
		Method:   "POST",
		Endpoint: endpoint,
		RawBody:  prompt,
	}
	req.Matcher.StrictHash = matcher.ComputeRequestHash(&req)
	respBody := map[string]interface{}{
		"model": model,
		"content": []interface{}{
			map[string]interface{}{"type": "text", "text": "done"},
		},
	}
	if toolName != "" {
		respBody["content"] = append(respBody["content"].([]interface{}), map[string]interface{}{
			"type":  "tool_use",
			"id":    "tu_1",
			"name":  toolName,
			"input": map[string]interface{}{"path": "main.go"},
		})
	}
	cas.Interactions = append(cas.Interactions, cassette.Interaction{
		ID:       "int_001",
		Sequence: 1,
		Type:     cassette.InteractionTypeHTTP,
		Request:  req,
		Response: cassette.Response{
			StatusCode: 200,
			Headers:    map[string]string{"Content-Type": "application/json"},
			Body:       respBody,
			Usage: cassette.Usage{
				InputTokens:          inTok,
				OutputTokens:         outTok,
				CacheReadInputTokens: cacheRead,
			},
		},
	})
	return cas
}

func TestSuite_DeclarativeYAMLParallelExecutionWithChaosAndAssertions(t *testing.T) {
	// Helper subprocess modes for HTTP retry with chaos & MCP stdio replay
	if mode := os.Getenv("AGENTFIXTURE_SUITE_HELPER"); mode != "" {
		switch mode {
		case "http_retry":
			baseURL := os.Getenv("ANTHROPIC_BASE_URL")
			// Expect first call to return 429 from chaos, retry and expect 200!
			var lastStatus int
			for attempt := 0; attempt < 3; attempt++ {
				resp, err := http.Post(baseURL+"/v1/messages", "application/json", strings.NewReader(`{"prompt":"hello"}`))
				if err != nil {
					os.Exit(2)
				}
				_, _ = io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				lastStatus = resp.StatusCode
				if resp.StatusCode == http.StatusOK {
					os.Exit(0)
				}
				if resp.StatusCode == http.StatusTooManyRequests {
					continue
				}
				os.Exit(3)
			}
			if lastStatus != http.StatusOK {
				os.Exit(4)
			}
			os.Exit(0)

		case "mcp_client":
			// Speak JSON-RPC over stdout (requests) and stdin (responses)
			req := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_file","arguments":{"path":"README.md"}}}` + "\n"
			_, _ = io.WriteString(os.Stdout, req)
			sc := bufio.NewScanner(os.Stdin)
			if !sc.Scan() {
				os.Exit(2)
			}
			var resp map[string]interface{}
			if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
				os.Exit(3)
			}
			if resp["result"] == nil {
				os.Exit(4)
			}
			os.Exit(0)
		}
	}

	tmpDir := t.TempDir()
	casHTTPPath := filepath.Join(tmpDir, "http_case.yaml")
	casMCPPath := filepath.Join(tmpDir, "mcp_case.yaml")
	casForbiddenPath := filepath.Join(tmpDir, "forbidden_case.yaml")

	saveTestCassette(t, casHTTPPath, makeHTTPCassette("cas_http", "claude-sonnet-4-5", "/v1/messages", `{"prompt":"hello"}`, 1000, 200, 3000, "read_file"))
	saveTestCassette(t, casForbiddenPath, makeHTTPCassette("cas_forbid", "claude-sonnet-4-5", "/v1/messages", `{"prompt":"danger"}`, 500, 100, 0, "rm_rf"))

	mcpCas := cassette.NewCassette("cas_mcp")
	mcpReq := cassette.Request{
		Method:    "tools/call",
		ToolName:  "read_file",
		ToolInput: map[string]interface{}{"path": "README.md"},
	}
	mcpReq.Matcher.StrictHash = matcher.ComputeToolHash("read_file", mcpReq.ToolInput)
	mcpCas.Interactions = append(mcpCas.Interactions, cassette.Interaction{
		ID:       "mcp_001",
		Sequence: 1,
		Type:     cassette.InteractionTypeTool,
		Request:  mcpReq,
		Response: cassette.Response{
			StatusCode: 0,
			ToolResult: "file contents ok",
		},
	})
	saveTestCassette(t, casMCPPath, mcpCas)

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	suiteYAML := `version: agentfixture/suite/v1
name: parallel-agent-regression
defaults:
  matcher: strict
  timing: instant
cases:
  - name: http_chaos_retry_pass
    cassette: http_case.yaml
    mode: http
    command: ` + jsonQuote(exe) + `
    args:
      - "-test.run=^TestSuite_DeclarativeYAMLParallelExecutionWithChaosAndAssertions$"
    env:
      AGENTFIXTURE_SUITE_HELPER: http_retry
    chaos:
      seed: 42
      rate_limit: 1
    budget:
      max_tokens: 10000
      max_cost_usd: 0.10
      min_cache_hit_rate: 0.50
    assertions:
      expected_tools:
        - read_file
      forbidden_tools:
        - rm_rf
      max_turns: 2
      expect_exit_code: 0

  - name: mcp_stdio_pass
    cassette: mcp_case.yaml
    mode: mcp
    command: ` + jsonQuote(exe) + `
    args:
      - "-test.run=^TestSuite_DeclarativeYAMLParallelExecutionWithChaosAndAssertions$"
    env:
      AGENTFIXTURE_SUITE_HELPER: mcp_client
    assertions:
      expected_tools:
        - read_file
      max_turns: 1

  - name: static_forbidden_tool_fail
    cassette: forbidden_case.yaml
    mode: verify
    assertions:
      forbidden_tools:
        - rm_rf
`
	suitePath := filepath.Join(tmpDir, "suite.yaml")
	if err := os.WriteFile(suitePath, []byte(suiteYAML), 0644); err != nil {
		t.Fatal(err)
	}

	spec, err := LoadSuiteFile(suitePath)
	if err != nil {
		t.Fatalf("LoadSuiteFile failed: %v", err)
	}

	rep, err := Run(spec, RunOptions{
		Concurrency: 3,
		BaseDir:     tmpDir,
		Source:      suitePath,
	})
	if err != nil {
		t.Fatalf("suite.Run failed: %v", err)
	}

	if rep.TotalCases != 3 || rep.PassedCases != 2 || rep.FailedCases != 1 {
		t.Fatalf("unexpected counts: total=%d passed=%d failed=%d\n%s",
			rep.TotalCases, rep.PassedCases, rep.FailedCases, rep.FormatSummary())
	}
	if rep.Passed {
		t.Fatalf("expected suite to fail due to forbidden_tools case")
	}
	// Deterministic order preserved
	if rep.Cases[0].Name != "http_chaos_retry_pass" || !rep.Cases[0].Passed {
		t.Fatalf("expected case 0 to pass: %+v", rep.Cases[0])
	}
	if rep.Cases[0].ChaosStats.RateLimitsInjected != 1 {
		t.Fatalf("expected 1 rate-limit chaos fault injected, got %+v", rep.Cases[0].ChaosStats)
	}
	if rep.Cases[1].Name != "mcp_stdio_pass" || !rep.Cases[1].Passed {
		t.Fatalf("expected case 1 (mcp) to pass: %+v", rep.Cases[1])
	}
	if rep.Cases[2].Name != "static_forbidden_tool_fail" || rep.Cases[2].Passed {
		t.Fatalf("expected case 2 (forbidden tool) to fail: %+v", rep.Cases[2])
	}
	if !strings.Contains(strings.Join(rep.Cases[2].Failures, " "), "forbidden tool") {
		t.Fatalf("expected forbidden tool failure message, got %v", rep.Cases[2].Failures)
	}

	md := rep.ToGitHubSummaryMarkdown()
	if !strings.Contains(md, "http_chaos_retry_pass") || !strings.Contains(md, "static_forbidden_tool_fail") {
		t.Fatalf("unexpected markdown summary:\n%s", md)
	}
}

func TestSuite_DirectoryDiscoveryAndBudgetEnforcement(t *testing.T) {
	dir := t.TempDir()
	saveTestCassette(t, filepath.Join(dir, "a.yaml"), makeHTTPCassette("a", "gpt-4o", "/v1/chat/completions", `{"p":"1"}`, 2000, 500, 1000, "search"))
	saveTestCassette(t, filepath.Join(dir, "b.yaml"), makeHTTPCassette("b", "gemini-2.5-flash", "/v1beta/models/gemini-2.5-flash:generateContent", `{"p":"2"}`, 1000, 200, 1000, ""))

	// Write a suite manifest inside the same directory to verify DiscoverDirectorySuite skips it!
	manifest := "version: agentfixture/suite/v1\ncases:\n  - name: x\n    cassette: a.yaml\n"
	if err := os.WriteFile(filepath.Join(dir, "suite.yaml"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}

	spec, err := DiscoverDirectorySuite(dir, CaseDefaults{})
	if err != nil {
		t.Fatalf("DiscoverDirectorySuite failed: %v", err)
	}
	if len(spec.Cases) != 2 {
		t.Fatalf("expected 2 discovered cassettes (skipping suite.yaml), got %d", len(spec.Cases))
	}

	// Pass with generous budget
	rep, err := Run(spec, RunOptions{
		Concurrency:   2,
		Source:        dir,
		DefaultBudget: budget.Thresholds{MaxTokens: 10000, MaxCostUSD: 0.50},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Passed {
		t.Fatalf("expected directory suite to pass:\n%s", rep.FormatSummary())
	}

	// Fail with tight token budget
	repFail, err := Run(spec, RunOptions{
		Concurrency:   2,
		Source:        dir,
		DefaultBudget: budget.Thresholds{MaxTokens: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	if repFail.Passed {
		t.Fatalf("expected directory suite to fail max-tokens budget")
	}
	_ = time.Now()
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestSuite_ValidationConcurrencyAndSuiteRelativePaths(t *testing.T) {
	// Nested suite directory separate from CWD
	rootDir := t.TempDir()
	suiteDir := filepath.Join(rootDir, "nested", "suites")
	casDir := filepath.Join(suiteDir, "cassettes")
	if err := os.MkdirAll(casDir, 0755); err != nil {
		t.Fatal(err)
	}

	saveTestCassette(t, filepath.Join(casDir, "rel.yaml"),
		makeHTTPCassette("rel_cas", "custom-model", "/v1/messages", `{"p":"hi"}`, 1000, 500, 0, "search"))
	pricingPath := filepath.Join(suiteDir, "custom_pricing.json")
	if err := os.WriteFile(pricingPath, []byte(`{"custom-model":{"input_per_million":2.0,"output_per_million":4.0}}`), 0644); err != nil {
		t.Fatal(err)
	}

	suitePath := filepath.Join(suiteDir, "suite.yaml")
	suiteYAML := `version: agentfixture/suite/v1
name: rel-path-suite
defaults:
  pricing_file: custom_pricing.json
cases:
  - name: relative_case
    cassette: cassettes/rel.yaml
    mode: verify
    assertions:
      expected_tools:
        - " search "
`
	if err := os.WriteFile(suitePath, []byte(suiteYAML), 0644); err != nil {
		t.Fatal(err)
	}

	spec, err := LoadSuiteFile(suitePath)
	if err != nil {
		t.Fatalf("LoadSuiteFile failed: %v", err)
	}

	// 1. Negative concurrency must error cleanly
	if _, err := Run(spec, RunOptions{Concurrency: -1}); err == nil {
		t.Fatalf("expected error for negative Concurrency")
	}

	// 2. Zero Concurrency defaults to 4 (never hangs) AND empty BaseDir resolves relative to suitePath!
	rep, err := Run(spec, RunOptions{Concurrency: 0})
	if err != nil {
		t.Fatalf("Run with default Concurrency and suite-relative BaseDir failed: %v", err)
	}
	if !rep.Passed || rep.PassedCases != 1 {
		t.Fatalf("expected suite-relative case to pass, got: %+v", rep)
	}
	// Custom pricing from suite-relative pricing_file: (1000*2 + 500*4)/1e6 = 0.004
	if math.Abs(rep.Budget.EstimatedCostUSD-0.004) > 1e-9 {
		t.Fatalf("expected suite-relative custom pricing cost $0.004, got $%.6f", rep.Budget.EstimatedCostUSD)
	}

	// 3. Validate rejects bad matcher, NaN/out-of-range threshold, bad timing, negative timeout
	badSpecs := []SuiteSpec{
		{Version: CurrentSuiteVersion, Defaults: CaseDefaults{Matcher: "bogus"}, Cases: []CaseSpec{{Name: "a", Cassette: "a.yaml"}}},
		{Version: CurrentSuiteVersion, Defaults: CaseDefaults{Threshold: math.NaN()}, Cases: []CaseSpec{{Name: "a", Cassette: "a.yaml"}}},
		{Version: CurrentSuiteVersion, Defaults: CaseDefaults{Threshold: 1.5}, Cases: []CaseSpec{{Name: "a", Cassette: "a.yaml"}}},
		{Version: CurrentSuiteVersion, Defaults: CaseDefaults{Timing: "warp"}, Cases: []CaseSpec{{Name: "a", Cassette: "a.yaml"}}},
		{Version: CurrentSuiteVersion, Defaults: CaseDefaults{Timeout: "-5s"}, Cases: []CaseSpec{{Name: "a", Cassette: "a.yaml"}}},
		{Version: CurrentSuiteVersion, Cases: []CaseSpec{{Name: "a", Cassette: "a.yaml", Timeout: "0s"}}},
	}
	for i, s := range badSpecs {
		if err := s.Validate(); err == nil {
			t.Errorf("badSpec[%d]: expected validation error, got nil", i)
		}
	}
}

func TestSuite_ExtractObservedTools_MultiProviderAndEmpty(t *testing.T) {
	// Empty cassette returns nil
	emptyCas := cassette.NewCassette("empty")
	if got := ExtractObservedTools(emptyCas, nil); len(got) != 0 {
		t.Fatalf("expected no tools for empty cassette, got %v", got)
	}

	cas := cassette.NewCassette("multi_provider_tools")
	cas.Interactions = []cassette.Interaction{
		// 1. Anthropic JSON body (tool_use + mcp_tool_use)
		{
			ID: "int_anth", Sequence: 1, Type: cassette.InteractionTypeHTTP,
			Response: cassette.Response{
				StatusCode: 200,
				Body: map[string]interface{}{
					"content": []interface{}{
						map[string]interface{}{"type": "tool_use", "name": "anth_tool"},
						map[string]interface{}{"type": "mcp_tool_use", "name": "anth_mcp_tool"},
					},
				},
			},
		},
		// 2. OpenAI Chat Completions (tool_calls + legacy function_call)
		{
			ID: "int_oai_chat", Sequence: 2, Type: cassette.InteractionTypeHTTP,
			Response: cassette.Response{
				StatusCode: 200,
				RawBody:    `{"choices":[{"message":{"tool_calls":[{"function":{"name":"oai_tool"}}],"function_call":{"name":"oai_legacy_fn"}}}]}`,
			},
		},
		// 3. OpenAI Responses API (output[].function_call)
		{
			ID: "int_oai_resp", Sequence: 3, Type: cassette.InteractionTypeHTTP,
			Response: cassette.Response{
				StatusCode: 200,
				RawBody:    `{"output":[{"type":"function_call","name":"oai_responses_fn"}]}`,
			},
		},
		// 4. Google Gemini functionCall JSON
		{
			ID: "int_gem", Sequence: 4, Type: cassette.InteractionTypeHTTP,
			Response: cassette.Response{
				StatusCode: 200,
				RawBody:    `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"gemini_fn","args":{}}}]}}]}`,
			},
		},
		// 5. Raw SSE string in Response.RawBody without pre-decoded SSEEvents
		{
			ID: "int_raw_sse", Sequence: 5, Type: cassette.InteractionTypeSSE,
			Response: cassette.Response{
				StatusCode: 200,
				RawBody:    "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"raw_sse_tool\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			},
		},
		// 6. MCP stdio tools/call with ToolName empty (only in Request.RawBody)
		{
			ID: "int_mcp_raw", Sequence: 6, Type: cassette.InteractionTypeStdio,
			Request: cassette.Request{
				Method:  "tools/call",
				RawBody: `{"name":"mcp_raw_tool","arguments":{"k":"v"}}`,
			},
		},
	}

	got := ExtractObservedTools(cas, nil)
	want := []string{"anth_mcp_tool", "anth_tool", "gemini_fn", "mcp_raw_tool", "oai_legacy_fn", "oai_responses_fn", "oai_tool", "raw_sse_tool"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExtractObservedTools mismatch:\n got: %v\nwant: %v", got, want)
	}

	// Filtering by empty non-nil matchedIDs returns nil
	if filtered := ExtractObservedTools(cas, map[string]bool{}); len(filtered) != 0 {
		t.Fatalf("expected empty tools for empty matchedIDs filter, got %v", filtered)
	}
}

func TestSuite_MCPSubprocessTimeoutAndChaosLatencyNoHang(t *testing.T) {
	if os.Getenv("AGENTFIXTURE_MCP_HANG_HELPER") == "1" {
		// Send a ping request to trigger the 10s chaos latency, then block forever waiting on stdin.
		_, _ = io.WriteString(os.Stdout, `{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n")
		_, _ = io.ReadAll(os.Stdin)
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}

	tmpDir := t.TempDir()
	casPath := filepath.Join(tmpDir, "mcp_hang.yaml")
	cas := cassette.NewCassette("cas_mcp_hang")
	saveTestCassette(t, casPath, cas)

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	expect124 := 124
	spec := &SuiteSpec{
		Version: CurrentSuiteVersion,
		Name:    "mcp-timeout-guard",
		Cases: []CaseSpec{
			{
				Name:     "mcp_hung_child_timeout",
				Cassette: casPath,
				Mode:     "mcp",
				Command:  exe,
				Args:     []string{"-test.run=^TestSuite_MCPSubprocessTimeoutAndChaosLatencyNoHang$"},
				Env:      map[string]string{"AGENTFIXTURE_MCP_HANG_HELPER": "1"},
				Timeout:  "250ms",
				Chaos:    chaos.Config{LatencyMs: 10000},
				Assertions: AssertionsSpec{
					ExpectExitCode: &expect124,
				},
			},
		},
	}

	start := time.Now()
	rep, err := Run(spec, RunOptions{Concurrency: 1})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("suite.Run failed: %v", err)
	}
	if elapsed > 6*time.Second {
		t.Fatalf("hung MCP subprocess was not terminated promptly by timeout + WaitDelay: elapsed=%v", elapsed)
	}
	if rep.Cases[0].ExitCode != 124 {
		t.Fatalf("expected timeout exit code 124, got %d (failures=%v)", rep.Cases[0].ExitCode, rep.Cases[0].Failures)
	}
	if !rep.Passed {
		t.Fatalf("expected case expecting exit_code 124 to pass, got failures=%v", rep.Cases[0].Failures)
	}
}
