package cassette

import (
	"errors"
	"fmt"
	"time"
)

// CurrentVersion is the active schema version for AgentFixture cassettes.
const CurrentVersion = "agentfixture/v1"

// InteractionType identifies the category of recorded interaction.
type InteractionType string

const (
	InteractionTypeLLMChat InteractionType = "llm_chat"
	InteractionTypeTool    InteractionType = "tool_execution"
	InteractionTypeHTTP    InteractionType = "http"
	InteractionTypeStdio   InteractionType = "stdio"
)

// Cassette is the top-level container encapsulating recorded agent interactions.
type Cassette struct {
	Version      string        `json:"version" yaml:"version"`
	Metadata     Metadata      `json:"metadata" yaml:"metadata"`
	Interactions []Interaction `json:"interactions" yaml:"interactions"`
}

// Metadata holds provenance, telemetry, and accounting metrics for a cassette.
type Metadata struct {
	CassetteID  string            `json:"cassette_id" yaml:"cassette_id"`
	RecordedAt  time.Time         `json:"recorded_at" yaml:"recorded_at"`
	Generator   string            `json:"generator,omitempty" yaml:"generator,omitempty"`
	AgentName   string            `json:"agent_name,omitempty" yaml:"agent_name,omitempty"`
	Model       string            `json:"model,omitempty" yaml:"model,omitempty"`
	Source      string            `json:"source,omitempty" yaml:"source,omitempty"`
	Metrics     Metrics           `json:"metrics" yaml:"metrics"`
	Tags        map[string]string `json:"tags,omitempty" yaml:"tags,omitempty"`
	Description string            `json:"description,omitempty" yaml:"description,omitempty"`
}

// Metrics tracks cumulative savings and efficiency from offline replay.
type Metrics struct {
	TotalInteractions     int     `json:"total_interactions" yaml:"total_interactions"`
	RecordedDurationMs    int64   `json:"recorded_duration_ms" yaml:"recorded_duration_ms"`
	EstimatedTokensSaved  int64   `json:"estimated_tokens_saved" yaml:"estimated_tokens_saved"`
	EstimatedCostSavedUSD float64 `json:"estimated_cost_saved_usd" yaml:"estimated_cost_saved_usd"`
}

// Interaction records a single paired request and response transaction.
type Interaction struct {
	ID       string            `json:"id" yaml:"id"`
	Sequence int               `json:"sequence" yaml:"sequence"`
	Type     InteractionType   `json:"type" yaml:"type"`
	Request  Request           `json:"request" yaml:"request"`
	Response Response          `json:"response" yaml:"response"`
	Metadata map[string]string `json:"metadata,omitempty" yaml:"metadata,omitempty"`
}

// Request captures the incoming tool or LLM query payload.
type Request struct {
	Method    string                 `json:"method,omitempty" yaml:"method,omitempty"`
	Endpoint  string                 `json:"endpoint,omitempty" yaml:"endpoint,omitempty"`
	Headers   map[string]string      `json:"headers,omitempty" yaml:"headers,omitempty"`
	Body      interface{}            `json:"body,omitempty" yaml:"body,omitempty"`
	RawBody   string                 `json:"raw_body,omitempty" yaml:"raw_body,omitempty"`
	ToolName  string                 `json:"tool_name,omitempty" yaml:"tool_name,omitempty"`
	ToolInput map[string]interface{} `json:"tool_input,omitempty" yaml:"tool_input,omitempty"`
	Matcher   MatcherMeta            `json:"matcher,omitempty" yaml:"matcher,omitempty"`
}

// MatcherMeta stores deterministic hashes and normalized semantic signatures.
type MatcherMeta struct {
	StrictHash     string         `json:"strict_hash,omitempty" yaml:"strict_hash,omitempty"`
	FuzzySignature FuzzySignature `json:"fuzzy_signature,omitempty" yaml:"fuzzy_signature,omitempty"`
}

