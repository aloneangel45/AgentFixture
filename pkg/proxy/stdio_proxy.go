package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
	"github.com/aloneangel45/AgentFixture/pkg/matcher"
)

// JSONRPCRequest models a standard JSON-RPC 2.0 / MCP tool invocation message.
type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
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

// StdioReplayer evaluates line-delimited JSON-RPC or tool invocations from an input reader
// and streams deterministic cassette responses to an output writer.
type StdioReplayer struct {
	cas     *cassette.Cassette
	matcher matcher.Matcher
}

// NewStdioReplayer creates an initialized StdioReplayer.
func NewStdioReplayer(cas *cassette.Cassette, m matcher.Matcher) *StdioReplayer {
	if m == nil {
		m = matcher.NewStrictMatcher()
	}
	return &StdioReplayer{
		cas:     cas,
		matcher: m,
	}
}

// ProcessStream reads line-by-line tool or JSON-RPC messages and writes out cassette answers.
func (sr *StdioReplayer) ProcessStream(in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		var rpcReq JSONRPCRequest
		if err := json.Unmarshal([]byte(trimmed), &rpcReq); err == nil && rpcReq.Method != "" {
			// Structured JSON-RPC / MCP call
			if err := sr.handleJSONRPC(rpcReq, out); err != nil {
				return err
			}
			continue
		}

		// Fallback: raw line or tool invocation
		req := &cassette.Request{
			RawBody: trimmed,
		}
		inter := sr.findMatch(req)
		if inter != nil {
			result := inter.Response.ToolResult
			if result == "" {
				result = inter.Response.RawBody
			}
			_, _ = fmt.Fprintln(out, result)
		} else {
			_, _ = fmt.Fprintf(out, `{"error":"No cassette interaction matched input: %s"}`+"\n", trimmed)
		}
	}

	return scanner.Err()
}

func (sr *StdioReplayer) handleJSONRPC(rpcReq JSONRPCRequest, out io.Writer) error {
	var toolName string
	var toolArgs map[string]interface{}

	if strings.Contains(rpcReq.Method, "tool") && len(rpcReq.Params) > 0 {
		var p MCPToolParams
		if err := json.Unmarshal(rpcReq.Params, &p); err == nil {
			toolName = p.Name
			toolArgs = p.Arguments
		}
	}

	candidate := &cassette.Request{
		ToolName:  toolName,
		ToolInput: toolArgs,
		RawBody:   string(rpcReq.Params),
	}

	inter := sr.findMatch(candidate)
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      rpcReq.ID,
	}

	if inter != nil {
		if inter.Response.ToolResult != "" {
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
		} else {
			resp.Result = inter.Response.RawBody
		}
	} else {
		resp.Error = map[string]interface{}{
			"code":    -32601,
			"message": fmt.Sprintf("AgentFixture: no match found for tool %s", toolName),
		}
	}

	b, _ := json.Marshal(resp)
	_, err := fmt.Fprintln(out, string(b))
	return err
}

func (sr *StdioReplayer) findMatch(candidate *cassette.Request) *cassette.Interaction {
	for i := range sr.cas.Interactions {
		inter := &sr.cas.Interactions[i]
		res := sr.matcher.Match(&inter.Request, candidate)
		if res.Matched {
			return inter
		}
	}
	return nil
}
