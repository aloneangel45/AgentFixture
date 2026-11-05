package importer

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
	"github.com/aloneangel45/AgentFixture/pkg/matcher"
)

// TracePortEventType mirrors TracePort model event types.
type TracePortEventType string

const (
	EventInput      TracePortEventType = "input"
	EventOutput     TracePortEventType = "output"
	EventToolCall   TracePortEventType = "tool_call"
	EventToolResult TracePortEventType = "tool_result"
	EventError      TracePortEventType = "error"
	EventCheckpoint TracePortEventType = "checkpoint"
)

// TracePortToolInvocation represents the tool structure in TracePort frames.
type TracePortToolInvocation struct {
	Name       string                 `json:"name"`
	Arguments  map[string]interface{} `json:"arguments"`
	Result     string                 `json:"result,omitempty"`
	DurationMs int64                  `json:"duration_ms,omitempty"`
}

// TracePortFrame mirrors a single execution frame from TracePort.
type TracePortFrame struct {
	Index       int                      `json:"index"`
	Timestamp   time.Time                `json:"timestamp"`
	Type        TracePortEventType       `json:"type"`
	Content     string                   `json:"content"`
	Tool        *TracePortToolInvocation `json:"tool,omitempty"`
	GitSnapshot string                   `json:"git_snapshot,omitempty"`
}

// TracePortSession represents the top-level flight recording JSON session from TracePort.
type TracePortSession struct {
	ID        string           `json:"id"`
	Command   string           `json:"command"`
	StartTime time.Time        `json:"start_time"`
	EndTime   time.Time        `json:"end_time,omitempty"`
	Status    string           `json:"status"`
	Frames    []TracePortFrame `json:"frames"`
}

// ImportTracePortSession converts raw TracePort flight recording JSON bytes into a clean Cassette.
func ImportTracePortSession(sessionJSON []byte) (*cassette.Cassette, error) {
	var sess TracePortSession
	if err := json.Unmarshal(sessionJSON, &sess); err != nil {
		return nil, fmt.Errorf("failed to parse traceport session JSON: %w", err)
	}

	casID := sess.ID
	if casID == "" {
		casID = fmt.Sprintf("trace_%d", time.Now().UnixNano())
	}

	cas := cassette.NewCassette(casID)
	cas.Metadata.RecordedAt = sess.StartTime
	cas.Metadata.AgentName = "traceport-agent"
	cas.Metadata.Source = "traceport_flight_recording"
	cas.Metadata.Description = fmt.Sprintf("Imported from TracePort flight recording (Command: %s, Status: %s)", sess.Command, sess.Status)

	if cas.Metadata.Tags == nil {
		cas.Metadata.Tags = make(map[string]string)
	}
	cas.Metadata.Tags["traceport_session_id"] = sess.ID
	cas.Metadata.Tags["command"] = sess.Command
	cas.Metadata.Tags["status"] = sess.Status

	// Map frames into interactions
	seq := 1
	n := len(sess.Frames)

	for i := 0; i < n; i++ {
		frame := sess.Frames[i]

		// Record initial workspace checkpoint if available
		if frame.Type == EventCheckpoint && frame.GitSnapshot != "" {
			cas.Metadata.Tags["git_snapshot"] = frame.GitSnapshot
			continue
		}

		// Handle tool calls
		if frame.Type == EventToolCall && frame.Tool != nil {
			toolName := frame.Tool.Name
			toolArgs := frame.Tool.Arguments
			if toolArgs == nil {
				toolArgs = make(map[string]interface{})
			}

			// Check if result is directly present or in a subsequent tool_result frame
			toolResult := frame.Tool.Result
			latency := frame.Tool.DurationMs

			if toolResult == "" && i+1 < n {
				next := sess.Frames[i+1]
				if next.Type == EventToolResult {
					if next.Tool != nil && next.Tool.Result != "" {
						toolResult = next.Tool.Result
						if next.Tool.DurationMs > 0 {
							latency = next.Tool.DurationMs
						}
					} else if next.Content != "" {
						toolResult = next.Content
					}
				}
			}

			strictHash := matcher.ComputeToolHash(toolName, toolArgs)

			inter := cassette.Interaction{
				ID:       fmt.Sprintf("tp_step_%d", frame.Index),
				Sequence: seq,
				Type:     cassette.InteractionTypeTool,
				Request: cassette.Request{
					ToolName:  toolName,
					ToolInput: toolArgs,
					Matcher: cassette.MatcherMeta{
						StrictHash: strictHash,
						FuzzySignature: cassette.FuzzySignature{
							NormalizedToolName: strings.ToLower(toolName),
							NormalizedArgs:     toolArgs,
						},
					},
				},
				Response: cassette.Response{
					StatusCode: 0,
					LatencyMs:  latency,
					ToolResult: toolResult,
				},
				Metadata: map[string]string{
					"traceport_frame_index": fmt.Sprintf("%d", frame.Index),
					"timestamp":             frame.Timestamp.Format(time.RFC3339Nano),
				},
			}

			if frame.GitSnapshot != "" {
				inter.Metadata["git_snapshot"] = frame.GitSnapshot
			}

			cas.Interactions = append(cas.Interactions, inter)
			seq++
		} else if frame.Type == EventInput && i+1 < n && sess.Frames[i+1].Type == EventOutput {
			// Paired user input / assistant output
			outputFrame := sess.Frames[i+1]
			inter := cassette.Interaction{
				ID:       fmt.Sprintf("tp_turn_%d", frame.Index),
				Sequence: seq,
				Type:     cassette.InteractionTypeLLMChat,
				Request: cassette.Request{
					RawBody: frame.Content,
					Matcher: cassette.MatcherMeta{
						StrictHash: matcher.CanonicalHash([]byte(frame.Content)),
						FuzzySignature: cassette.FuzzySignature{
							NormalizedPrompt: matcher.NormalizeText(frame.Content),
						},
					},
				},
				Response: cassette.Response{
					StatusCode: 200,
					RawBody:    outputFrame.Content,
					LatencyMs:  outputFrame.Timestamp.Sub(frame.Timestamp).Milliseconds(),
				},
				Metadata: map[string]string{
					"input_index":  fmt.Sprintf("%d", frame.Index),
					"output_index": fmt.Sprintf("%d", outputFrame.Index),
				},
			}
			cas.Interactions = append(cas.Interactions, inter)
			seq++
			i++ // skip the paired output frame
		}
	}

	// Redact sensitive secrets from flight log interactions
	cassette.RedactCassette(cas)
	cas.ComputeMetrics()

	return cas, nil
}

// ImportTracePortFile loads a TracePort session JSON file from disk, converts it,
// and saves the resulting cassette to the requested output path (YAML or JSON).
func ImportTracePortFile(sessionPath, outputPath string) (*cassette.Cassette, error) {
	data, err := os.ReadFile(sessionPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read traceport session file: %w", err)
	}

	cas, err := ImportTracePortSession(data)
	if err != nil {
		return nil, err
	}

	if outputPath != "" {
		if err := cassette.Save(outputPath, cas); err != nil {
			return nil, fmt.Errorf("failed to save imported cassette to %s: %w", outputPath, err)
		}
	}

	return cas, nil
}
