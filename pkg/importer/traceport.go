package importer

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
// Redacts all secrets BEFORE computing deterministic StrictHash values so hashes never leak secret oracles.
func ImportTracePortSession(sessionJSON []byte) (*cassette.Cassette, error) {
	var sess TracePortSession
	if err := json.Unmarshal(sessionJSON, &sess); err != nil {
		return nil, fmt.Errorf("failed to parse traceport session JSON: %w", err)
	}

	casID := sess.ID
	if casID == "" {
		casID = fmt.Sprintf("trace_%d", time.Now().UnixNano())
	}

	redactedCommand := cassette.RedactString(sess.Command)

	cas := cassette.NewCassette(casID)
	cas.Metadata.RecordedAt = sess.StartTime
	cas.Metadata.AgentName = "traceport-agent"
	cas.Metadata.Source = "traceport_flight_recording"
	cas.Metadata.Description = fmt.Sprintf("Imported from TracePort flight recording (Command: %s, Status: %s)", redactedCommand, sess.Status)

	if cas.Metadata.Tags == nil {
		cas.Metadata.Tags = make(map[string]string)
	}
	cas.Metadata.Tags["traceport_session_id"] = sess.ID
	cas.Metadata.Tags["command"] = redactedCommand
	cas.Metadata.Tags["status"] = sess.Status

	seq := 1
	n := len(sess.Frames)

	for i := 0; i < n; i++ {
		frame := sess.Frames[i]

		// Record workspace checkpoint if available
		if frame.Type == EventCheckpoint && frame.GitSnapshot != "" {
			cas.Metadata.Tags["git_snapshot"] = frame.GitSnapshot
			continue
		}

		// 1. Handle tool calls (paired with optional tool_result or error frame)
		if frame.Type == EventToolCall && frame.Tool != nil {
			toolName := frame.Tool.Name
			rawArgs := frame.Tool.Arguments
			if rawArgs == nil {
				rawArgs = make(map[string]interface{})
			}
			redactedArgs := cassette.RedactMap(rawArgs)

			toolResult := cassette.RedactString(frame.Tool.Result)
			var errText string
			statusCode := 0
			latency := frame.Tool.DurationMs

			if toolResult == "" && i+1 < n {
				next := sess.Frames[i+1]
				switch next.Type {
				case EventToolResult:
					if next.Tool != nil && next.Tool.Result != "" {
						toolResult = cassette.RedactString(next.Tool.Result)
						if next.Tool.DurationMs > 0 {
							latency = next.Tool.DurationMs
						}
					} else if next.Content != "" {
						toolResult = cassette.RedactString(next.Content)
					}
					i++
				case EventError:
					errText = cassette.RedactString(next.Content)
					statusCode = 500
					if latency == 0 && !next.Timestamp.IsZero() && !frame.Timestamp.IsZero() {
						if d := next.Timestamp.Sub(frame.Timestamp).Milliseconds(); d > 0 {
							latency = d
						}
					}
					i++
				}
			}

			strictHash := matcher.ComputeToolHash(toolName, redactedArgs)

			inter := cassette.Interaction{
				ID:       fmt.Sprintf("tp_step_%d", frame.Index),
				Sequence: seq,
				Type:     cassette.InteractionTypeTool,
				Request: cassette.Request{
					ToolName:  toolName,
					ToolInput: redactedArgs,
					Matcher: cassette.MatcherMeta{
						StrictHash: strictHash,
						FuzzySignature: cassette.FuzzySignature{
							NormalizedToolName: strings.ToLower(toolName),
							NormalizedArgs:     redactedArgs,
						},
					},
				},
				Response: cassette.Response{
					StatusCode: statusCode,
					LatencyMs:  latency,
					ToolResult: toolResult,
					Error:      errText,
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
			continue
		}

		// 2. Paired user input / assistant output (or error response)
		if frame.Type == EventInput && i+1 < n && (sess.Frames[i+1].Type == EventOutput || sess.Frames[i+1].Type == EventError) {
			nextFrame := sess.Frames[i+1]
			redactedInput := cassette.RedactString(frame.Content)
			redactedNext := cassette.RedactString(nextFrame.Content)

			req := cassette.Request{
				RawBody: redactedInput,
			}
			req.Matcher = cassette.MatcherMeta{
				StrictHash: matcher.ComputeRequestHash(&req),
				FuzzySignature: cassette.FuzzySignature{
					NormalizedPrompt: matcher.NormalizeText(redactedInput),
				},
			}

			resp := cassette.Response{
				StatusCode: 200,
				RawBody:    redactedNext,
				LatencyMs:  nextFrame.Timestamp.Sub(frame.Timestamp).Milliseconds(),
			}
			if nextFrame.Type == EventError {
				resp.StatusCode = 500
				resp.RawBody = ""
				resp.Error = redactedNext
			}

			inter := cassette.Interaction{
				ID:       fmt.Sprintf("tp_turn_%d", frame.Index),
				Sequence: seq,
				Type:     cassette.InteractionTypeLLMChat,
				Request:  req,
				Response: resp,
				Metadata: map[string]string{
					"input_index":  fmt.Sprintf("%d", frame.Index),
					"output_index": fmt.Sprintf("%d", nextFrame.Index),
				},
			}
			cas.Interactions = append(cas.Interactions, inter)
			seq++
			i++
			continue
		}

		// 3. Standalone EventError frame (never dropped)
		if frame.Type == EventError {
			redactedErr := cassette.RedactString(frame.Content)
			interType := cassette.InteractionTypeLLMChat
			var req cassette.Request

			if frame.Tool != nil && frame.Tool.Name != "" {
				interType = cassette.InteractionTypeTool
				redArgs := cassette.RedactMap(frame.Tool.Arguments)
				if redArgs == nil {
					redArgs = make(map[string]interface{})
				}
				req = cassette.Request{
					ToolName:  frame.Tool.Name,
					ToolInput: redArgs,
					Matcher: cassette.MatcherMeta{
						StrictHash: matcher.ComputeToolHash(frame.Tool.Name, redArgs),
						FuzzySignature: cassette.FuzzySignature{
							NormalizedToolName: strings.ToLower(frame.Tool.Name),
							NormalizedArgs:     redArgs,
						},
					},
				}
			} else {
				req = cassette.Request{
					RawBody: redactedErr,
				}
				req.Matcher = cassette.MatcherMeta{
					StrictHash: matcher.ComputeRequestHash(&req),
					FuzzySignature: cassette.FuzzySignature{
						NormalizedPrompt: matcher.NormalizeText(redactedErr),
					},
				}
			}

			inter := cassette.Interaction{
				ID:       fmt.Sprintf("tp_err_%d", frame.Index),
				Sequence: seq,
				Type:     interType,
				Request:  req,
				Response: cassette.Response{
					StatusCode: 500,
					Error:      redactedErr,
				},
				Metadata: map[string]string{
					"traceport_frame_index": fmt.Sprintf("%d", frame.Index),
					"event_type":            string(EventError),
					"timestamp":             frame.Timestamp.Format(time.RFC3339Nano),
				},
			}
			cas.Interactions = append(cas.Interactions, inter)
			seq++
		}
	}

	cassette.RedactCassette(cas)
	cas.ComputeMetrics()

	return cas, nil
}

// ImportTracePortFile loads a TracePort session from disk (supporting legacy monolithic .json,
// `traceport inspect --json` output, and TracePort v1.1+ split .meta.json + .frames.jsonl WAL logs),
// converts it, and saves the resulting cassette to outputPath (YAML or JSON).
func ImportTracePortFile(sessionPath, outputPath string) (*cassette.Cassette, error) {
	data, err := resolveTracePortSessionJSON(sessionPath)
	if err != nil {
		return nil, err
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

func resolveTracePortSessionJSON(sessionPath string) ([]byte, error) {
	switch {
	case strings.HasSuffix(sessionPath, ".meta.json"):
		prefix := strings.TrimSuffix(sessionPath, ".meta.json")
		return assembleSplitTracePortSession(sessionPath, prefix+".frames.jsonl")
	case strings.HasSuffix(sessionPath, ".frames.jsonl"):
		prefix := strings.TrimSuffix(sessionPath, ".frames.jsonl")
		return assembleSplitTracePortSession(prefix+".meta.json", sessionPath)
	}

	data, err := os.ReadFile(sessionPath)
	if err == nil {
		return data, nil
	}

	// Fallback: check if TracePort v1.1+ split files (<id>.meta.json + <id>.frames.jsonl) exist.
	prefix := strings.TrimSuffix(sessionPath, ".json")
	if _, mErr := os.Stat(prefix + ".meta.json"); mErr == nil {
		return assembleSplitTracePortSession(prefix+".meta.json", prefix+".frames.jsonl")
	}

	return nil, fmt.Errorf("failed to read traceport session file: %w", err)
}

func assembleSplitTracePortSession(metaPath, framesPath string) ([]byte, error) {
	var sess TracePortSession
	if metaBytes, err := os.ReadFile(metaPath); err == nil {
		if err := json.Unmarshal(metaBytes, &sess); err != nil {
			return nil, fmt.Errorf("failed to parse traceport meta file %s: %w", metaPath, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to read traceport meta file %s: %w", metaPath, err)
	}

	f, err := os.Open(framesPath)
	if err != nil {
		if os.IsNotExist(err) && sess.ID != "" {
			return json.Marshal(sess)
		}
		return nil, fmt.Errorf("failed to read traceport frames file %s: %w", framesPath, err)
	}
	defer f.Close()

	reader := bufio.NewReaderSize(f, 64*1024)
	for {
		line, readErr := reader.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			var frame TracePortFrame
			if err := json.Unmarshal(trimmed, &frame); err != nil {
				return nil, fmt.Errorf("invalid frame line in %s: %w", framesPath, err)
			}
			sess.Frames = append(sess.Frames, frame)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("error reading %s: %w", framesPath, readErr)
		}
	}

	return json.Marshal(sess)
}
