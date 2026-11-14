package cassette

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Anthropic Messages API Server-Sent Event type names.
const (
	SSEEventMessageStart      = "message_start"
	SSEEventContentBlockStart = "content_block_start"
	SSEEventContentBlockDelta = "content_block_delta"
	SSEEventContentBlockStop  = "content_block_stop"
	SSEEventMessageDelta      = "message_delta"
	SSEEventMessageStop       = "message_stop"
	SSEEventPing              = "ping"
	SSEEventError             = "error"
)

// Anthropic Messages API content_block_delta inner delta types.
const (
	SSEDeltaText      = "text_delta"
	SSEDeltaThinking  = "thinking_delta"
	SSEDeltaSignature = "signature_delta"
	SSEDeltaInputJSON = "input_json_delta"
	SSEDeltaCitations = "citations_delta"
)

// OpenAISSEDone is the sentinel data marker terminating OpenAI-compatible SSE streams.
const OpenAISSEDone = "[DONE]"

// SSEEvent captures a single Server-Sent Event frame and its inter-chunk arrival delta.
type SSEEvent struct {
	Index   int    `json:"index" yaml:"index"`
	Event   string `json:"event,omitempty" yaml:"event,omitempty"`
	ID      string `json:"id,omitempty" yaml:"id,omitempty"`
	Retry   int    `json:"retry,omitempty" yaml:"retry,omitempty"`
	Comment string `json:"comment,omitempty" yaml:"comment,omitempty"`
	Data    string `json:"data,omitempty" yaml:"data,omitempty"`
	DeltaMs int64  `json:"delta_ms,omitempty" yaml:"delta_ms,omitempty"`
}

// SSETimingMode controls inter-chunk timing behavior during SSE stream replay.
type SSETimingMode string

const (
	SSETimingInstant SSETimingMode = "instant"
	SSETimingScaled  SSETimingMode = "scaled"
	SSETimingExact   SSETimingMode = "exact"
)

// SSETimingConfig configures chunk pacing when replaying recorded SSE streams.
type SSETimingConfig struct {
	Mode          SSETimingMode
	Scale         float64
	MaxChunkDelay time.Duration
}

// ParseSSETimingMode parses a CLI or configuration string into an SSETimingMode.
func ParseSSETimingMode(raw string) (SSETimingMode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "instant":
		return SSETimingInstant, nil
	case "scaled":
		return SSETimingScaled, nil
	case "exact":
		return SSETimingExact, nil
	default:
		return "", fmt.Errorf("unsupported SSE timing mode %q (expected instant, scaled, or exact)", raw)
	}
}

// ToolUseSummary represents a reconstructed tool invocation assembled from streamed JSON fragments.
type ToolUseSummary struct {
	Index    int                    `json:"index" yaml:"index"`
	ID       string                 `json:"id" yaml:"id"`
	Name     string                 `json:"name" yaml:"name"`
	RawInput string                 `json:"raw_input,omitempty" yaml:"raw_input,omitempty"`
	Input    map[string]interface{} `json:"input,omitempty" yaml:"input,omitempty"`
	ParseErr string                 `json:"parse_error,omitempty" yaml:"parse_error,omitempty"`
}

// StreamSummary aggregates the semantic result of an Anthropic, OpenAI, or Google Gemini SSE stream.
type StreamSummary struct {
	Provider       string           `json:"provider,omitempty" yaml:"provider,omitempty"`
	MessageID      string           `json:"message_id,omitempty" yaml:"message_id,omitempty"`
	Model          string           `json:"model,omitempty" yaml:"model,omitempty"`
	Role           string           `json:"role,omitempty" yaml:"role,omitempty"`
	Text           string           `json:"text,omitempty" yaml:"text,omitempty"`
	ThinkingBlocks []string         `json:"thinking_blocks,omitempty" yaml:"thinking_blocks,omitempty"`
	Signatures     []string         `json:"signatures,omitempty" yaml:"signatures,omitempty"`
	Citations      []interface{}    `json:"citations,omitempty" yaml:"citations,omitempty"`
	ToolCalls      []ToolUseSummary `json:"tool_calls,omitempty" yaml:"tool_calls,omitempty"`
	StopReason     string           `json:"stop_reason,omitempty" yaml:"stop_reason,omitempty"`
	StopSequence   string           `json:"stop_sequence,omitempty" yaml:"stop_sequence,omitempty"`
	Usage          Usage            `json:"usage,omitempty" yaml:"usage,omitempty"`
	ErrorMessage   string           `json:"error_message,omitempty" yaml:"error_message,omitempty"`
	EventCount     int              `json:"event_count" yaml:"event_count"`
	TotalDeltaMs   int64            `json:"total_delta_ms" yaml:"total_delta_ms"`
}

