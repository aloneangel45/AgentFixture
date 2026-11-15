package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
	"github.com/aloneangel45/AgentFixture/pkg/matcher"
)

// MaxStdioMessageBytes is the maximum supported MCP stdio frame or line size (16 MB).
const MaxStdioMessageBytes = 16 * 1024 * 1024

type stdioFraming int

const (
	framingNDJSON stdioFraming = iota
	framingContentLength
)

// JSONRPCRequest models a standard JSON-RPC 2.0 / MCP invocation or notification message.
type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// MCPToolParams represents tool call parameters in MCP stdio messages.
type MCPToolParams struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

// JSONRPCResponse models a standard JSON-RPC 2.0 response.
type JSONRPCResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   interface{} `json:"error,omitempty"`
}

// StdioReplayer evaluates NDJSON or Content-Length framed JSON-RPC / MCP tool invocations
// and streams deterministic cassette responses to an output writer.
type StdioReplayer struct {
	mu         sync.Mutex
	cas        *cassette.Cassette
	matcher    matcher.Matcher
	matchedIDs map[string]bool
}

// NewStdioReplayer creates an initialized StdioReplayer.
func NewStdioReplayer(cas *cassette.Cassette, m matcher.Matcher) *StdioReplayer {
	if m == nil {
		m = matcher.NewStrictMatcher()
	}
	if cas == nil {
		cas = cassette.NewCassette("cas_stdio_empty")
	}
	return &StdioReplayer{
		cas:        cas,
		matcher:    m,
		matchedIDs: make(map[string]bool),
	}
}

// MatchedCount returns how many unique cassette interactions have been replayed.
func (sr *StdioReplayer) MatchedCount() int {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	return len(sr.matchedIDs)
}

// MatchedIDs returns a thread-safe snapshot of matched cassette interaction IDs.
func (sr *StdioReplayer) MatchedIDs() map[string]bool {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	cp := make(map[string]bool, len(sr.matchedIDs))
	for k, v := range sr.matchedIDs {
		cp[k] = v
	}
	return cp
}

// ProcessStream reads NDJSON or Content-Length framed messages up to 16MB and writes cassette responses.
func (sr *StdioReplayer) ProcessStream(in io.Reader, out io.Writer) error {
	br := bufio.NewReaderSize(in, 64*1024)
	for {
		payload, framing, err := readStdioFrame(br)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		trimmed := strings.TrimSpace(string(payload))
		if trimmed == "" {
			continue
		}

		rawBytes := []byte(trimmed)

		// 1. JSON-RPC 2.0 Batch array (`[...]`)
		if rawBytes[0] == '[' {
			var batchElems []json.RawMessage
			if json.Unmarshal(rawBytes, &batchElems) == nil {
				if len(batchElems) == 0 {
					inv := newInvalidRequestResponse(nil, "Invalid Request: empty batch")
					b, mErr := json.Marshal(inv)
					if mErr != nil {
						return mErr
					}
					if wErr := writeStdioFrame(out, b, framing); wErr != nil {
						return wErr
					}
					continue
				}

				responses := make([]JSONRPCResponse, 0, len(batchElems))
				for _, elem := range batchElems {
					elemTrimmed := bytes.TrimSpace(elem)
					if len(elemTrimmed) == 0 || elemTrimmed[0] != '{' {
						responses = append(responses, newInvalidRequestResponse(nil, "Invalid Request: batch item must be an object"))
						continue
					}
					var elemObj map[string]json.RawMessage
					if json.Unmarshal(elemTrimmed, &elemObj) != nil {
						responses = append(responses, newInvalidRequestResponse(nil, "Invalid Request: malformed batch item"))
						continue
					}
					resp, shouldReply := sr.processJSONRPCObject(elemTrimmed, elemObj)
					if shouldReply {
						responses = append(responses, resp)
					}
				}

				// Per JSON-RPC 2.0 Section 6: if the batch contains only Notifications, nothing is returned.
				if len(responses) > 0 {
					b, mErr := json.Marshal(responses)
					if mErr != nil {
						return mErr
					}
					if wErr := writeStdioFrame(out, b, framing); wErr != nil {
						return wErr
					}
				}
				continue
			}
		}

		// 2. Single JSON-RPC 2.0 object (`{...}`)
		if rawBytes[0] == '{' {
			var rawObj map[string]json.RawMessage
			if json.Unmarshal(rawBytes, &rawObj) == nil {
				_, hasJSONRPC := rawObj["jsonrpc"]
				_, hasMethod := rawObj["method"]
				if hasJSONRPC || hasMethod {
					rpcReq, decErr := decodeJSONRPCRequest(rawBytes)
					if hasJSONRPC || (decErr == nil && rpcReq.Method != "") {
						resp, shouldReply := sr.processJSONRPCObject(rawBytes, rawObj)
						if shouldReply {
							b, mErr := json.Marshal(resp)
							if mErr != nil {
								return mErr
							}
							if wErr := writeStdioFrame(out, b, framing); wErr != nil {
								return wErr
							}
						}
						continue
					}
				}
			}
		}

		// 3. Fallback: raw line or non-JSON-RPC tool invocation
		req := &cassette.Request{
			RawBody: trimmed,
		}
		redactedReq := req.Clone()
		cassette.RedactRequest(&redactedReq)
		redactedReq.Matcher.StrictHash = matcher.ComputeRequestHash(&redactedReq)

		inter := sr.findMatch(&redactedReq)
		if inter == nil && redactedReq.RawBody != req.RawBody {
			req.Matcher.StrictHash = matcher.ComputeRequestHash(req)
			inter = sr.findMatch(req)
		}
		if inter != nil {
			result := inter.Response.ToolResult
			if result == "" {
				result = inter.Response.RawBody
			}
			if err := writeStdioFrame(out, []byte(result), framing); err != nil {
				return err
			}
		} else {
			errPayload, _ := json.Marshal(map[string]string{
				"error": "No cassette interaction matched input: " + cassette.RedactString(trimmed),
			})
			if err := writeStdioFrame(out, errPayload, framing); err != nil {
				return err
			}
		}
	}
}

