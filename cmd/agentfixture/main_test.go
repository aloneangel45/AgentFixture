package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
)

func TestCLI_VerifyCommand(t *testing.T) {
	tmpDir := t.TempDir()
	validCasPath := filepath.Join(tmpDir, "valid.yaml")

	cas := cassette.NewCassette("cas_verify_test")
	cas.Interactions = append(cas.Interactions, cassette.Interaction{
		ID:       "int_001",
		Sequence: 1,
		Type:     cassette.InteractionTypeTool,
		Request: cassette.Request{
			ToolName: "view_file",
		},
		Response: cassette.Response{
			StatusCode: 200,
			ToolResult: "file content",
		},
	})

	if err := cassette.Save(validCasPath, cas); err != nil {
		t.Fatalf("failed to save test cassette: %v", err)
	}

	// Verify command should succeed
	if err := handleVerify([]string{validCasPath}); err != nil {
		t.Fatalf("expected handleVerify to succeed, got: %v", err)
	}

	// Corrupt cassette without interaction ID
	invalidCasPath := filepath.Join(tmpDir, "invalid.yaml")
	casBad := cassette.NewCassette("bad_cas")
	casBad.Interactions = append(casBad.Interactions, cassette.Interaction{
		Sequence: 1,
		Type:     cassette.InteractionTypeTool,
	})
	_ = cassette.Save(invalidCasPath, casBad)

	if err := handleVerify([]string{invalidCasPath}); err == nil {
		t.Fatalf("expected handleVerify to fail on invalid cassette")
	}
}

func TestCLI_DiffCommand(t *testing.T) {
	tmpDir := t.TempDir()
	casPath1 := filepath.Join(tmpDir, "cas1.yaml")
	casPath2 := filepath.Join(tmpDir, "cas2.yaml")

	c1 := cassette.NewCassette("c1")
	c1.Interactions = append(c1.Interactions, cassette.Interaction{
		ID:       "int_01",
		Sequence: 1,
		Type:     cassette.InteractionTypeTool,
		Request: cassette.Request{
			ToolName: "view_file",
			ToolInput: map[string]interface{}{
				"path": "a.txt",
			},
		},
	})
	_ = cassette.Save(casPath1, c1)

	c2 := cassette.NewCassette("c2")
	c2.Interactions = append(c2.Interactions, cassette.Interaction{
		ID:       "int_01",
		Sequence: 1,
		Type:     cassette.InteractionTypeTool,
		Request: cassette.Request{
			ToolName: "view_file",
			ToolInput: map[string]interface{}{
				"path": "b.txt", // drifted argument
			},
		},
	})
	_ = cassette.Save(casPath2, c2)

	if err := handleDiff([]string{casPath1, casPath2}); err != nil {
		t.Fatalf("handleDiff returned unexpected error: %v", err)
	}
}

func TestCLI_ImportCommand(t *testing.T) {
	tmpDir := t.TempDir()
	sampleJSON := `{
		"id": "trace_test_import",
		"command": "test cmd",
		"start_time": "2026-11-05T20:00:00Z",
		"status": "completed",
		"frames": [
			{
				"index": 1,
				"timestamp": "2026-11-05T20:00:01Z",
				"type": "tool_call",
				"tool": {
					"name": "calc",
					"arguments": {"a": 1, "b": 2},
					"result": "3",
					"duration_ms": 10
				}
			}
		]
	}`

	jsonPath := filepath.Join(tmpDir, "flight.json")
	outPath := filepath.Join(tmpDir, "out.yaml")
	if err := os.WriteFile(jsonPath, []byte(sampleJSON), 0644); err != nil {
		t.Fatalf("failed to write flight json: %v", err)
	}

	if err := handleImport([]string{"-traceport", jsonPath, "-out", outPath}); err != nil {
		t.Fatalf("handleImport failed: %v", err)
	}

	imported, err := cassette.Load(outPath)
	if err != nil {
		t.Fatalf("failed to reload imported cassette: %v", err)
	}
	if len(imported.Interactions) != 1 {
		t.Errorf("expected 1 imported interaction, got %d", len(imported.Interactions))
	}
	if !strings.Contains(imported.Interactions[0].Response.ToolResult, "3") {
		t.Errorf("expected result '3', got %s", imported.Interactions[0].Response.ToolResult)
	}
}