// DecodeSSE parses an SSE byte stream into discrete SSEEvents with zero line-size ceiling.
// DeltaMs is normalized to 0 for deterministic offline decoding.
func DecodeSSE(r io.Reader) ([]SSEEvent, error) {
	events, err := RecordSSEStream(r, nil)
	for i := range events {
		events[i].DeltaMs = 0
	}
	return events, err
}

// RecordSSEStream reads an SSE stream using bufio.Reader (supporting arbitrarily large lines
// well beyond bufio.Scanner's 64KB cap), measures inter-event DeltaMs arrival latency, and
// invokes onEvent immediately upon each completed SSE frame for zero-lag live proxy flushing.
func RecordSSEStream(r io.Reader, onEvent func(SSEEvent) error) ([]SSEEvent, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var events []SSEEvent

	var (
		curEvent    string
		curID       string
		curRetry    int
		curComments []string
		curData     []string
		hasFields   bool
	)

	lastTick := time.Now()

	flushEvent := func() error {
		if !hasFields {
			return nil
		}
		now := time.Now()
		delta := now.Sub(lastTick).Milliseconds()
		if delta < 0 {
			delta = 0
		}
		lastTick = now

		ev := SSEEvent{
			Index:   len(events),
			Event:   curEvent,
			ID:      curID,
			Retry:   curRetry,
			Comment: strings.Join(curComments, "\n"),
			Data:    strings.Join(curData, "\n"),
			DeltaMs: delta,
		}
		events = append(events, ev)

		curEvent = ""
		curID = ""
		curRetry = 0
		curComments = curComments[:0]
		curData = curData[:0]
		hasFields = false

		if onEvent != nil {
			return onEvent(ev)
		}
		return nil
	}

	processLine := func(rawLine string) error {
		line := strings.TrimRight(rawLine, "\r\n")
		if line == "" {
			return flushEvent()
		}

		// Comment line (": ...")
		if strings.HasPrefix(line, ":") {
			comment := line[1:]
			if strings.HasPrefix(comment, " ") {
				comment = comment[1:]
			}
			curComments = append(curComments, comment)
			hasFields = true
			return nil
		}

		field := line
		value := ""
		if colonIdx := strings.IndexByte(line, ':'); colonIdx != -1 {
			field = line[:colonIdx]
			value = line[colonIdx+1:]
			// Per WHATWG SSE spec, strip at most one single leading U+0020 SPACE after colon.
			if strings.HasPrefix(value, " ") {
				value = value[1:]
			}
		}

		switch field {
		case "event":
			curEvent = value
			hasFields = true
		case "data":
			curData = append(curData, value)
			hasFields = true
		case "id":
			if !strings.ContainsRune(value, '\x00') {
				curID = value
				hasFields = true
			}
		case "retry":
			if ms, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && ms >= 0 {
				curRetry = ms
				hasFields = true
			}
		default:
			// Per SSE specification, unknown field names are ignored.
		}
		return nil
	}

	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			if pErr := processLine(line); pErr != nil {
				return events, pErr
			}
		}
		if err != nil {
			if err == io.EOF {
				if fErr := flushEvent(); fErr != nil {
					return events, fErr
				}
				return events, nil
			}
			return events, err
		}
	}
}

// EncodeSSEEvent formats a single SSEEvent into standard wire-protocol SSE bytes.
func EncodeSSEEvent(ev SSEEvent) []byte {
	var buf bytes.Buffer
	encodeSSEEventInto(&buf, ev)
	return buf.Bytes()
}

