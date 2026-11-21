package budget

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
)

func almostEqual(a, b, eps float64) bool {
	return math.Abs(a-b) <= eps
}

func TestDefaultPricingTableAndPrefixLookup(t *testing.T) {
	tbl := DefaultPricingTable()

	cases := []struct {
		model     string
		wantKey   string
		wantMatch bool
		wantIn    float64
		wantOut   float64
	}{
		{"claude-sonnet-4-5", "claude-sonnet-4-5", true, 3.0, 15.0},
		{"claude-sonnet-4-5-20250929", "claude-sonnet-4-5", true, 3.0, 15.0},
		{"anthropic/claude-opus-4-20250514", "claude-opus-4", true, 15.0, 75.0},
		{"claude-3-7-sonnet-20250219", "claude-3-7-sonnet", true, 3.0, 15.0},
		{"claude-3-5-sonnet-20241022", "claude-3-5-sonnet", true, 3.0, 15.0},
		{"gpt-5-2026-01-01", "gpt-5", true, 1.25, 10.0},
		{"gpt-4.1", "gpt-4.1", true, 2.0, 8.0},
		{"gpt-4o-2024-11-20", "gpt-4o", true, 2.5, 10.0},
		{"o3-2025-04-16", "o3", true, 2.0, 8.0},
		{"models/gemini-2.5-pro-preview", "gemini-2.5-pro", true, 1.25, 10.0},
		{"gemini-2.5-flash", "gemini-2.5-flash", true, 0.15, 0.60},
		{"unknown-custom-model", "default", false, 3.0, 15.0},
	}

	for _, tc := range cases {
		p, key, ok := tbl.Lookup(tc.model)
		if ok != tc.wantMatch || key != tc.wantKey {
			t.Errorf("Lookup(%q) = (%q, %v), want (%q, %v)", tc.model, key, ok, tc.wantKey, tc.wantMatch)
		}
		if !almostEqual(p.InputPerMillion, tc.wantIn, 1e-9) || !almostEqual(p.OutputPerMillion, tc.wantOut, 1e-9) {
			t.Errorf("Lookup(%q) rates = (%v, %v), want (%v, %v)", tc.model, p.InputPerMillion, p.OutputPerMillion, tc.wantIn, tc.wantOut)
		}
	}
}

func TestLoadPricingFileOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.json")
	content := `{
		"my-enterprise-model": {
			"input_per_million": 10.0,
			"output_per_million": 40.0,
			"cache_read_per_million": 1.0,
			"cache_write_per_million": 12.5
		},
		"gpt-5": {
			"input_per_million": 0.5,
			"output_per_million": 2.0,
			"cache_read_per_million": 0.05,
			"cache_write_per_million": 0.5
		}
	}`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	tbl, err := LoadPricingFile(path)
	if err != nil {
		t.Fatalf("LoadPricingFile failed: %v", err)
	}
	p, _, ok := tbl.Lookup("my-enterprise-model")
	if !ok || !almostEqual(p.InputPerMillion, 10.0, 1e-9) || !almostEqual(p.OutputPerMillion, 40.0, 1e-9) {
		t.Fatalf("unexpected custom model pricing: %+v (ok=%v)", p, ok)
	}
	p5, _, ok := tbl.Lookup("gpt-5")
	if !ok || !almostEqual(p5.InputPerMillion, 0.5, 1e-9) {
		t.Fatalf("expected gpt-5 override, got %+v", p5)
	}
	// Built-in untouched models remain intact
	if _, _, ok := tbl.Lookup("claude-opus-4"); !ok {
		t.Fatalf("expected built-in claude-opus-4 to remain present")
	}
}

