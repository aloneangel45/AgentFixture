package budget

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
	"github.com/aloneangel45/AgentFixture/pkg/matcher"
)

// Thresholds specifies optional CI budget limits. Zero values disable the corresponding assertion.
type Thresholds struct {
	MaxTokens       int64   `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
	MaxCostUSD      float64 `json:"max_cost_usd,omitempty" yaml:"max_cost_usd,omitempty"`
	MinCacheHitRate float64 `json:"min_cache_hit_rate,omitempty" yaml:"min_cache_hit_rate,omitempty"`
}

// HasAssertions reports whether at least one budget limit is active.
func (t Thresholds) HasAssertions() bool {
	return t.MaxTokens > 0 || t.MaxCostUSD > 0 || t.MinCacheHitRate > 0
}

// Validate checks that threshold bounds are non-negative and cache hit rate is in [0.0, 1.0].
func (t Thresholds) Validate() error {
	if t.MaxTokens < 0 {
		return fmt.Errorf("max-tokens must be >= 0 (got %d)", t.MaxTokens)
	}
	if t.MaxCostUSD < 0 {
		return fmt.Errorf("max-cost-usd must be >= 0 (got %.6f)", t.MaxCostUSD)
	}
	if t.MinCacheHitRate < 0 || t.MinCacheHitRate > 1.0 {
		return fmt.Errorf("min-cache-hit-rate must be between 0.0 and 1.0 (got %v)", t.MinCacheHitRate)
	}
	return nil
}

// Violation describes a single budget threshold breach.
type Violation struct {
	Metric  string  `json:"metric"`
	Actual  float64 `json:"actual"`
	Limit   float64 `json:"limit"`
	Message string  `json:"message"`
}

// Report contains aggregated token usage, cost, prompt cache hit rate, and budget assertion status.
type Report struct {
	Model                    string      `json:"model,omitempty"`
	InputTokens              int64       `json:"input_tokens"`
	OutputTokens             int64       `json:"output_tokens"`
	CacheCreationInputTokens int64       `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64       `json:"cache_read_input_tokens"`
	TotalTokens              int64       `json:"total_tokens"`
	EstimatedCostUSD         float64     `json:"estimated_cost_usd"`
	CacheHitRate             float64     `json:"cache_hit_rate"`
	InteractionCount         int         `json:"interaction_count"`
	Passed                   bool        `json:"passed"`
	Violations               []Violation `json:"violations,omitempty"`
}

// Evaluate aggregates usage, cost, and prompt cache hit rate across all interactions in cas
// and checks them against thresholds.
func Evaluate(cas *cassette.Cassette, table *PricingTable, thresholds Thresholds) Report {
	return EvaluateSubset(cas, nil, table, thresholds)
}

// EvaluateSubset aggregates usage across interactions in cas (optionally filtered by filterIDs when non-nil)
// and enforces thresholds.
func EvaluateSubset(cas *cassette.Cassette, filterIDs map[string]bool, table *PricingTable, thresholds Thresholds) Report {
	if table == nil {
		table = DefaultPricingTable()
	}
	rep := Report{Passed: true}
	if cas == nil {
		applyThresholds(&rep, thresholds)
		return rep
	}

	defaultModel := strings.TrimSpace(cas.Metadata.Model)
	seenModels := make(map[string]bool)

	for i := range cas.Interactions {
		inter := &cas.Interactions[i]
		if filterIDs != nil && !filterIDs[inter.ID] {
			continue
		}
		rep.InteractionCount++

		model := resolveInteractionModel(inter, defaultModel)
		if model != "" {
			seenModels[model] = true
			if rep.Model == "" {
				rep.Model = model
			}
		}

		u := cassette.ExtractResponseUsage(&inter.Response)
		rep.InputTokens += int64(u.InputTokens)
		rep.OutputTokens += int64(u.OutputTokens)
		rep.CacheCreationInputTokens += int64(u.CacheCreationInputTokens)
		rep.CacheReadInputTokens += int64(u.CacheReadInputTokens)

		hasGranular := u.InputTokens > 0 || u.OutputTokens > 0 ||
			u.CacheCreationInputTokens > 0 || u.CacheReadInputTokens > 0

		pricing, _, _ := table.Lookup(model)
		if hasGranular {
			interTotal := int64(u.TotalTokens)
			if interTotal == 0 {
				interTotal = int64(u.InputTokens + u.OutputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens)
			}
			rep.TotalTokens += interTotal
			rep.EstimatedCostUSD += (float64(u.InputTokens)*pricing.InputPerMillion +
				float64(u.OutputTokens)*pricing.OutputPerMillion +
				float64(u.CacheReadInputTokens)*pricing.CacheReadPerMillion +
				float64(u.CacheCreationInputTokens)*pricing.CacheWritePerMillion) / 1_000_000.0
		} else if u.TotalTokens > 0 {
			rep.TotalTokens += int64(u.TotalTokens)
			blended := (pricing.InputPerMillion + pricing.OutputPerMillion) / 2.0
			rep.EstimatedCostUSD += (float64(u.TotalTokens) * blended) / 1_000_000.0
		}
	}

	if len(seenModels) > 1 {
		rep.Model = "mixed"
	} else if rep.Model == "" {
		rep.Model = defaultModel
	}

	rep.CacheHitRate = ComputeCacheHitRate(rep.InputTokens, rep.CacheReadInputTokens, rep.CacheCreationInputTokens)
	applyThresholds(&rep, thresholds)
	return rep
}

