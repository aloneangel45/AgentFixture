package cassette

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCassette_Validation(t *testing.T) {
	c := NewCassette("cas_valid_01")
	c.Interactions = append(c.Interactions, Interaction{
		ID:       "int_1",
		Sequence: 1,
		Type:     InteractionTypeTool,
		Request: Request{
			ToolName: "view_file",
		},
		Response: Response{
			StatusCode: 200,
			ToolResult: "file content",
		},
	})

	if err := c.Validate(); err != nil {
		t.Fatalf("expected valid cassette, got: %v", err)
	}

	// Missing ID in interaction
	c.Interactions = append(c.Interactions, Interaction{
		Sequence: 2,
		Type:     InteractionTypeLLMChat,
	})
	if err := c.Validate(); err == nil {
		t.Fatalf("expected error on missing interaction ID")
	}

	// Duplicate ID
	c.Interactions[1].ID = "int_1"
	if err := c.Validate(); err == nil {
		t.Fatalf("expected error on duplicate interaction ID")
	}
}

func TestCassette_SerializationYAMLAndJSON(t *testing.T) {
	tmpDir := t.TempDir()

	c := NewCassette("cas_test_serialization")
	c.Metadata.AgentName = "claude-code"
	c.Metadata.Model = "claude-3-7-sonnet"
	c.Interactions = append(c.Interactions, Interaction{
		ID:       "int_100",
		Sequence: 1,
		Type:     InteractionTypeTool,
		Request: Request{
			ToolName: "execute_cmd",
			ToolInput: map[string]interface{}{
				"command": "go test ./...",
			},
		},
		Response: Response{
			StatusCode: 0,
			LatencyMs:  150,
			ToolResult: "PASS\nok",
			Usage: Usage{
				InputTokens:  120,
				OutputTokens: 30,
				TotalTokens:  150,
			},
		},
	})

	// Test YAML save and load
	yamlPath := filepath.Join(tmpDir, "cassette.yaml")
	if err := Save(yamlPath, c); err != nil {
		t.Fatalf("failed to save YAML cassette: %v", err)
	}

	loadedYAML, err := Load(yamlPath)
	if err != nil {
		t.Fatalf("failed to load YAML cassette: %v", err)
	}
	if loadedYAML.Metadata.CassetteID != c.Metadata.CassetteID {
		t.Errorf("expected cassette ID %s, got %s", c.Metadata.CassetteID, loadedYAML.Metadata.CassetteID)
	}
	if len(loadedYAML.Interactions) != 1 {
		t.Fatalf("expected 1 interaction, got %d", len(loadedYAML.Interactions))
	}
	if loadedYAML.Metadata.Metrics.TotalInteractions != 1 {
		t.Errorf("expected 1 total interaction metric, got %d", loadedYAML.Metadata.Metrics.TotalInteractions)
	}

	// Test JSON save and load
	jsonPath := filepath.Join(tmpDir, "cassette.json")
	if err := Save(jsonPath, c); err != nil {
		t.Fatalf("failed to save JSON cassette: %v", err)
	}

	loadedJSON, err := Load(jsonPath)
	if err != nil {
		t.Fatalf("failed to load JSON cassette: %v", err)
	}
	if loadedJSON.Metadata.AgentName != "claude-code" {
		t.Errorf("expected agent name 'claude-code', got %s", loadedJSON.Metadata.AgentName)
	}

	// Verify file content actually exists on disk
	info, err := os.Stat(yamlPath)
	if err != nil || info.Size() == 0 {
		t.Fatalf("expected non-empty YAML file, err: %v", err)
	}
}

func TestSave_AtomicOverwriteAndNoTempLeak(t *testing.T) {
	tmpDir := t.TempDir()
	target := filepath.Join(tmpDir, "sub", "atomic.yaml")

	c1 := NewCassette("atomic-v1")
	c1.Interactions = []Interaction{{ID: "int_1", Sequence: 1, Type: InteractionTypeHTTP}}
	if err := Save(target, c1); err != nil {
		t.Fatalf("first Save failed: %v", err)
	}

	c2 := NewCassette("atomic-v2")
	c2.Interactions = []Interaction{
		{ID: "int_1", Sequence: 1, Type: InteractionTypeHTTP},
		{ID: "int_2", Sequence: 2, Type: InteractionTypeHTTP},
	}
	if err := Save(target, c2); err != nil {
		t.Fatalf("overwrite Save failed: %v", err)
	}

	loaded, err := Load(target)
	if err != nil {
		t.Fatalf("Load after atomic overwrite failed: %v", err)
	}
	if loaded.Metadata.CassetteID != "atomic-v2" || len(loaded.Interactions) != 2 {
		t.Fatalf("unexpected cassette content after overwrite: %+v", loaded.Metadata)
	}

	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "atomic.yaml" {
		t.Fatalf("expected no leftover temporary files in %s, found %d entries", filepath.Dir(target), len(entries))
	}
}
