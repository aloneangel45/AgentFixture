package suite

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aloneangel45/AgentFixture/pkg/budget"
	"github.com/aloneangel45/AgentFixture/pkg/cassette"
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