// ComputeCacheHitRate calculates CacheRead / (Input + CacheRead + CacheCreation), returning 0 when denominator is 0.
func ComputeCacheHitRate(inputTokens, cacheReadTokens, cacheCreationTokens int64) float64 {
	denom := inputTokens + cacheReadTokens + cacheCreationTokens
	if denom <= 0 || cacheReadTokens <= 0 {
		return 0
	}
	rate := float64(cacheReadTokens) / float64(denom)
	if rate > 1.0 {
		rate = 1.0
	}
	return rate
}

// Aggregate sums multiple Reports and evaluates thresholds against the combined totals.
func Aggregate(reports []Report, thresholds Thresholds) Report {
	agg := Report{Passed: true}
	seenModels := make(map[string]bool)
	for _, r := range reports {
		agg.InputTokens += r.InputTokens
		agg.OutputTokens += r.OutputTokens
		agg.CacheCreationInputTokens += r.CacheCreationInputTokens
		agg.CacheReadInputTokens += r.CacheReadInputTokens
		agg.TotalTokens += r.TotalTokens
		agg.EstimatedCostUSD += r.EstimatedCostUSD
		agg.InteractionCount += r.InteractionCount
		if r.Model != "" {
			seenModels[r.Model] = true
			if agg.Model == "" {
				agg.Model = r.Model
			}
		}
	}
	if len(seenModels) > 1 {
		agg.Model = "mixed"
	}
	agg.CacheHitRate = ComputeCacheHitRate(agg.InputTokens, agg.CacheReadInputTokens, agg.CacheCreationInputTokens)
	applyThresholds(&agg, thresholds)
	return agg
}

func applyThresholds(rep *Report, t Thresholds) {
	rep.Violations = nil
	if t.MaxTokens > 0 && rep.TotalTokens > t.MaxTokens {
		rep.Violations = append(rep.Violations, Violation{
			Metric:  "max_tokens",
			Actual:  float64(rep.TotalTokens),
			Limit:   float64(t.MaxTokens),
			Message: fmt.Sprintf("total tokens %d exceeded max-tokens limit %d", rep.TotalTokens, t.MaxTokens),
		})
	}
	if t.MaxCostUSD > 0 && rep.EstimatedCostUSD > t.MaxCostUSD {
		rep.Violations = append(rep.Violations, Violation{
			Metric:  "max_cost_usd",
			Actual:  rep.EstimatedCostUSD,
			Limit:   t.MaxCostUSD,
			Message: fmt.Sprintf("estimated cost $%.6f exceeded max-cost-usd limit $%.6f", rep.EstimatedCostUSD, t.MaxCostUSD),
		})
	}
	if t.MinCacheHitRate > 0 && rep.CacheHitRate < t.MinCacheHitRate {
		rep.Violations = append(rep.Violations, Violation{
			Metric:  "min_cache_hit_rate",
			Actual:  rep.CacheHitRate,
			Limit:   t.MinCacheHitRate,
			Message: fmt.Sprintf("prompt cache hit rate %.1f%% below min-cache-hit-rate %.1f%%", rep.CacheHitRate*100, t.MinCacheHitRate*100),
		})
	}
	rep.Passed = len(rep.Violations) == 0
}

