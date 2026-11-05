package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
)

const sampleTracePortJSON = `{
  "id": "trace_17939077467428723",
  "command": "claude code \"Refactor the authentication middleware\"",
  "start_time": "2026-11-05T22:42:26.123456789Z",
  "end_time": "2026-11-05T22:42:35.987654321Z",
  "status": "completed",
  "frames": [
    {
      "index": 1,
      "timestamp": "2026-11-05T22:42:26.130000000Z",
      "type": "checkpoint",
      "content": "Initial flight snapshot",
      "git_snapshot": "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
    },
    {
      "index": 2,
      "timestamp": "2026-11-05T22:42:26.500000000Z",
      "type": "input",
      "content": "Fix the authentication nil pointer"
    },
    {
      "index": 3,
      "timestamp": "2026-11-05T22:42:27.100000000Z",
      "type": "output",
      "content": "I will inspect pkg/auth/auth.go to find the nil pointer."
    },
    {
      "index": 4,
      "timestamp": "2026-11-05T22:42:27.200000000Z",
      "type": "tool_call",
      "content": "Invoking view_file on auth.go",
      "tool": {
        "name": "view_file",
        "arguments": {
          "path": "pkg/auth/auth.go",
          "api_key": "sk-ant-sensitive1234567890abcdef"
        },
        "duration_ms": 42
      }
    },
    {
      "index": 5,
      "timestamp": "2026-11-05T22:42:27.242000000Z",
      "type": "tool_result",
      "content": "package auth\nfunc Authenticate() bool { return true }",
      "tool": {
        "name": "view_file",
        "arguments": {},
        "result": "package auth\nfunc Authenticate() bool { return true }",
        "duration_ms": 42
      }
    }
  ]
}`

func TestImportTracePortSession(t *testing.T) {
	cas, err := ImportTracePortSession([]byte(sampleTracePortJSON))
	if err != nil {
		t.Fatalf("unexpected error importing session: %v", err)
	}

	if cas.Version != cassette.CurrentVersion {
		t.Errorf("expected version %s, got %s", cassette.CurrentVersion, cas.Version)
	}
	if cas.Metadata.CassetteID != "trace_17939077467428723" {
		t.Errorf("expected cassette ID trace_17939077467428723, got %s", cas.Metadata.CassetteID)
	}
	if cas.Metadata.Tags["git_snapshot"] != "4b825dc642cb6eb9a060e54bf8d69288fbee4904" {
		t.Errorf("expected git_snapshot tag, got %s", cas.Metadata.Tags["git_snapshot"])
	}

	// Expect 2 interactions: 1 llm_chat turn (input+output) + 1 tool_execution
	if len(cas.Interactions) != 2 {
		t.Fatalf("expected 2 interactions, got %d", len(cas.Interactions))
	}

	chatTurn := cas.Interactions[0]
	if chatTurn.Type != cassette.InteractionTypeLLMChat {
		t.Errorf("expected first interaction to be llm_chat, got %s", chatTurn.Type)
	}
	if chatTurn.Request.RawBody != "Fix the authentication nil pointer" {
		t.Errorf("unexpected chat request body: %s", chatTurn.Request.RawBody)
	}

	toolTurn := cas.Interactions[1]
	if toolTurn.Type != cassette.InteractionTypeTool {
		t.Errorf("expected second interaction to be tool_execution, got %s", toolTurn.Type)
	}
	if toolTurn.Request.ToolName != "view_file" {
		t.Errorf("expected tool_name view_file, got %s", toolTurn.Request.ToolName)
	}
	if !strings.Contains(toolTurn.Response.ToolResult, "package auth") {
		t.Errorf("expected tool result to contain auth package, got %s", toolTurn.Response.ToolResult)
	}

	// Secret masking verification
	apiKeyValue := toolTurn.Request.ToolInput["api_key"]
	if apiKeyValue != "[REDACTED]" && apiKeyValue != "[REDACTED_ANTHROPIC_KEY]" {
		t.Errorf("expected api_key argument to be redacted, got %v", apiKeyValue)
	}

	// Metrics
	if cas.Metadata.Metrics.TotalInteractions != 2 {
		t.Errorf("expected 2 total interactions in metrics, got %d", cas.Metadata.Metrics.TotalInteractions)
	}
}

func TestImportTracePortFile(t *testing.T) {
	tmpDir := t.TempDir()
	sessionFile := filepath.Join(tmpDir, "session.json")
	outputFile := filepath.Join(tmpDir, "imported_cassette.yaml")

	if err := os.WriteFile(sessionFile, []byte(sampleTracePortJSON), 0644); err != nil {
		t.Fatalf("failed to write test session file: %v", err)
	}

	cas, err := ImportTracePortFile(sessionFile, outputFile)
	if err != nil {
		t.Fatalf("failed to import traceport file: %v", err)
	}

	if cas == nil {
		t.Fatalf("expected non-nil cassette")
	}

	// Verify file was written to disk and can be reloaded
	loaded, err := cassette.Load(outputFile)
	if err != nil {
		t.Fatalf("failed to reload imported YAML file: %v", err)
	}
	if len(loaded.Interactions) != 2 {
		t.Errorf("expected 2 interactions in reloaded cassette, got %d", len(loaded.Interactions))
	}
}