func newInvalidRequestResponse(id interface{}, msg string) JSONRPCResponse {
	return JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: map[string]interface{}{
			"code":    -32600,
			"message": msg,
		},
	}
}

func isValidNonNullRPCID(idRaw json.RawMessage, decodedID interface{}) bool {
	trimmed := bytes.TrimSpace(idRaw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || decodedID == nil {
		return false
	}
	switch decodedID.(type) {
	case string, json.Number, float64, int, int64:
		return true
	default:
		return false
	}
}

func sanitizeReplyID(decodedID interface{}) interface{} {
	switch decodedID.(type) {
	case string, json.Number, float64, int, int64:
		return decodedID
	default:
		return nil
	}
}

func (sr *StdioReplayer) processJSONRPCObject(rawBytes []byte, rawObj map[string]json.RawMessage) (JSONRPCResponse, bool) {
	rpcReq, decErr := decodeJSONRPCRequest(rawBytes)
	idRaw, hasID := rawObj["id"]

	if decErr != nil || rpcReq.Method == "" {
		return newInvalidRequestResponse(sanitizeReplyID(rpcReq.ID), "Invalid Request"), true
	}

	// JSON-RPC 2.0 Notification: omitted "id" member MUST NOT receive a response.
	if !hasID {
		return JSONRPCResponse{}, false
	}

	// Explicit "id": null or non-scalar "id" in a request -> return JSON-RPC 2.0 -32600 Invalid Request with "id": null.
	if !isValidNonNullRPCID(idRaw, rpcReq.ID) {
		return newInvalidRequestResponse(nil, "Invalid Request: request id must be a non-null string or number"), true
	}

	return sr.evaluateJSONRPC(rpcReq), true
}

func (sr *StdioReplayer) evaluateJSONRPC(rpcReq JSONRPCRequest) JSONRPCResponse {
	var toolName string
	var toolArgs map[string]interface{}

	if rpcReq.Method == "tools/call" && len(rpcReq.Params) > 0 {
		if parsed, err := matcher.DecodeJSONUseNumber(rpcReq.Params); err == nil {
			if pMap, ok := parsed.(map[string]interface{}); ok {
				if n, ok := pMap["name"].(string); ok {
					toolName = n
				}
				if a, ok := pMap["arguments"].(map[string]interface{}); ok {
					toolArgs = a
				}
			}
		}
	}

	candidate := &cassette.Request{
		Method:    rpcReq.Method,
		ToolName:  toolName,
		ToolInput: toolArgs,
		RawBody:   string(rpcReq.Params),
	}
	redactedCandidate := candidate.Clone()
	cassette.RedactRequest(&redactedCandidate)
	if redactedCandidate.ToolName != "" {
		redactedCandidate.Matcher.StrictHash = matcher.ComputeToolHash(redactedCandidate.ToolName, redactedCandidate.ToolInput)
	} else {
		redactedCandidate.Matcher.StrictHash = matcher.ComputeRequestHash(&redactedCandidate)
	}

	inter := sr.findMatch(&redactedCandidate)
	if inter == nil {
		if candidate.ToolName != "" {
			candidate.Matcher.StrictHash = matcher.ComputeToolHash(candidate.ToolName, candidate.ToolInput)
		} else {
			candidate.Matcher.StrictHash = matcher.ComputeRequestHash(candidate)
		}
		if redactedCandidate.Matcher.StrictHash != candidate.Matcher.StrictHash {
			inter = sr.findMatch(candidate)
		}
	}
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      rpcReq.ID,
	}

	if inter != nil {
		if inter.Response.Error != "" && inter.Response.ToolResult == "" && inter.Response.Body == nil && inter.Response.RawBody == "" {
			resp.Error = map[string]interface{}{
				"code":    -32000,
				"message": inter.Response.Error,
			}
		} else if inter.Response.ToolResult != "" {
			resp.Result = map[string]interface{}{
				"content": []map[string]string{
					{
						"type": "text",
						"text": inter.Response.ToolResult,
					},
				},
			}
		} else if inter.Response.Body != nil {
			resp.Result = inter.Response.Body
		} else if inter.Response.RawBody != "" && json.Valid([]byte(inter.Response.RawBody)) {
			resp.Result = json.RawMessage(inter.Response.RawBody)
		} else {
			resp.Result = inter.Response.RawBody
		}
	} else if synth, ok := sr.synthesizeProtocolResponse(rpcReq.Method); ok {
		resp.Result = synth
	} else {
		label := toolName
		if label == "" {
			label = rpcReq.Method
		}
		resp.Error = map[string]interface{}{
			"code":    -32601,
			"message": fmt.Sprintf("AgentFixture: no match found for %s", label),
		}
	}

	return resp
}

