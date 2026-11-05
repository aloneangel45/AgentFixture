package matcher

import (
	"strings"
	"testing"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
)

func TestCanonicalJSON_KeyOrderIndependence(t *testing.T) {
	m1 := map[string]interface{}{
		"b": 2,
		"a": 1,
		"nested": map[string]interface{}{
			"z": "end",
			"y": "start",
		},
	}
	m2 := map[string]interface{}{
		"nested": map[string]interface{}{
			"y": "start",
			"z": "end",
		},
		"a": 1,
		"b": 2,
	}

	b1, err := CanonicalJSON(m1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b2, err := CanonicalJSON(m2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if string(b1) != string(b2) {
		t.Errorf("expected canonical json to be identical, got %s vs %s", string(b1), string(b2))
	}

	hash1 := CanonicalHash(b1)
	hash2 := CanonicalHash(b2)
	if hash1 != hash2 {
		t.Errorf("expected hashes to match: %s vs %s", hash1, hash2)
	}
}

func TestStrictMatcher_Tools(t *testing.T) {
	strict := NewStrictMatcher()

	req1 := &cassette.Request{
		ToolName: "view_file",
		ToolInput: map[string]interface{}{
			"path":  "pkg/auth/auth.go",
			"lines": 100,
		},
	}

	req2 := &cassette.Request{
		ToolName: "view_file",
		ToolInput: map[string]interface{}{
			"lines": 100,
			"path":  "pkg/auth/auth.go",
		},
	}

	reqDifferentPath := &cassette.Request{
		ToolName: "view_file",
		ToolInput: map[string]interface{}{
			"path":  "pkg/auth/other.go",
			"lines": 100,
		},
	}

	reqDifferentTool := &cassette.Request{
		ToolName: "edit_file",
		ToolInput: map[string]interface{}{
			"path": "pkg/auth/auth.go",
		},
	}

	res := strict.Match(req1, req2)
	if !res.Matched || res.Score != 1.0 {
		t.Fatalf("expected strict match for reordered tool inputs, got %+v", res)
	}

	resDiff := strict.Match(req1, reqDifferentPath)
	if resDiff.Matched {
		t.Fatalf("expected mismatch for differing path, got %+v", resDiff)
	}
	if !strings.Contains(resDiff.Diff, "differ") {
		t.Errorf("expected diff explanation, got %q", resDiff.Diff)
	}

	resToolDiff := strict.Match(req1, reqDifferentTool)
	if resToolDiff.Matched {
		t.Fatalf("expected mismatch for differing tool name")
	}
	if !strings.Contains(resToolDiff.Diff, "tool_name mismatch") {
		t.Errorf("expected tool_name mismatch in diff, got %q", resToolDiff.Diff)
	}
}

func TestStrictMatcher_HTTP(t *testing.T) {
	strict := NewStrictMatcher()

	req1 := &cassette.Request{
		Method:   "POST",
		Endpoint: "https://api.anthropic.com/v1/messages",
		RawBody:  `{"model":"claude-3-7-sonnet","prompt":"hello"}`,
	}

	req2 := &cassette.Request{
		Method:   "POST",
		Endpoint: "https://api.anthropic.com/v1/messages",
		RawBody:  `{"model":"claude-3-7-sonnet","prompt":"hello"}`,
	}

	reqDrift := &cassette.Request{
		Method:   "POST",
		Endpoint: "https://api.anthropic.com/v1/messages",
		RawBody:  `{"model":"claude-3-7-sonnet","prompt":"goodbye"}`,
	}

	res := strict.Match(req1, req2)
	if !res.Matched {
		t.Fatalf("expected HTTP match, got %+v", res)
	}

	resDrift := strict.Match(req1, reqDrift)
	if resDrift.Matched {
		t.Fatalf("expected mismatch on modified prompt")
	}
}

func TestFuzzyMatcher_ToolDynamicKeysAndCase(t *testing.T) {
	fuzzy := NewFuzzyMatcher(0.80)

	expected := &cassette.Request{
		ToolName: "VIEW_FILE",
		ToolInput: map[string]interface{}{
			"path":      "pkg/auth/auth.go",
			"timestamp": "2026-11-05T20:00:00Z",
			"nonce":     12345,
		},
	}

	actual := &cassette.Request{
		ToolName: "view_file",
		ToolInput: map[string]interface{}{
			"path":      "pkg/auth/auth.go",
			"timestamp": "2026-11-06T12:00:00Z", // differing dynamic timestamp
			"nonce":     99999,                  // differing dynamic nonce
		},
	}

	res := fuzzy.Match(expected, actual)
	if !res.Matched || res.Score < 0.99 {
		t.Fatalf("expected fuzzy match despite dynamic keys and case differences, got %+v", res)
	}
}

func TestFuzzyMatcher_HTTPPromptNormalization(t *testing.T) {
	fuzzy := NewFuzzyMatcher(0.80)

	expected := &cassette.Request{
		Method:   "POST",
		Endpoint: "https://api.anthropic.com/v1/messages",
		RawBody:  "Current time is 2026-11-05T20:00:00Z.   Please inspect  session 12345678-1234-1234-1234-123456789abc.",
	}

	actual := &cassette.Request{
		Method:   "POST",
		Endpoint: "https://api.anthropic.com/v1/messages",
		RawBody:  "Current time is 2026-11-06T08:15:20Z. Please inspect session 98765432-4321-4321-4321-cba987654321.",
	}

	res := fuzzy.Match(expected, actual)
	if !res.Matched {
		t.Fatalf("expected fuzzy match after timestamp, uuid, and whitespace normalization, got %+v", res)
	}
}

func TestLevenshteinDistance(t *testing.T) {
	dist := LevenshteinDistance("kitten", "sitting")
	if dist != 3 {
		t.Errorf("expected distance 3 between kitten and sitting, got %d", dist)
	}

	sim := StringSimilarity("hello world", "hello world")
	if sim != 1.0 {
		t.Errorf("expected similarity 1.0, got %f", sim)
	}

	simEmpty := StringSimilarity("", "test")
	if simEmpty != 0.0 {
		t.Errorf("expected similarity 0.0, got %f", simEmpty)
	}
}