// EncodeSSEStream encodes a slice of SSEEvents into a contiguous SSE byte stream.
func EncodeSSEStream(events []SSEEvent) []byte {
	var buf bytes.Buffer
	for _, ev := range events {
		encodeSSEEventInto(&buf, ev)
	}
	return buf.Bytes()
}

func encodeSSEEventInto(buf *bytes.Buffer, ev SSEEvent) {
	wroteAny := false
	if ev.Comment != "" {
		normComment := strings.ReplaceAll(ev.Comment, "\r\n", "\n")
		for _, line := range strings.Split(normComment, "\n") {
			buf.WriteString(": ")
			buf.WriteString(line)
			buf.WriteByte('\n')
		}
		wroteAny = true
	}
	if ev.Event != "" {
		buf.WriteString("event: ")
		buf.WriteString(ev.Event)
		buf.WriteByte('\n')
		wroteAny = true
	}
	if ev.ID != "" {
		buf.WriteString("id: ")
		buf.WriteString(ev.ID)
		buf.WriteByte('\n')
		wroteAny = true
	}
	if ev.Retry > 0 {
		buf.WriteString("retry: ")
		buf.WriteString(strconv.Itoa(ev.Retry))
		buf.WriteByte('\n')
		wroteAny = true
	}
	if ev.Data != "" || !wroteAny {
		normData := strings.ReplaceAll(ev.Data, "\r\n", "\n")
		for _, line := range strings.Split(normData, "\n") {
			buf.WriteString("data: ")
			buf.WriteString(line)
			buf.WriteByte('\n')
		}
	}
	buf.WriteByte('\n')
}

// ReplaySSEStream writes recorded SSE events to w with optional per-chunk flushing and timing simulation.
// Honors ctx.Done() promptly without leaking timers.
func ReplaySSEStream(ctx context.Context, w io.Writer, flusher http.Flusher, events []SSEEvent, cfg SSETimingConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	var timer *time.Timer
	defer func() {
		if timer != nil {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
	}()

	var buf bytes.Buffer
	for _, ev := range events {
		if err := ctx.Err(); err != nil {
			return err
		}

		delay := computeChunkDelay(ev.DeltaMs, cfg)
		if delay > 0 {
			if timer == nil {
				timer = time.NewTimer(delay)
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(delay)
			}

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
			}
		}

		buf.Reset()
		encodeSSEEventInto(&buf, ev)
		if _, err := w.Write(buf.Bytes()); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
	}

	return nil
}

func computeChunkDelay(deltaMs int64, cfg SSETimingConfig) time.Duration {
	if deltaMs <= 0 {
		return 0
	}
	var d time.Duration
	switch cfg.Mode {
	case SSETimingInstant, "":
		return 0
	case SSETimingScaled:
		if cfg.Scale <= 0 {
			return 0
		}
		d = time.Duration(float64(deltaMs) * cfg.Scale * float64(time.Millisecond))
	case SSETimingExact:
		d = time.Duration(deltaMs) * time.Millisecond
	default:
		return 0
	}
	if cfg.MaxChunkDelay > 0 && d > cfg.MaxChunkDelay {
		d = cfg.MaxChunkDelay
	}
	return d
}

type activeAnthropicBlock struct {
	index       int
	blockType   string
	toolID      string
	toolName    string
	initInput   map[string]interface{}
	textBuf     strings.Builder
	thinkingBuf strings.Builder
	sigBuf      strings.Builder
	jsonBuf     strings.Builder
	closed      bool
}

type openAIToolAcc struct {
	index   int
	id      string
	name    string
	argsBuf strings.Builder
}