func (sr *StdioReplayer) synthesizeProtocolResponse(method string) (interface{}, bool) {
	switch method {
	case "ping":
		return map[string]interface{}{}, true
	case "initialize":
		return map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]interface{}{
				"tools": map[string]interface{}{},
			},
			"serverInfo": map[string]interface{}{
				"name":    "AgentFixture-MCP-Replay",
				"version": cassette.GeneratorVersion,
			},
		}, true
	case "tools/list":
		sr.mu.Lock()
		defer sr.mu.Unlock()
		seen := make(map[string]bool)
		tools := make([]map[string]interface{}, 0)
		for _, inter := range sr.cas.Interactions {
			name := strings.TrimSpace(inter.Request.ToolName)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			tools = append(tools, map[string]interface{}{
				"name":        name,
				"description": "Recorded AgentFixture MCP tool",
				"inputSchema": map[string]interface{}{
					"type": "object",
				},
			})
		}
		return map[string]interface{}{"tools": tools}, true
	default:
		return nil, false
	}
}

func (sr *StdioReplayer) findMatch(candidate *cassette.Request) *cassette.Interaction {
	sr.mu.Lock()
	defer sr.mu.Unlock()

	var bestInter *cassette.Interaction
	var bestScore float64

	// Pass 1: Prefer unconsumed interactions first for stateful multi-turn tool calls.
	for i := range sr.cas.Interactions {
		inter := &sr.cas.Interactions[i]
		if sr.matchedIDs[inter.ID] {
			continue
		}
		res := sr.matcher.Match(&inter.Request, candidate)
		if res.Matched && res.Score > bestScore {
			bestScore = res.Score
			bestInter = inter
			if res.Score == 1.0 {
				break
			}
		}
	}

	// Pass 2: Fallback to already-consumed interactions for idempotent retries.
	if bestInter == nil {
		for i := range sr.cas.Interactions {
			inter := &sr.cas.Interactions[i]
			if !sr.matchedIDs[inter.ID] {
				continue
			}
			res := sr.matcher.Match(&inter.Request, candidate)
			if res.Matched && res.Score > bestScore {
				bestScore = res.Score
				bestInter = inter
				if res.Score == 1.0 {
					break
				}
			}
		}
	}

	if bestInter != nil {
		sr.matchedIDs[bestInter.ID] = true
		cloned := bestInter.Clone()
		return &cloned
	}
	return nil
}