func TestEvaluateAcrossProvidersAndRawFallback(t *testing.T) {
	cas := cassette.NewCassette("cas_multi_provider")

	// 1. Anthropic SSE (Usage.IsZero() -> parsed from SSE events)
	anthEvents, err := cassette.DecodeSSE(strings.NewReader(
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"claude-sonnet-4-5-20250929\",\"usage\":{\"input_tokens\":1000,\"cache_read_input_tokens\":3000,\"cache_creation_input_tokens\":1000,\"output_tokens\":1}}}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":500}}\n\n",
	))
	if err != nil {
		t.Fatal(err)
	}
	cas.Interactions = append(cas.Interactions, cassette.Interaction{
		ID:       "int_anth",
		Sequence: 1,
		Type:     cassette.InteractionTypeSSE,
		Response: cassette.Response{
			StatusCode: 200,
			SSEEvents:  anthEvents,
		},
	})

	// 2. OpenAI RawBody JSON (Usage.IsZero() -> parsed from RawBody with prompt_tokens_details.cached_tokens)
	cas.Interactions = append(cas.Interactions, cassette.Interaction{
		ID:       "int_openai",
		Sequence: 2,
		Type:     cassette.InteractionTypeHTTP,
		Response: cassette.Response{
			StatusCode: 200,
			RawBody:    `{"model":"gpt-4o","usage":{"prompt_tokens":2000,"completion_tokens":1000,"prompt_tokens_details":{"cached_tokens":1000}}}`,
		},
		Metadata: map[string]string{"model": "gpt-4o"},
	})

	// 3. Gemini RawBody JSON (Usage.IsZero() -> parsed from usageMetadata)
	cas.Interactions = append(cas.Interactions, cassette.Interaction{
		ID:       "int_gemini",
		Sequence: 3,
		Type:     cassette.InteractionTypeHTTP,
		Response: cassette.Response{
			StatusCode: 200,
			RawBody:    `{"modelVersion":"gemini-2.5-flash","usageMetadata":{"promptTokenCount":4000,"candidatesTokenCount":1000,"cachedContentTokenCount":2000}}`,
		},
		Metadata: map[string]string{"model": "gemini-2.5-flash"},
	})

	rep := Evaluate(cas, DefaultPricingTable(), Thresholds{})
	if !rep.Passed {
		t.Fatalf("expected passing report with no thresholds, got violations: %+v", rep.Violations)
	}
	if rep.InputTokens != 7000 {
		t.Errorf("InputTokens=%d, want 7000", rep.InputTokens)
	}
	if rep.OutputTokens != 2500 {
		t.Errorf("OutputTokens=%d, want 2500", rep.OutputTokens)
	}
	if rep.CacheReadInputTokens != 6000 {
		t.Errorf("CacheReadInputTokens=%d, want 6000", rep.CacheReadInputTokens)
	}
	if rep.CacheCreationInputTokens != 1000 {
		t.Errorf("CacheCreationInputTokens=%d, want 1000", rep.CacheCreationInputTokens)
	}
	// CacheHitRate = 6000 / (7000 + 6000 + 1000) = 6000 / 14000
	wantHit := 6000.0 / 14000.0
	if !almostEqual(rep.CacheHitRate, wantHit, 1e-6) {
		t.Errorf("CacheHitRate=%f, want %f", rep.CacheHitRate, wantHit)
	}
	if rep.EstimatedCostUSD <= 0 {
		t.Errorf("expected positive cost, got %f", rep.EstimatedCostUSD)
	}
	if rep.Model != "mixed" {
		t.Errorf("expected model 'mixed', got %q", rep.Model)
	}
}

func TestBudgetThresholdsEnforcement(t *testing.T) {
	cas := cassette.NewCassette("cas_budget_limits")
	cas.Metadata.Model = "claude-opus-4"
	cas.Interactions = append(cas.Interactions, cassette.Interaction{
		ID:       "int_001",
		Sequence: 1,
		Type:     cassette.InteractionTypeHTTP,
		Response: cassette.Response{
			StatusCode: 200,
			Usage: cassette.Usage{
				InputTokens:          10000,
				OutputTokens:         2000,
				CacheReadInputTokens: 2000,
			},
		},
	})

	// Cost = (10000*15 + 2000*75 + 2000*1.5) / 1e6 = (150000 + 150000 + 3000)/1e6 = $0.303
	// TotalTokens = 14000
	// CacheHitRate = 2000 / 12000 = 0.16667
	repPass := Evaluate(cas, DefaultPricingTable(), Thresholds{
		MaxTokens:       20000,
		MaxCostUSD:      0.50,
		MinCacheHitRate: 0.10,
	})
	if !repPass.Passed || len(repPass.Violations) != 0 {
		t.Fatalf("expected budget pass, got: %+v", repPass)
	}
	if !almostEqual(repPass.EstimatedCostUSD, 0.303, 1e-6) {
		t.Fatalf("expected cost $0.303, got $%.6f", repPass.EstimatedCostUSD)
	}

	repFail := Evaluate(cas, DefaultPricingTable(), Thresholds{
		MaxTokens:       10000,
		MaxCostUSD:      0.10,
		MinCacheHitRate: 0.50,
	})
	if repFail.Passed || len(repFail.Violations) != 3 {
		t.Fatalf("expected 3 budget violations, got passed=%v violations=%+v", repFail.Passed, repFail.Violations)
	}
}

