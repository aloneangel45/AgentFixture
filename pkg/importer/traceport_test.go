package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
	"github.com/aloneangel45/AgentFixture/pkg/matcher"
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

	// Secret masking & zero secret hash oracle verification
	apiKeyValue := toolTurn.Request.ToolInput["api_key"]
	if apiKeyValue != "[REDACTED]" {
		t.Errorf("expected api_key argument to be [REDACTED], got %v", apiKeyValue)
	}

	// Replaying with a dummy CI key must match StrictHash 100%!
	strict := matcher.NewStrictMatcher()
	replayCandidate := &cassette.Request{
		ToolName: "view_file",
		ToolInput: map[string]interface{}{
			"path":    "pkg/auth/auth.go",
			"api_key": "ci-dummy-placeholder-key",
		},
	}
	if res := strict.Match(&toolTurn.Request, replayCandidate); !res.Matched {
		t.Fatalf("StrictHash oracle leak / mismatch when replaying with dummy CI key: %s", res.Diff)
	}

	// Metrics
	if cas.Metadata.Metrics.TotalInteractions != 2 {
		t.Errorf("expected 2 total interactions in metrics, got %d", cas.Metadata.Metrics.TotalInteractions)
	}
}

func TestImportTracePortSession_EventErrorFrames(t *testing.T) {
	errSession := `{
	  "id": "trace_errors_01",
	  "command": "agent --api-key=sk-ant-api03-1234567890abcdef1234567890",
	  "start_time": "2026-11-10T10:00:00Z",
	  "status": "failed",
	  "frames": [
	    {
	      "index": 1,
	      "timestamp": "2026-11-10T10:00:01Z",
	      "type": "tool_call",
	      "tool": {"name": "exec_cmd", "arguments": {"cmd": "false"}}
	    },
	    {
	      "index": 2,
	      "timestamp": "2026-11-10T10:00:02Z",
	      "type": "error",
	      "content": "exit status 1: permission denied"
	    },
	    {
	      "index": 3,
	      "timestamp": "2026-11-10T10:00:03Z",
	      "type": "error",
	      "content": "upstream rate limit exceeded (token sk-ant-api03-1234567890abcdef1234567890)"
	    }
	  ]
	}`

	cas, err := ImportTracePortSession([]byte(errSession))
	if err != nil {
		t.Fatalf("ImportTracePortSession failed: %v", err)
	}
	if len(cas.Interactions) != 2 {
		t.Fatalf("expected 2 interactions (paired tool error + standalone error), got %d", len(cas.Interactions))
	}
	if cas.Interactions[0].Response.StatusCode != 500 || !strings.Contains(cas.Interactions[0].Response.Error, "permission denied") {
		t.Errorf("unexpected paired tool error interaction: %+v", cas.Interactions[0].Response)
	}
	if cas.Interactions[1].ID != "tp_err_3" || cas.Interactions[1].Response.StatusCode != 500 {
		t.Errorf("unexpected standalone error interaction: %+v", cas.Interactions[1])
	}
	if hasSec, reason := cassette.CheckForUnredactedSecrets(cas); hasSec {
		t.Fatalf("unredacted secret found in error session import: %s", reason)
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

func TestImportTracePortFile_SplitV11MetaAndJSONL(t *testing.T) {
	tmpDir := t.TempDir()
	metaPath := filepath.Join(tmpDir, "trace_v11_01.meta.json")
	framesPath := filepath.Join(tmpDir, "trace_v11_01.frames.jsonl")
	outPath := filepath.Join(tmpDir, "v11.yaml")

	metaJSON := `{"id":"trace_v11_01","command":"claude --api-key=sk-ant-api03-secret999999999999","start_time":"2026-11-10T12:00:00Z","status":"completed","frame_count":3}`
	framesJSONL := strings.Join([]string{
		`{"index":1,"timestamp":"2026-11-10T12:00:01Z","type":"checkpoint","content":"Initial flight snapshot","git_snapshot":"zstd:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}`,
		`{"index":2,"timestamp":"2026-11-10T12:00:02Z","type":"tool_call","tool":{"name":"read_file","arguments":{"path":"main.go"}}}`,
		`{"index":3,"timestamp":"2026-11-10T12:00:03Z","type":"tool_result","content":"package main"}`,
	}, "\n") + "\n"

	if err := os.WriteFile(metaPath, []byte(metaJSON), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(framesPath, []byte(framesJSONL), 0600); err != nil {
		t.Fatal(err)
	}

	// Test importing via .meta.json, .frames.jsonl, and bare session prefix
	for _, inputPath := range []string{metaPath, framesPath, filepath.Join(tmpDir, "trace_v11_01")} {
		cas, err := ImportTracePortFile(inputPath, outPath)
		if err != nil {
			t.Fatalf("ImportTracePortFile(%s) failed: %v", inputPath, err)
		}
		if len(cas.Interactions) != 1 || cas.Interactions[0].Request.ToolName != "read_file" {
			t.Fatalf("unexpected imported interactions from %s: %+v", inputPath, cas.Interactions)
		}
		if !strings.HasPrefix(cas.Metadata.Tags["git_snapshot"], "zstd:") {
			t.Errorf("expected zstd checkpoint tag preserved, got %q", cas.Metadata.Tags["git_snapshot"])
		}
		if hasSec, reason := cassette.CheckForUnredactedSecrets(cas); hasSec {
			t.Fatalf("unredacted secret in split v1.1 import: %s", reason)
		}
	}
}

func TestImportTracePortSession_V12TagsDuplicateFrameIndexAndClockSkew(t *testing.T) {
	// Simulates TracePort v1.2 session with top-level tags (including a sensitive tag),
	// frames missing/duplicating `index` (both 0), and slight negative NTP clock skew on output frame.
	sessionJSON := `{
		"id": "tp_v12_tags_dup_idx",
		"command": "agent run",
		"start_time": "2026-11-14T10:00:00Z",
		"status": "completed",
		"tags": {
			"env": "staging",
			"branch": "feat/gemini",
			"api_key": "raw-tag-secret-must-be-scrubbed"
		},
		"frames": [
			{"index": 0, "timestamp": "2026-11-14T10:00:05Z", "type": "input", "content": "Hello", "git_snapshot": "zstd:snap_turn_1"},
			{"index": 0, "timestamp": "2026-11-14T10:00:04Z", "type": "output", "content": "Hi there"},
			{"index": 0, "timestamp": "2026-11-14T10:00:06Z", "type": "input", "content": "Second turn"},
			{"index": 0, "timestamp": "2026-11-14T10:00:07Z", "type": "output", "content": "Second reply"}
		]
	}`

	cas, err := ImportTracePortSession([]byte(sessionJSON))
	if err != nil {
		t.Fatalf("ImportTracePortSession failed: %v", err)
	}
	if err := cas.Validate(); err != nil {
		t.Fatalf("expected imported cassette with duplicate frame.Index=0 to pass Validate(), got: %v", err)
	}
	if cas.Metadata.Tags["env"] != "staging" || cas.Metadata.Tags["branch"] != "feat/gemini" {
		t.Errorf("expected TracePort v1.2 session tags preserved, got: %+v", cas.Metadata.Tags)
	}
	if cas.Metadata.Tags["api_key"] != "[REDACTED]" {
		t.Errorf("expected sensitive session tag 'api_key' scrubbed to [REDACTED], got %q", cas.Metadata.Tags["api_key"])
	}
	if hasSec, reason := cassette.CheckForUnredactedSecrets(cas); hasSec {
		t.Fatalf("unexpected secret leak in imported v1.2 session: %s", reason)
	}
	if cas.Interactions[0].Response.LatencyMs < 0 {
		t.Errorf("expected negative clock-skew latency clamped to >= 0, got %d", cas.Interactions[0].Response.LatencyMs)
	}
	if cas.Interactions[0].Metadata["git_snapshot"] != "zstd:snap_turn_1" {
		t.Errorf("expected turn git_snapshot preserved, got %q", cas.Interactions[0].Metadata["git_snapshot"])
	}
}