// StdioRecorder taps bidirectional JSON-RPC / MCP stdio traffic between a host client and a child MCP server process.
type StdioRecorder struct {
	mu      sync.Mutex
	cas     *cassette.Cassette
	pending map[string]pendingRPC
}

type pendingRPC struct {
	req      JSONRPCRequest
	toolName string
	toolArgs map[string]interface{}
	sentAt   time.Time
}

// NewStdioRecorder creates an initialized StdioRecorder backed by cas.
func NewStdioRecorder(cas *cassette.Cassette) *StdioRecorder {
	if cas == nil {
		cas = cassette.NewCassette(fmt.Sprintf("cas_mcp_%d", time.Now().UnixNano()))
	}
	cas.Metadata.Source = "agentfixture_mcp_stdio_recorder"
	return &StdioRecorder{
		cas:     cas,
		pending: make(map[string]pendingRPC),
	}
}

// GetCassette returns a thread-safe clone of the recorded MCP cassette.
func (r *StdioRecorder) GetCassette() *cassette.Cassette {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cas.Clone()
}

// RecordCommand launches cmdArgs as an MCP stdio child process, bridges clientIn -> child.Stdin
// and child.Stdout -> clientOut while recording all JSON-RPC interactions into the cassette.
func (r *StdioRecorder) RecordCommand(ctx context.Context, cmdArgs []string, env []string, clientIn io.Reader, clientOut io.Writer) (*cassette.Cassette, error) {
	if len(cmdArgs) == 0 {
		return nil, fmt.Errorf("mcp command cannot be empty")
	}
	cmd := exec.CommandContext(ctx, cmdArgs[0], cmdArgs[1:]...)
	cmd.WaitDelay = 3 * time.Second
	if len(env) > 0 {
		cmd.Env = env
	}
	cmd.Stderr = os.Stderr

	childStdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to attach mcp stdin pipe: %w", err)
	}
	childStdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to attach mcp stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start mcp server %q: %w", cmdArgs[0], err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- r.pumpResponses(childStdout, clientOut)
	}()

	go func() {
		_ = r.pumpRequests(clientIn, childStdin)
		_ = childStdin.Close()
	}()

	respErr := <-errCh
	waitErr := cmd.Wait()
	if respErr != nil && respErr != io.EOF {
		return r.GetCassette(), respErr
	}
	if waitErr != nil && ctx.Err() == nil {
		return r.GetCassette(), waitErr
	}
	return r.GetCassette(), nil
}

// RecordStreams bridges two io.Reader/Writer pairs directly (useful for in-process testing without spawning an OS process).
func (r *StdioRecorder) RecordStreams(clientIn io.Reader, serverIn io.WriteCloser, serverOut io.Reader, clientOut io.Writer) (*cassette.Cassette, error) {
	var wg sync.WaitGroup
	var respErr error

	wg.Add(1)
	go func() {
		defer wg.Done()
		respErr = r.pumpResponses(serverOut, clientOut)
	}()

	_ = r.pumpRequests(clientIn, serverIn)
	_ = serverIn.Close()
	wg.Wait()

	if respErr != nil && respErr != io.EOF {
		return r.GetCassette(), respErr
	}
	return r.GetCassette(), nil
}