func TestGitHubStepSummaryAppend(t *testing.T) {
	dir := t.TempDir()
	sumPath := filepath.Join(dir, "nested", "step_summary.md")

	if got := ResolveSummaryPath(false, sumPath); got != sumPath {
		t.Fatalf("expected explicit path %q, got %q", sumPath, got)
	}
	t.Setenv("GITHUB_STEP_SUMMARY", sumPath)
	if got := ResolveSummaryPath(true, ""); got != sumPath {
		t.Fatalf("expected env GITHUB_STEP_SUMMARY %q, got %q", sumPath, got)
	}

	md := FormatSummaryMarkdown("Verification", []SummaryRow{
		{Name: "fixtures/a.yaml", Passed: true, Interactions: 3, TotalTokens: 1200, CostUSD: 0.012, CacheHitRate: 0.5, Details: "ok"},
	}, Report{InteractionCount: 3, TotalTokens: 1200, EstimatedCostUSD: 0.012, CacheHitRate: 0.5, Passed: true}, true)

	if err := AppendGitHubSummary(sumPath, md); err != nil {
		t.Fatalf("AppendGitHubSummary failed: %v", err)
	}
	raw, err := os.ReadFile(sumPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "## AgentFixture Verification") || !strings.Contains(text, "fixtures/a.yaml") || !strings.Contains(text, "50.0%") {
		t.Fatalf("unexpected GitHub summary markdown:\n%s", text)
	}
}

func TestBudget_ZeroTokensCacheHitRateAndNaNInfValidation(t *testing.T) {
	// 1. Cassette with 0 tokens + MinCacheHitRate 0.5 -> CacheHitRate must be 0 (not NaN) and fail deterministically
	emptyCas := cassette.NewCassette("cas_zero_tokens")
	emptyCas.Interactions = append(emptyCas.Interactions, cassette.Interaction{
		ID:       "int_empty",
		Sequence: 1,
		Type:     cassette.InteractionTypeTool,
		Response: cassette.Response{StatusCode: 0, ToolResult: "ok"},
	})
	rep := Evaluate(emptyCas, DefaultPricingTable(), Thresholds{MinCacheHitRate: 0.5})
	if math.IsNaN(rep.CacheHitRate) || math.IsInf(rep.CacheHitRate, 0) || rep.CacheHitRate != 0 {
		t.Fatalf("expected CacheHitRate=0 on zero-token cassette, got %v", rep.CacheHitRate)
	}
	if rep.Passed || len(rep.Violations) != 1 || !strings.Contains(rep.Violations[0].Message, "no prompt tokens recorded") {
		t.Fatalf("expected crisp min_cache_hit_rate violation on zero-token cassette, got passed=%v violations=%+v", rep.Passed, rep.Violations)
	}

	// 2. Malformed negative token counts are clamped to 0 (never produce negative cost or totals)
	negCas := cassette.NewCassette("cas_neg_tokens")
	negCas.Interactions = append(negCas.Interactions, cassette.Interaction{
		ID:       "int_neg",
		Sequence: 1,
		Type:     cassette.InteractionTypeHTTP,
		Response: cassette.Response{
			StatusCode: 200,
			Usage:      cassette.Usage{InputTokens: -500, OutputTokens: -100, CacheReadInputTokens: -50},
		},
	})
	negRep := Evaluate(negCas, DefaultPricingTable(), Thresholds{})
	if negRep.TotalTokens != 0 || negRep.EstimatedCostUSD != 0 || negRep.CacheHitRate != 0 {
		t.Fatalf("expected negative tokens clamped to 0, got %+v", negRep)
	}

	// 3. Thresholds.Validate rejects NaN, Inf, and out-of-range values
	badThresholds := []Thresholds{
		{MaxTokens: -1},
		{MaxCostUSD: -0.01},
		{MaxCostUSD: math.NaN()},
		{MaxCostUSD: math.Inf(1)},
		{MaxCostUSD: math.Inf(-1)},
		{MinCacheHitRate: -0.1},
		{MinCacheHitRate: 1.01},
		{MinCacheHitRate: math.NaN()},
		{MinCacheHitRate: math.Inf(1)},
	}
	for i, th := range badThresholds {
		if err := th.Validate(); err == nil {
			t.Errorf("threshold case %d (%+v): expected validation error, got nil", i, th)
		}
	}
}