// FuzzySignature encapsulates normalized representations used by the fuzzy matcher.
type FuzzySignature struct {
	NormalizedPrompt   string                 `json:"normalized_prompt,omitempty" yaml:"normalized_prompt,omitempty"`
	NormalizedToolName string                 `json:"normalized_tool_name,omitempty" yaml:"normalized_tool_name,omitempty"`
	NormalizedArgs     map[string]interface{} `json:"normalized_args,omitempty" yaml:"normalized_args,omitempty"`
	Model              string                 `json:"model,omitempty" yaml:"model,omitempty"`
}

// Response captures the recorded result, HTTP status, or tool output.
type Response struct {
	StatusCode int               `json:"status_code" yaml:"status_code"`
	LatencyMs  int64             `json:"latency_ms,omitempty" yaml:"latency_ms,omitempty"`
	Headers    map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
	Body       interface{}       `json:"body,omitempty" yaml:"body,omitempty"`
	RawBody    string            `json:"raw_body,omitempty" yaml:"raw_body,omitempty"`
	ToolResult string            `json:"tool_result,omitempty" yaml:"tool_result,omitempty"`
	Error      string            `json:"error,omitempty" yaml:"error,omitempty"`
	Usage      Usage             `json:"usage,omitempty" yaml:"usage,omitempty"`
}

// Usage tracks token counts reported by the upstream model provider.
type Usage struct {
	InputTokens  int `json:"input_tokens,omitempty" yaml:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty" yaml:"output_tokens,omitempty"`
	TotalTokens  int `json:"total_tokens,omitempty" yaml:"total_tokens,omitempty"`
}

// NewCassette creates an initialized, schema-compliant Cassette instance.
func NewCassette(cassetteID string) *Cassette {
	return &Cassette{
		Version: CurrentVersion,
		Metadata: Metadata{
			CassetteID: cassetteID,
			RecordedAt: time.Now().UTC(),
			Generator:  "AgentFixture v1.0.0",
			Tags:       make(map[string]string),
		},
		Interactions: make([]Interaction, 0),
	}
}

// Validate ensures cassette structure and required fields comply with agentfixture/v1.
func (c *Cassette) Validate() error {
	if c.Version == "" {
		return errors.New("cassette version is required")
	}
	if c.Metadata.CassetteID == "" {
		return errors.New("cassette metadata.cassette_id is required")
	}

	seenIDs := make(map[string]bool)
	for i, inter := range c.Interactions {
		if inter.ID == "" {
			return fmt.Errorf("interaction at index %d missing id", i)
		}
		if seenIDs[inter.ID] {
			return fmt.Errorf("duplicate interaction id '%s' at index %d", inter.ID, i)
		}
		seenIDs[inter.ID] = true

		if inter.Type == "" {
			return fmt.Errorf("interaction '%s' missing type", inter.ID)
		}
	}
	return nil
}

// ComputeMetrics recalculates interaction totals and estimated savings.
// Estimates cost based on Anthropic Claude Sonnet standard rates ($3/M in, $15/M out).
func (c *Cassette) ComputeMetrics() {
	var totalTokens int64
	var totalLatency int64
	for _, inter := range c.Interactions {
		totalLatency += inter.Response.LatencyMs
		if inter.Response.Usage.TotalTokens > 0 {
			totalTokens += int64(inter.Response.Usage.TotalTokens)
		} else {
			// Heuristic estimation: ~4 chars per token for text
			bodyLen := len(inter.Request.RawBody) + len(inter.Response.RawBody) + len(inter.Response.ToolResult)
			if bodyLen > 0 {
				totalTokens += int64(bodyLen / 4)
			}
		}
	}

	c.Metadata.Metrics.TotalInteractions = len(c.Interactions)
	c.Metadata.Metrics.RecordedDurationMs = totalLatency
	c.Metadata.Metrics.EstimatedTokensSaved = totalTokens
	// $3 per million input tokens, $15 per million output tokens -> avg approx $6 per million tokens
	c.Metadata.Metrics.EstimatedCostSavedUSD = float64(totalTokens) * 0.000006
}