func (r *StdioRecorder) registerPendingRequestObject(raw []byte) {
	var rawObj map[string]json.RawMessage
	if json.Unmarshal(raw, &rawObj) != nil {
		return
	}
	rpcReq, decErr := decodeJSONRPCRequest(raw)
	if decErr != nil || rpcReq.Method == "" {
		return
	}
	idRaw, hasID := rawObj["id"]
	if !hasID || !isValidNonNullRPCID(idRaw, rpcReq.ID) {
		return
	}

	var toolName string
	var toolArgs map[string]interface{}
	if rpcReq.Method == "tools/call" && len(rpcReq.Params) > 0 {
		if parsed, pErr := matcher.DecodeJSONUseNumber(rpcReq.Params); pErr == nil {
			if pMap, ok := parsed.(map[string]interface{}); ok {
				toolName, _ = pMap["name"].(string)
				toolArgs, _ = pMap["arguments"].(map[string]interface{})
			}
		}
	}
	key := rpcIDKey(rpcReq.ID)
	r.mu.Lock()
	r.pending[key] = pendingRPC{
		req:      rpcReq,
		toolName: toolName,
		toolArgs: toolArgs,
		sentAt:   time.Now(),
	}
	r.mu.Unlock()
}

func (r *StdioRecorder) pumpRequests(src io.Reader, dst io.Writer) error {
	br := bufio.NewReaderSize(src, 64*1024)
	for {
		payload, framing, err := readStdioFrame(br)
		if err != nil {
			return err
		}
		trimmed := bytes.TrimSpace(payload)
		if len(trimmed) > 0 {
			switch trimmed[0] {
			case '{':
				r.registerPendingRequestObject(trimmed)
			case '[':
				var batch []json.RawMessage
				if json.Unmarshal(trimmed, &batch) == nil {
					for _, elem := range batch {
						elemTrimmed := bytes.TrimSpace(elem)
						if len(elemTrimmed) > 0 && elemTrimmed[0] == '{' {
							r.registerPendingRequestObject(elemTrimmed)
						}
					}
				}
			}
		}
		if err := writeStdioFrame(dst, payload, framing); err != nil {
			return err
		}
	}
}

func (r *StdioRecorder) processIncomingResponseObject(raw []byte) {
	rpcResp, decErr := decodeJSONRPCResponse(raw)
	if decErr != nil || rpcResp.ID == nil {
		return
	}
	key := rpcIDKey(rpcResp.ID)
	r.mu.Lock()
	pend, ok := r.pending[key]
	if ok {
		delete(r.pending, key)
	}
	r.mu.Unlock()

	if ok {
		r.recordPairedInteraction(pend, rpcResp)
	}
}

func (r *StdioRecorder) pumpResponses(src io.Reader, dst io.Writer) error {
	br := bufio.NewReaderSize(src, 64*1024)
	for {
		payload, framing, err := readStdioFrame(br)
		if err != nil {
			return err
		}
		trimmed := bytes.TrimSpace(payload)
		if len(trimmed) > 0 {
			switch trimmed[0] {
			case '{':
				r.processIncomingResponseObject(trimmed)
			case '[':
				var batch []json.RawMessage
				if json.Unmarshal(trimmed, &batch) == nil {
					for _, elem := range batch {
						elemTrimmed := bytes.TrimSpace(elem)
						if len(elemTrimmed) > 0 && elemTrimmed[0] == '{' {
							r.processIncomingResponseObject(elemTrimmed)
						}
					}
				}
			}
		}
		if err := writeStdioFrame(dst, payload, framing); err != nil {
			return err
		}
	}
}