// ReconstructStreamSummary reassembles an Anthropic Messages API, OpenAI-compatible, or
// Google Gemini (:streamGenerateContent?alt=sse) SSE stream into a complete structured
// StreamSummary (assistant/model text, thinking/thought blocks, signatures, citations,
// tool/function calls, stop/finish reason, and Usage).
func ReconstructStreamSummary(events []SSEEvent) StreamSummary {
	summary := StreamSummary{
		EventCount: len(events),
	}
	if len(events) == 0 {
		return summary
	}

	var fullText strings.Builder
	blocks := make(map[int]*activeAnthropicBlock)
	var blockOrder []int

	openAITools := make(map[int]*openAIToolAcc)
	var openAIToolOrder []int

	var (
		geminiThoughtBuf    strings.Builder
		hasGeminiThought    bool
		geminiToolIndex     int
		geminiCandTokens    int
		geminiThoughtTokens int
	)

	flushGeminiThought := func() {
		if !hasGeminiThought {
			return
		}
		if geminiThoughtBuf.Len() > 0 {
			summary.ThinkingBlocks = append(summary.ThinkingBlocks, geminiThoughtBuf.String())
		}
		geminiThoughtBuf.Reset()
		hasGeminiThought = false
	}

	getOrCreateBlock := func(idx int) *activeAnthropicBlock {
		b, exists := blocks[idx]
		if !exists {
			b = &activeAnthropicBlock{index: idx}
			blocks[idx] = b
			blockOrder = append(blockOrder, idx)
		}
		return b
	}

	finalizeBlock := func(b *activeAnthropicBlock) {
		if b == nil || b.closed {
			return
		}
		b.closed = true
		switch b.blockType {
		case "thinking":
			if b.thinkingBuf.Len() > 0 {
				summary.ThinkingBlocks = append(summary.ThinkingBlocks, b.thinkingBuf.String())
			}
			if b.sigBuf.Len() > 0 {
				summary.Signatures = append(summary.Signatures, b.sigBuf.String())
			}
		case "tool_use", "server_tool_use":
			rawJSON := strings.TrimSpace(b.jsonBuf.String())
			tc := ToolUseSummary{
				Index:    b.index,
				ID:       b.toolID,
				Name:     b.toolName,
				RawInput: rawJSON,
			}
			if rawJSON != "" {
				parsed, err := decodeJSONMapUseNumber([]byte(rawJSON))
				if err != nil {
					tc.ParseErr = err.Error()
				} else {
					tc.Input = parsed
				}
			} else if b.initInput != nil {
				tc.Input = b.initInput
			} else {
				tc.Input = map[string]interface{}{}
			}
			summary.ToolCalls = append(summary.ToolCalls, tc)
		}
	}

	for _, ev := range events {
		summary.TotalDeltaMs += ev.DeltaMs
		trimmedData := strings.TrimSpace(ev.Data)
		if trimmedData == "" {
			continue
		}
		if trimmedData == OpenAISSEDone {
			if summary.Provider == "" {
				summary.Provider = "openai"
			}
			continue
		}

		rawMap, err := decodeJSONMapUseNumber([]byte(trimmedData))
		if err != nil {
			continue
		}

		evType := ev.Event
		if evType == "" {
			if t, ok := rawMap["type"].(string); ok {
				evType = t
			}
		}

		switch evType {
		case SSEEventPing:
			if summary.Provider == "" {
				summary.Provider = "anthropic"
			}
			continue

		case SSEEventMessageStart:
			summary.Provider = "anthropic"
			if msg, ok := rawMap["message"].(map[string]interface{}); ok {
				if id, ok := msg["id"].(string); ok {
					summary.MessageID = id
				}
				if model, ok := msg["model"].(string); ok {
					summary.Model = model
				}
				if role, ok := msg["role"].(string); ok {
					summary.Role = role
				}
				if sr, ok := msg["stop_reason"].(string); ok && sr != "" {
					summary.StopReason = sr
				}
				if uMap, ok := msg["usage"].(map[string]interface{}); ok {
					mergeAnthropicUsage(&summary.Usage, uMap)
				}
			}

		case SSEEventContentBlockStart:
			summary.Provider = "anthropic"
			idx := asInt(rawMap["index"])
			b := getOrCreateBlock(idx)
			if cb, ok := rawMap["content_block"].(map[string]interface{}); ok {
				if bt, ok := cb["type"].(string); ok {
					b.blockType = bt
				}
				switch b.blockType {
				case "text":
					if txt, ok := cb["text"].(string); ok && txt != "" {
						b.textBuf.WriteString(txt)
						fullText.WriteString(txt)
					}
				case "thinking":
					if th, ok := cb["thinking"].(string); ok && th != "" {
						b.thinkingBuf.WriteString(th)
					}
					if sig, ok := cb["signature"].(string); ok && sig != "" {
						b.sigBuf.WriteString(sig)
					}
				case "tool_use", "server_tool_use":
					if id, ok := cb["id"].(string); ok {
						b.toolID = id
					}
					if name, ok := cb["name"].(string); ok {
						b.toolName = name
					}
					if inp, ok := cb["input"].(map[string]interface{}); ok && len(inp) > 0 {
						b.initInput = inp
					}
				}
			}

		case SSEEventContentBlockDelta:
			summary.Provider = "anthropic"
			idx := asInt(rawMap["index"])
			b := getOrCreateBlock(idx)
			if delta, ok := rawMap["delta"].(map[string]interface{}); ok {
				dtype, _ := delta["type"].(string)
				switch dtype {
				case SSEDeltaText:
					if b.blockType == "" {
						b.blockType = "text"
					}
					if txt, ok := delta["text"].(string); ok {
						b.textBuf.WriteString(txt)
						fullText.WriteString(txt)
					}
				case SSEDeltaThinking:
					if b.blockType == "" {
						b.blockType = "thinking"
					}
					if th, ok := delta["thinking"].(string); ok {
						b.thinkingBuf.WriteString(th)
					}
				case SSEDeltaSignature:
					if b.blockType == "" {
						b.blockType = "thinking"
					}
					if sig, ok := delta["signature"].(string); ok {
						b.sigBuf.WriteString(sig)
					}
				case SSEDeltaInputJSON:
					if b.blockType == "" {
						b.blockType = "tool_use"
					}
					if pj, ok := delta["partial_json"].(string); ok {
						b.jsonBuf.WriteString(pj)
					}
				case SSEDeltaCitations:
					if cit, exists := delta["citation"]; exists && cit != nil {
						summary.Citations = append(summary.Citations, cit)
					}
				}
			}

		case SSEEventContentBlockStop:
			summary.Provider = "anthropic"
			idx := asInt(rawMap["index"])
			if b, ok := blocks[idx]; ok {
				finalizeBlock(b)
			}

		case SSEEventMessageDelta:
			summary.Provider = "anthropic"
			if delta, ok := rawMap["delta"].(map[string]interface{}); ok {
				if sr, ok := delta["stop_reason"].(string); ok && sr != "" {
					summary.StopReason = sr
				}
				if ss, ok := delta["stop_sequence"].(string); ok && ss != "" {
					summary.StopSequence = ss
				}
			}
			if uMap, ok := rawMap["usage"].(map[string]interface{}); ok {
				mergeAnthropicUsage(&summary.Usage, uMap)
			}

		case SSEEventMessageStop:
			summary.Provider = "anthropic"

		case SSEEventError:
			if errObj, ok := rawMap["error"].(map[string]interface{}); ok {
				msg, _ := errObj["message"].(string)
				errType, _ := errObj["type"].(string)
				if errType != "" && msg != "" {
					summary.ErrorMessage = errType + ": " + msg
				} else if msg != "" {
					summary.ErrorMessage = msg
				} else {
					summary.ErrorMessage = errType
				}
			} else if msg, ok := rawMap["error"].(string); ok {
				summary.ErrorMessage = msg
			}

		default:
			// Check OpenAI Chat Completions / Responses streaming chunks
			if choices, ok := rawMap["choices"].([]interface{}); ok {
				summary.Provider = "openai"
				if id, ok := rawMap["id"].(string); ok && id != "" {
					summary.MessageID = id
				}
				if model, ok := rawMap["model"].(string); ok && model != "" {
					summary.Model = model
				}
				for _, cItem := range choices {
					cMap, ok := cItem.(map[string]interface{})
					if !ok {
						continue
					}
					if fr, ok := cMap["finish_reason"].(string); ok && fr != "" {
						summary.StopReason = fr
					}
					if delta, ok := cMap["delta"].(map[string]interface{}); ok {
						if role, ok := delta["role"].(string); ok && role != "" {
							summary.Role = role
						}
						if content, ok := delta["content"].(string); ok {
							fullText.WriteString(content)
						}
						if reasoning, ok := delta["reasoning_content"].(string); ok && reasoning != "" {
							b := getOrCreateBlock(-1)
							b.blockType = "thinking"
							b.thinkingBuf.WriteString(reasoning)
						}
						if tcs, ok := delta["tool_calls"].([]interface{}); ok {
							for _, tcItem := range tcs {
								tcMap, ok := tcItem.(map[string]interface{})
								if !ok {
									continue
								}
								tcIdx := asInt(tcMap["index"])
								acc, exists := openAITools[tcIdx]
								if !exists {
									acc = &openAIToolAcc{index: tcIdx}
									openAITools[tcIdx] = acc
									openAIToolOrder = append(openAIToolOrder, tcIdx)
								}
								if id, ok := tcMap["id"].(string); ok && id != "" {
									acc.id = id
								}
								if fn, ok := tcMap["function"].(map[string]interface{}); ok {
									if name, ok := fn["name"].(string); ok && name != "" {
										acc.name = name
									}
									if argsPart, ok := fn["arguments"].(string); ok {
										acc.argsBuf.WriteString(argsPart)
									}
								}
							}
						}
					}
				}
			}
			if uMap, ok := rawMap["usage"].(map[string]interface{}); ok {
				mergeOpenAIOrAnthropicUsage(&summary.Usage, uMap)
			}

			// Check Google Gemini (:streamGenerateContent?alt=sse) streaming chunks
			_, hasCandidates := rawMap["candidates"]
			_, hasUsageMeta := rawMap["usageMetadata"]
			_, hasModelVer := rawMap["modelVersion"]
			if hasCandidates || hasUsageMeta || hasModelVer {
				summary.Provider = "gemini"
				if summary.Role == "" {
					summary.Role = "model"
				}
				if mv, ok := rawMap["modelVersion"].(string); ok && mv != "" {
					summary.Model = mv
				}
				if rid, ok := rawMap["responseId"].(string); ok && rid != "" {
					summary.MessageID = rid
				}
				if cands, ok := rawMap["candidates"].([]interface{}); ok && len(cands) > 0 {
					if cand, ok := cands[0].(map[string]interface{}); ok {
						if fr, ok := cand["finishReason"].(string); ok && fr != "" {
							summary.StopReason = fr
						}
						if content, ok := cand["content"].(map[string]interface{}); ok {
							if role, ok := content["role"].(string); ok && role != "" {
								summary.Role = role
							}
							if parts, ok := content["parts"].([]interface{}); ok {
								for _, partItem := range parts {
									part, ok := partItem.(map[string]interface{})
									if !ok {
										continue
									}
									isThought, _ := part["thought"].(bool)
									if isThought {
										if txt, ok := part["text"].(string); ok && txt != "" {
											geminiThoughtBuf.WriteString(txt)
											hasGeminiThought = true
										}
									} else {
										if txt, ok := part["text"].(string); ok {
											flushGeminiThought()
											fullText.WriteString(txt)
										}
									}
									if fc, ok := part["functionCall"].(map[string]interface{}); ok {
										flushGeminiThought()
										tc := extractGeminiFunctionCall(geminiToolIndex, fc)
										geminiToolIndex++
										summary.ToolCalls = append(summary.ToolCalls, tc)
									}
									if sig, ok := part["thoughtSignature"].(string); ok && sig != "" {
										summary.Signatures = append(summary.Signatures, sig)
									}
								}
							}
						}
					}
				}
				if uMeta, ok := rawMap["usageMetadata"].(map[string]interface{}); ok {
					mergeGeminiUsage(&summary.Usage, uMeta, &geminiCandTokens, &geminiThoughtTokens)
				}
			}

			if errObj, ok := rawMap["error"].(map[string]interface{}); ok && summary.ErrorMessage == "" {
				msg, _ := errObj["message"].(string)
				errType, _ := errObj["type"].(string)
				if errType == "" {
					errType, _ = errObj["status"].(string)
				}
				if errType != "" && msg != "" {
					summary.ErrorMessage = errType + ": " + msg
				} else if msg != "" {
					summary.ErrorMessage = msg
				} else {
					summary.ErrorMessage = errType
				}
			}
		}
	}

	// Finalize any Anthropic blocks that remained open (e.g. truncated stream)
	sort.Ints(blockOrder)
	for _, idx := range blockOrder {
		finalizeBlock(blocks[idx])
	}

	// Finalize any trailing Google Gemini thought block
	flushGeminiThought()

	// Finalize any OpenAI streamed tool calls
	sort.Ints(openAIToolOrder)
	for _, idx := range openAIToolOrder {
		acc := openAITools[idx]
		rawArgs := strings.TrimSpace(acc.argsBuf.String())
		tc := ToolUseSummary{
			Index:    acc.index,
			ID:       acc.id,
			Name:     acc.name,
			RawInput: rawArgs,
		}
		if rawArgs != "" {
			parsed, err := decodeJSONMapUseNumber([]byte(rawArgs))
			if err != nil {
				tc.ParseErr = err.Error()
			} else {
				tc.Input = parsed
			}
		} else {
			tc.Input = map[string]interface{}{}
		}
		summary.ToolCalls = append(summary.ToolCalls, tc)
	}

	summary.Text = fullText.String()
	return summary
}

