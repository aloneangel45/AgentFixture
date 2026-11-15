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

// TracePortTags decodes TracePort session tags emitted either as a JSON string array
// (e.g. `["ci", "agent"]`, `["env=staging"]`) or as a legacy JSON string map (`{"env":"staging"}`).
type TracePortTags map[string]string

// UnmarshalJSON implements json.Unmarshaler for both []string and map[string]string tag payloads.
func (t *TracePortTags) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*t = nil
		return nil
	}

	switch trimmed[0] {
	case '{':
		var m map[string]string
		if err := json.Unmarshal(trimmed, &m); err != nil {
			return err
		}
		*t = TracePortTags(m)
		return nil
	case '[':
		var arr []string
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return err
		}
		m := make(map[string]string, len(arr)+1)
		bare := make([]string, 0, len(arr))
		for _, raw := range arr {
			s := strings.TrimSpace(raw)
			if s == "" {
				continue
			}
			if eq := strings.IndexByte(s, '='); eq > 0 && !strings.Contains(s[:eq], " ") && !strings.Contains(s, "://") {
				k := cassette.RedactString(strings.TrimSpace(s[:eq]))
				v := strings.TrimSpace(s[eq+1:])
				if k != "" {
					m[k] = v
				}
				continue
			}
			clean := cassette.RedactString(s)
			m[clean] = "true"
			bare = append(bare, clean)
		}
		if len(bare) > 0 {
			if _, exists := m["tags"]; !exists {
				m["tags"] = strings.Join(bare, ",")
			}
		}
		*t = TracePortTags(m)
		return nil
	default:
		return fmt.Errorf("unsupported traceport tags JSON format (expected array or object)")
	}
}

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
	Tags      TracePortTags    `json:"tags,omitempty"`
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
	for k, v := range sess.Tags {
		cas.Metadata.Tags[k] = v
	}
	cas.Metadata.Tags["traceport_session_id"] = sess.ID
	cas.Metadata.Tags["command"] = redactedCommand
	cas.Metadata.Tags["status"] = sess.Status

	usedIDs := make(map[string]bool, len(sess.Frames))
	allocInteractionID := func(base string) string {
		if !usedIDs[base] {
			usedIDs[base] = true
			return base
		}
		for suffix := 2; ; suffix++ {
			candidate := fmt.Sprintf("%s_%d", base, suffix)
			if !usedIDs[candidate] {
				usedIDs[candidate] = true
				return candidate
			}
		}
	}

	seq := 1
	n := len(sess.Frames)
	lastTimestamp := sess.StartTime

	for i := 0; i < n; i++ {
		frame := sess.Frames[i]

		// Record workspace checkpoint if available
		if frame.Type == EventCheckpoint {
			if frame.GitSnapshot != "" {
				cas.Metadata.Tags["git_snapshot"] = frame.GitSnapshot
			}
			if !frame.Timestamp.IsZero() {
				lastTimestamp = frame.Timestamp
			}
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
			endTS := frame.Timestamp

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
					if !next.Timestamp.IsZero() {
						endTS = next.Timestamp
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
					if !next.Timestamp.IsZero() {
						endTS = next.Timestamp
					}
					i++
				}
			}

			if latency < 0 {
				latency = 0
			}
			if !endTS.IsZero() {
				lastTimestamp = endTS
			}
			strictHash := matcher.ComputeToolHash(toolName, redactedArgs)

			inter := cassette.Interaction{
				ID:       allocInteractionID(fmt.Sprintf("tp_step_%d", frame.Index)),
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

			turnLatency := int64(0)
			if !nextFrame.Timestamp.IsZero() && !frame.Timestamp.IsZero() {
				if d := nextFrame.Timestamp.Sub(frame.Timestamp).Milliseconds(); d > 0 {
					turnLatency = d
				}
			}
			if !nextFrame.Timestamp.IsZero() {
				lastTimestamp = nextFrame.Timestamp
			} else if !frame.Timestamp.IsZero() {
				lastTimestamp = frame.Timestamp
			}

			resp := cassette.Response{
				StatusCode: 200,
				RawBody:    redactedNext,
				LatencyMs:  turnLatency,
			}
			if nextFrame.Type == EventError {
				resp.StatusCode = 500
				resp.RawBody = ""
				resp.Error = redactedNext
			}

			inter := cassette.Interaction{
				ID:       allocInteractionID(fmt.Sprintf("tp_turn_%d", frame.Index)),
				Sequence: seq,
				Type:     cassette.InteractionTypeLLMChat,
				Request:  req,
				Response: resp,
				Metadata: map[string]string{
					"input_index":  fmt.Sprintf("%d", frame.Index),
					"output_index": fmt.Sprintf("%d", nextFrame.Index),
				},
			}
			if nextFrame.GitSnapshot != "" {
				inter.Metadata["git_snapshot"] = nextFrame.GitSnapshot
			} else if frame.GitSnapshot != "" {
				inter.Metadata["git_snapshot"] = frame.GitSnapshot
			}
			cas.Interactions = append(cas.Interactions, inter)
			seq++
			i++
			continue
		}

		// 3. Standalone EventOutput frame (e.g. non-interactive CLI execution captured via `traceport record`)
		if frame.Type == EventOutput {
			redactedOut := cassette.RedactString(frame.Content)
			latency := int64(0)
			if !frame.Timestamp.IsZero() && !lastTimestamp.IsZero() {
				if d := frame.Timestamp.Sub(lastTimestamp).Milliseconds(); d > 0 {
					latency = d
				}
			}
			if !frame.Timestamp.IsZero() {
				lastTimestamp = frame.Timestamp
			}

			interType := cassette.InteractionTypeStdio
			var req cassette.Request
			var resp cassette.Response

			if frame.Tool != nil && frame.Tool.Name != "" {
				interType = cassette.InteractionTypeTool
				redArgs := cassette.RedactMap(frame.Tool.Arguments)
				if redArgs == nil {
					redArgs = make(map[string]interface{})
				}
				if frame.Tool.DurationMs > 0 {
					latency = frame.Tool.DurationMs
				}
				outText := redactedOut
				if outText == "" && frame.Tool.Result != "" {
					outText = cassette.RedactString(frame.Tool.Result)
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
				resp = cassette.Response{
					StatusCode: 0,
					LatencyMs:  latency,
					ToolResult: outText,
				}
			} else {
				reqBody := redactedCommand
				if reqBody == "" {
					reqBody = redactedOut
				}
				req = cassette.Request{
					RawBody: reqBody,
				}
				req.Matcher = cassette.MatcherMeta{
					StrictHash: matcher.ComputeRequestHash(&req),
					FuzzySignature: cassette.FuzzySignature{
						NormalizedPrompt: matcher.NormalizeText(reqBody),
					},
				}
				resp = cassette.Response{
					StatusCode: 200,
					LatencyMs:  latency,
					RawBody:    redactedOut,
				}
			}

			meta := map[string]string{
				"traceport_frame_index": fmt.Sprintf("%d", frame.Index),
				"event_type":            string(EventOutput),
				"timestamp":             frame.Timestamp.Format(time.RFC3339Nano),
			}
			if frame.GitSnapshot != "" {
				meta["git_snapshot"] = frame.GitSnapshot
			}

			inter := cassette.Interaction{
				ID:       allocInteractionID(fmt.Sprintf("tp_out_%d", frame.Index)),
				Sequence: seq,
				Type:     interType,
				Request:  req,
				Response: resp,
				Metadata: meta,
			}
			cas.Interactions = append(cas.Interactions, inter)
			seq++
			continue
		}

		// 4. Standalone EventError frame (never dropped)
		if frame.Type == EventError {
			redactedErr := cassette.RedactString(frame.Content)
			latency := int64(0)
			if !frame.Timestamp.IsZero() && !lastTimestamp.IsZero() {
				if d := frame.Timestamp.Sub(lastTimestamp).Milliseconds(); d > 0 {
					latency = d
				}
			}
			if !frame.Timestamp.IsZero() {
				lastTimestamp = frame.Timestamp
			}

			interType := cassette.InteractionTypeLLMChat
			var req cassette.Request

			if frame.Tool != nil && frame.Tool.Name != "" {
				interType = cassette.InteractionTypeTool
				redArgs := cassette.RedactMap(frame.Tool.Arguments)
				if redArgs == nil {
					redArgs = make(map[string]interface{})
				}
				if frame.Tool.DurationMs > 0 {
					latency = frame.Tool.DurationMs
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
				reqBody := redactedCommand
				if reqBody == "" {
					reqBody = redactedErr
				}
				req = cassette.Request{
					RawBody: reqBody,
				}
				req.Matcher = cassette.MatcherMeta{
					StrictHash: matcher.ComputeRequestHash(&req),
					FuzzySignature: cassette.FuzzySignature{
						NormalizedPrompt: matcher.NormalizeText(reqBody),
					},
				}
			}

			meta := map[string]string{
				"traceport_frame_index": fmt.Sprintf("%d", frame.Index),
				"event_type":            string(EventError),
				"timestamp":             frame.Timestamp.Format(time.RFC3339Nano),
			}
			if frame.GitSnapshot != "" {
				meta["git_snapshot"] = frame.GitSnapshot
			}

			inter := cassette.Interaction{
				ID:       allocInteractionID(fmt.Sprintf("tp_err_%d", frame.Index)),
				Sequence: seq,
				Type:     interType,
				Request:  req,
				Response: cassette.Response{
					StatusCode: 500,
					LatencyMs:  latency,
					Error:      redactedErr,
				},
				Metadata: meta,
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