func (r *StdioRecorder) recordPairedInteraction(pend pendingRPC, rpcResp JSONRPCResponse) {
	latency := time.Since(pend.sentAt).Milliseconds()
	interType := cassette.InteractionTypeStdio
	if pend.toolName != "" {
		interType = cassette.InteractionTypeTool
	}

	redArgs := cassette.RedactMap(pend.toolArgs)
	rawParams := cassette.RedactString(string(pend.req.Params))

	req := cassette.Request{
		Method:    pend.req.Method,
		ToolName:  pend.toolName,
		ToolInput: redArgs,
		RawBody:   rawParams,
	}
	req.Matcher.StrictHash = matcher.ComputeRequestHash(&req)
	if pend.toolName != "" {
		req.Matcher.FuzzySignature = cassette.FuzzySignature{
			NormalizedToolName: strings.ToLower(pend.toolName),
			NormalizedArgs:     redArgs,
		}
	}

	var resp cassette.Response
	resp.LatencyMs = latency

	if rpcResp.Error != nil {
		resp.StatusCode = 500
		if errMap, ok := rpcResp.Error.(map[string]interface{}); ok {
			if msg, ok := errMap["message"].(string); ok {
				resp.Error = cassette.RedactString(msg)
			}
		}
		if resp.Error == "" {
			b, _ := json.Marshal(rpcResp.Error)
			resp.Error = cassette.RedactString(string(b))
		}
	} else {
		resp.StatusCode = 0
		if textResult, ok := extractMCPTextContent(rpcResp.Result); ok && pend.toolName != "" {
			resp.ToolResult = cassette.RedactString(textResult)
		} else {
			resp.Body = cassette.RedactValue(rpcResp.Result)
		}
	}

	r.mu.Lock()
	id, seq := nextInteractionID(r.cas.Interactions, "mcp_")
	inter := cassette.Interaction{
		ID:       id,
		Sequence: seq,
		Type:     interType,
		Request:  req,
		Response: resp,
	}
	cassette.RedactInteraction(&inter)
	r.cas.Interactions = append(r.cas.Interactions, inter)
	r.mu.Unlock()
}

func extractMCPTextContent(result interface{}) (string, bool) {
	m, ok := result.(map[string]interface{})
	if !ok {
		return "", false
	}
	// Preserve full structured result in Response.Body whenever additional MCP fields
	// such as "isError": true, "structuredContent", or "_meta" are present.
	if len(m) != 1 {
		return "", false
	}
	contentSlice, ok := m["content"].([]interface{})
	if !ok || len(contentSlice) != 1 {
		return "", false
	}
	first, ok := contentSlice[0].(map[string]interface{})
	if !ok || len(first) != 2 {
		return "", false
	}
	if t, _ := first["type"].(string); t != "text" {
		return "", false
	}
	txt, ok := first["text"].(string)
	return txt, ok
}

func decodeJSONRPCRequest(data []byte) (JSONRPCRequest, error) {
	var req JSONRPCRequest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil {
		return req, err
	}
	return req, nil
}

func decodeJSONRPCResponse(data []byte) (JSONRPCResponse, error) {
	var resp JSONRPCResponse
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&resp); err != nil {
		return resp, err
	}
	resp.Result = normalizeStdioJSONNumbers(resp.Result)
	resp.Error = normalizeStdioJSONNumbers(resp.Error)
	return resp, nil
}

func normalizeStdioJSONNumbers(v interface{}) interface{} {
	switch val := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(val))
		for k, el := range val {
			out[k] = normalizeStdioJSONNumbers(el)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(val))
		for i, el := range val {
			out[i] = normalizeStdioJSONNumbers(el)
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
			return f
		}
		return s
	default:
		return val
	}
}

func rpcIDKey(id interface{}) string {
	switch v := id.(type) {
	case string:
		return "s:" + v
	case json.Number:
		s := v.String()
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return "n:" + strconv.FormatInt(i, 10)
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil && float64(int64(f)) == f {
			return "n:" + strconv.FormatInt(int64(f), 10)
		}
		return "n:" + s
	case float64:
		if float64(int64(v)) == v {
			return "n:" + strconv.FormatInt(int64(v), 10)
		}
		return fmt.Sprintf("n:%g", v)
	case int:
		return "n:" + strconv.Itoa(v)
	case int64:
		return "n:" + strconv.FormatInt(v, 10)
	default:
		b, _ := json.Marshal(id)
		return string(b)
	}
}