func extractGeminiFunctionCall(idx int, fc map[string]interface{}) ToolUseSummary {
	name, _ := fc["name"].(string)
	id, _ := fc["id"].(string)
	tc := ToolUseSummary{
		Index: idx,
		ID:    id,
		Name:  name,
	}
	switch args := fc["args"].(type) {
	case map[string]interface{}:
		tc.Input = args
		if b, err := json.Marshal(args); err == nil {
			tc.RawInput = string(b)
		}
	case string:
		trimmed := strings.TrimSpace(args)
		tc.RawInput = trimmed
		if trimmed != "" {
			if parsed, err := decodeJSONMapUseNumber([]byte(trimmed)); err == nil {
				tc.Input = parsed
			} else {
				tc.ParseErr = err.Error()
			}
		} else {
			tc.Input = map[string]interface{}{}
		}
	default:
		tc.Input = map[string]interface{}{}
	}
	return tc
}

func mergeGeminiUsage(dst *Usage, uMeta map[string]interface{}, candTokens *int, thoughtTokens *int) {
	if v, ok := uMeta["promptTokenCount"]; ok {
		if n := asInt(v); n > 0 || dst.InputTokens == 0 {
			dst.InputTokens = n
		}
	}
	updatedOut := false
	if v, ok := uMeta["candidatesTokenCount"]; ok {
		if n := asInt(v); n > 0 || *candTokens == 0 {
			*candTokens = n
			updatedOut = true
		}
	}
	if v, ok := uMeta["thoughtsTokenCount"]; ok {
		if n := asInt(v); n > 0 || *thoughtTokens == 0 {
			*thoughtTokens = n
			updatedOut = true
		}
	}
	if updatedOut {
		out := *candTokens + *thoughtTokens
		if out > 0 || dst.OutputTokens == 0 {
			dst.OutputTokens = out
		}
	}
	if v, ok := uMeta["cachedContentTokenCount"]; ok {
		if n := asInt(v); n > 0 || dst.CacheReadInputTokens == 0 {
			dst.CacheReadInputTokens = n
		}
	}
	if v, ok := uMeta["totalTokenCount"]; ok {
		if n := asInt(v); n > 0 || dst.TotalTokens == 0 {
			dst.TotalTokens = n
		}
	}
}