// ViolationSummary joins all violation messages into a single human-readable error string.
func (r Report) ViolationSummary() string {
	if len(r.Violations) == 0 {
		return ""
	}
	parts := make([]string, len(r.Violations))
	for i, v := range r.Violations {
		parts[i] = v.Message
	}
	return strings.Join(parts, "; ")
}

func resolveInteractionModel(inter *cassette.Interaction, fallback string) string {
	if inter == nil {
		return fallback
	}
	if inter.Metadata != nil {
		if m := strings.TrimSpace(inter.Metadata["model"]); m != "" {
			return m
		}
	}
	if m := strings.TrimSpace(inter.Request.Matcher.FuzzySignature.Model); m != "" {
		return m
	}
	if len(inter.Response.SSEEvents) > 0 {
		if m := strings.TrimSpace(cassette.ReconstructStreamSummary(inter.Response.SSEEvents).Model); m != "" {
			return m
		}
	}
	if inter.Response.Body != nil {
		if m := strings.TrimSpace(cassette.ExtractModelFromBody(inter.Response.Body)); m != "" {
			return m
		}
	}
	if _, m := matcher.ExtractPromptAndModel(&inter.Request); strings.TrimSpace(m) != "" {
		return strings.TrimSpace(m)
	}
	return fallback
}

// SummaryRow represents one row in a GitHub Actions Step Summary Markdown table.
type SummaryRow struct {
	Name         string
	Passed       bool
	Interactions int
	TotalTokens  int64
	CostUSD      float64
	CacheHitRate float64
	Details      string
}

// ResolveSummaryPath returns explicitPath if non-empty, else GITHUB_STEP_SUMMARY when enabled is true.
func ResolveSummaryPath(enabled bool, explicitPath string) string {
	if p := strings.TrimSpace(explicitPath); p != "" {
		return p
	}
	if enabled {
		return strings.TrimSpace(os.Getenv("GITHUB_STEP_SUMMARY"))
	}
	return ""
}

// FormatSummaryMarkdown builds a GitHub-flavored Markdown telemetry & pass/fail table.
func FormatSummaryMarkdown(title string, rows []SummaryRow, agg Report, overallPassed bool) string {
	var sb strings.Builder
	badge := "PASS"
	if !overallPassed {
		badge = "FAIL"
	}
	sb.WriteString(fmt.Sprintf("## AgentFixture %s — `%s`\n\n", title, badge))
	sb.WriteString("| Case / Cassette | Status | Interactions | Tokens | Est. Cost (USD) | Cache Hit | Notes |\n")
	sb.WriteString("|---|---|---:|---:|---:|---:|---|\n")
	for _, r := range rows {
		st := "PASS"
		if !r.Passed {
			st = "FAIL"
		}
		notes := strings.ReplaceAll(strings.TrimSpace(r.Details), "|", "\\|")
		notes = strings.ReplaceAll(notes, "\n", " ")
		name := strings.ReplaceAll(r.Name, "|", "\\|")
		sb.WriteString(fmt.Sprintf("| `%s` | **%s** | %d | %d | $%.4f | %.1f%% | %s |\n",
			name, st, r.Interactions, r.TotalTokens, r.CostUSD, r.CacheHitRate*100, notes))
	}
	sb.WriteString(fmt.Sprintf("| **Total** | **%s** | **%d** | **%d** | **$%.4f** | **%.1f%%** | |\n",
		badge, agg.InteractionCount, agg.TotalTokens, agg.EstimatedCostUSD, agg.CacheHitRate*100))

	if len(agg.Violations) > 0 {
		sb.WriteString("\n### Budget Violations\n")
		for _, v := range agg.Violations {
			sb.WriteString(fmt.Sprintf("- **%s**: %s\n", v.Metric, v.Message))
		}
	}
	sb.WriteByte('\n')
	return sb.String()
}

// AppendGitHubSummary appends markdown to path (creating parent directories if needed).
func AppendGitHubSummary(path string, markdown string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create summary directory %q: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open GitHub summary file %q: %w", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(markdown); err != nil {
		return fmt.Errorf("failed to write GitHub summary file %q: %w", path, err)
	}
	return nil
}