// isMIMEHeaderLine reports whether trimmed begins an HTTP/MIME-style stdio header block
// (e.g. `Content-Length: 120`, `Content-Type: application/vscode-jsonrpc`, `X-Request-Id: abc`).
func isMIMEHeaderLine(trimmed string) bool {
	colon := strings.IndexByte(trimmed, ':')
	if colon <= 0 {
		return false
	}
	// Exclude URL schemes such as http:// or https://
	if strings.HasPrefix(trimmed[colon:], "://") {
		return false
	}
	name := trimmed[:colon]
	first := name[0]
	if !((first >= 'A' && first <= 'Z') || (first >= 'a' && first <= 'z')) {
		return false
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func readStdioFrame(br *bufio.Reader) ([]byte, stdioFraming, error) {
	for {
		line, err := readBoundedLine(br, MaxStdioMessageBytes)
		if err != nil {
			if err == io.EOF && len(line) == 0 {
				return nil, framingNDJSON, io.EOF
			}
			if err != io.EOF {
				return nil, framingNDJSON, err
			}
		}

		trimmed := strings.TrimRight(string(line), "\r\n")
		if trimmed == "" {
			if err == io.EOF {
				return nil, framingNDJSON, io.EOF
			}
			continue
		}

		if isMIMEHeaderLine(trimmed) {
			if err == io.EOF {
				return nil, framingContentLength, io.ErrUnexpectedEOF
			}
			contentLen := -1
			if strings.HasPrefix(strings.ToLower(trimmed), "content-length:") {
				valStr := strings.TrimSpace(trimmed[len("content-length:"):])
				n, convErr := strconv.Atoi(valStr)
				if convErr != nil || n < 0 {
					return nil, framingContentLength, fmt.Errorf("invalid Content-Length header %q", trimmed)
				}
				contentLen = n
			}

			// Consume remaining MIME headers until blank line ("\r\n" or "\n")
			for {
				hdrLine, hdrErr := readBoundedLine(br, 64*1024)
				hdrTrimmed := strings.TrimRight(string(hdrLine), "\r\n")
				if hdrErr != nil {
					if hdrErr == io.EOF {
						return nil, framingContentLength, io.ErrUnexpectedEOF
					}
					return nil, framingContentLength, hdrErr
				}
				if hdrTrimmed == "" {
					break
				}
				if strings.HasPrefix(strings.ToLower(hdrTrimmed), "content-length:") {
					valStr := strings.TrimSpace(hdrTrimmed[len("content-length:"):])
					n, convErr := strconv.Atoi(valStr)
					if convErr != nil || n < 0 {
						return nil, framingContentLength, fmt.Errorf("invalid Content-Length header %q", hdrTrimmed)
					}
					contentLen = n
				}
			}

			if contentLen < 0 {
				return nil, framingContentLength, fmt.Errorf("missing Content-Length in MCP stdio header block")
			}
			if contentLen > MaxStdioMessageBytes {
				return nil, framingContentLength, fmt.Errorf("MCP Content-Length %d exceeds 16MB limit", contentLen)
			}

			payload := make([]byte, contentLen)
			if _, readErr := io.ReadFull(br, payload); readErr != nil {
				if readErr == io.EOF {
					return nil, framingContentLength, io.ErrUnexpectedEOF
				}
				return nil, framingContentLength, readErr
			}
			return payload, framingContentLength, nil
		}

		return []byte(trimmed), framingNDJSON, nil
	}
}

func readBoundedLine(br *bufio.Reader, maxBytes int) ([]byte, error) {
	var buf []byte
	for {
		frag, isPrefix, err := br.ReadLine()
		buf = append(buf, frag...)
		if len(buf) > maxBytes {
			return nil, fmt.Errorf("stdio message line exceeds maximum %d bytes", maxBytes)
		}
		if err != nil {
			return buf, err
		}
		if !isPrefix {
			return buf, nil
		}
	}
}

func writeStdioFrame(out io.Writer, payload []byte, framing stdioFraming) error {
	if framing == framingContentLength {
		header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(payload))
		if _, err := io.WriteString(out, header); err != nil {
			return err
		}
		_, err := out.Write(payload)
		return err
	}
	var buf bytes.Buffer
	buf.Grow(len(payload) + 1)
	buf.Write(payload)
	buf.WriteByte('\n')
	_, err := out.Write(buf.Bytes())
	return err
}