func mergeAnthropicUsage(dst *Usage, uMap map[string]interface{}) {
	if v, ok := uMap["input_tokens"]; ok {
		if n := asInt(v); n > 0 || dst.InputTokens == 0 {
			dst.InputTokens = n
		}
	}
	if v, ok := uMap["output_tokens"]; ok {
		// In Anthropic SSE, message_delta usage.output_tokens is cumulative total output tokens.
		if n := asInt(v); n > 0 || dst.OutputTokens == 0 {
			dst.OutputTokens = n
		}
	}
	if v, ok := uMap["cache_creation_input_tokens"]; ok {
		if n := asInt(v); n > 0 || dst.CacheCreationInputTokens == 0 {
			dst.CacheCreationInputTokens = n
		}
	}
	if v, ok := uMap["cache_read_input_tokens"]; ok {
		if n := asInt(v); n > 0 || dst.CacheReadInputTokens == 0 {
			dst.CacheReadInputTokens = n
		}
	}
	if v, ok := uMap["total_tokens"]; ok {
		if n := asInt(v); n > 0 {
			dst.TotalTokens = n
		}
	}
}

func mergeOpenAIOrAnthropicUsage(dst *Usage, uMap map[string]interface{}) {
	mergeAnthropicUsage(dst, uMap)
	if v, ok := uMap["prompt_tokens"]; ok {
		dst.InputTokens = asInt(v)
	}
	if v, ok := uMap["completion_tokens"]; ok {
		dst.OutputTokens = asInt(v)
	}
}