func TestPricingFile_ValidationAndCompoundPrefixNormalization(t *testing.T) {
	// Reject NaN/Inf/negative rates via validatePricingRate
	badRates := []struct {
		name string
		rate ModelPricing
	}{
		{"", ModelPricing{InputPerMillion: 1}},
		{"models/", ModelPricing{InputPerMillion: 1}},
		{"anthropic/", ModelPricing{InputPerMillion: 1}},
		{"m1", ModelPricing{InputPerMillion: -1}},
		{"m2", ModelPricing{OutputPerMillion: math.NaN()}},
		{"m3", ModelPricing{CacheReadPerMillion: math.Inf(1)}},
		{"m4", ModelPricing{CacheWritePerMillion: math.Inf(-1)}},
	}
	for _, tc := range badRates {
		if err := validatePricingRate(tc.name, tc.rate); err == nil {
			t.Errorf("validatePricingRate(%q, %+v): expected error, got nil", tc.name, tc.rate)
		}
	}

	// LoadPricingFile rejects negative prices and JSON null
	dir := t.TempDir()
	negPath := filepath.Join(dir, "neg.json")
	if err := os.WriteFile(negPath, []byte(`{"gpt-5":{"input_per_million":-2.0}}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPricingFile(negPath); err == nil {
		t.Fatalf("expected LoadPricingFile to reject negative price")
	}

	nullPath := filepath.Join(dir, "null.json")
	if err := os.WriteFile(nullPath, []byte(`null`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPricingFile(nullPath); err == nil {
		t.Fatalf("expected LoadPricingFile to reject JSON null")
	}

	// Compound/uppercase provider prefix normalization
	tbl := DefaultPricingTable()
	for _, tc := range []struct {
		raw     string
		wantKey string
	}{
		{"MODELS/GEMINI-2.5-PRO", "gemini-2.5-pro"},
		{"GOOGLE/MODELS/GEMINI-2.5-PRO", "gemini-2.5-pro"},
		{"ANTHROPIC/CLAUDE-SONNET-4-5", "claude-sonnet-4-5"},
		{"projects/my-proj/locations/us-central1/publishers/google/models/gemini-2.5-flash-001", "gemini-2.5-flash"},
		{"OPENAI/GPT-4O-2024-11-20", "gpt-4o"},
	} {
		_, key, ok := tbl.Lookup(tc.raw)
		if !ok || key != tc.wantKey {
			t.Errorf("Lookup(%q) = (%q, %v), want (%q, true)", tc.raw, key, ok, tc.wantKey)
		}
	}
}

func TestGitHubStepSummary_MarkdownEscaping(t *testing.T) {
	md := FormatSummaryMarkdown("Suite\nInjection", []SummaryRow{
		{
			Name:         "dir/case`with|pipe\r\nand\nnewline.yaml",
			Passed:       false,
			Interactions: 1,
			TotalTokens:  10,
			CostUSD:      0.001,
			CacheHitRate: 0,
			Details:      "err | pipe\r\nsecond line",
		},
		{
			Name:         "`leading_backtick`",
			Passed:       true,
			Interactions: 1,
			TotalTokens:  10,
			CostUSD:      0.001,
			CacheHitRate: 0.5,
			Details:      "ok",
		},
	}, Report{
		InteractionCount: 2,
		TotalTokens:      20,
		EstimatedCostUSD: 0.002,
		Passed:           false,
		Violations: []Violation{
			{Metric: "max_tokens", Message: "line1\nline2"},
		},
	}, false)

	// Verify title and every table row stay single-line with escaped pipes and multi-backtick code fences
	if strings.Contains(md, "## AgentFixture Suite\nInjection") {
		t.Fatalf("newline in title was not sanitized:\n%s", md)
	}
	if !strings.Contains(md, "``dir/case`with\\|pipe and newline.yaml``") {
		t.Fatalf("expected multi-backtick fenced and pipe/newline-escaped name cell, got:\n%s", md)
	}
	if !strings.Contains(md, "`` `leading_backtick` ``") {
		t.Fatalf("expected space-padded multi-backtick fence for leading/trailing backticks, got:\n%s", md)
	}
	if !strings.Contains(md, `err \| pipe second line`) {
		t.Fatalf("expected pipe and CRLF escaped in notes cell, got:\n%s", md)
	}
	if strings.Contains(md, "\r") {
		t.Fatalf("markdown summary must not contain stray CR characters:\n%q", md)
	}
}