// ExtractUsageFromBody extracts Anthropic, OpenAI, or Google Gemini Usage statistics
// from a decoded JSON response body map.
func ExtractUsageFromBody(body interface{}) Usage {
	var u Usage
	m, ok := body.(map[string]interface{})
	if !ok {
		return u
	}
	if uMap, ok := m["usage"].(map[string]interface{}); ok {
		mergeOpenAIOrAnthropicUsage(&u, uMap)
	}
	if uMeta, ok := m["usageMetadata"].(map[string]interface{}); ok {
		var cand, thoughts int
		mergeGeminiUsage(&u, uMeta, &cand, &thoughts)
	}
	return u
}

// ExtractModelFromBody extracts the provider model identifier ("model" or Gemini "modelVersion")
// from a decoded JSON response body map.
func ExtractModelFromBody(body interface{}) string {
	m, ok := body.(map[string]interface{})
	if !ok {
		return ""
	}
	if model, ok := m["model"].(string); ok && model != "" {
		return model
	}
	if mv, ok := m["modelVersion"].(string); ok && mv != "" {
		return mv
	}
	return ""
}

// ExtractResponseUsage returns recorded Usage or reconstructs it from SSEEvents,
// structured JSON Body, or JSON RawBody (Anthropic, OpenAI, and Google Gemini).
func ExtractResponseUsage(resp *Response) Usage {
	if resp == nil {
		return Usage{}
	}
	if !resp.Usage.IsZero() {
		return resp.Usage
	}
	if len(resp.SSEEvents) > 0 {
		if u := ReconstructStreamSummary(resp.SSEEvents).Usage; !u.IsZero() {
			return u
		}
	}
	if resp.Body != nil {
		if u := ExtractUsageFromBody(resp.Body); !u.IsZero() {
			return u
		}
	}
	if raw := strings.TrimSpace(resp.RawBody); len(raw) > 0 && raw[0] == '{' {
		if parsed, err := decodeJSONMapUseNumber([]byte(raw)); err == nil {
			if u := ExtractUsageFromBody(parsed); !u.IsZero() {
				return u
			}
		}
	}
	return Usage{}
}

func decodeJSONMapUseNumber(data []byte) (map[string]interface{}, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var raw map[string]interface{}
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	norm, _ := normalizeDecodedNumbers(raw).(map[string]interface{})
	return norm, nil
}

func normalizeDecodedNumbers(v interface{}) interface{} {
	switch val := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(val))
		for k, el := range val {
			out[k] = normalizeDecodedNumbers(el)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(val))
		for i, el := range val {
			out[i] = normalizeDecodedNumbers(el)
		}
		return out
	case json.Number:
		s := val.String()
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i
		}
		if u, err := strconv.ParseUint(s, 10, 64); err == nil {
			return u
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			if !math.IsInf(f, 0) && !math.IsNaN(f) && math.Trunc(f) == f &&
				f >= -9007199254740992 && f <= 9007199254740992 {
				return int64(f)
			}
			return f
		}
		return s
	default:
		return val
	}
}

func asInt(v interface{}) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case uint64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
		if f, err := n.Float64(); err == nil {
			return int(f)
		}
	}
	return 0
}
