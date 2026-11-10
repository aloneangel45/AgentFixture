package matcher

import (
	"fmt"
	"strings"
	"testing"
	"time"

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

func TestStrictMatcher_64BitIntegerPrecisionAndRawBodyCanonicalization(t *testing.T) {
	strict := NewStrictMatcher()

	// 9007199254740993 and 9007199254740992 collide if decoded as float64!
	req1 := &cassette.Request{
		Method:   "POST",
		Endpoint: "/v1/messages",
		RawBody:  `{"seed": 9007199254740993, "model": "claude-sonnet-4"}`,
	}
	req2Collision := &cassette.Request{
		Method:   "POST",
		Endpoint: "/v1/messages",
		RawBody:  `{"model": "claude-sonnet-4", "seed": 9007199254740992}`,
	}
	req3Reordered := &cassette.Request{
		Method:   "POST",
		Endpoint: "https://api.anthropic.com/v1/messages",
		RawBody:  `{"model":"claude-sonnet-4","seed":9007199254740993}`,
	}

	if strict.Match(req1, req2Collision).Matched {
		t.Fatalf("PRECISION BUG: 9007199254740993 falsely matched 9007199254740992 due to float64 truncation")
	}
	if !strict.Match(req1, req3Reordered).Matched {
		t.Fatalf("expected reordered JSON RawBody + full URL vs path to match strictly")
	}
}

func TestStrictMatcher_QueryCanonicalizationAndSecretSafeHash(t *testing.T) {
	strict := NewStrictMatcher()

	recorded := &cassette.Request{
		Method:   "POST",
		Endpoint: "https://api.anthropic.com/v1/messages?beta=true&version=2",
		RawBody:  `{"prompt":"hello","auth":"Bearer sk-ant-api03-realSecretKey1234567890abcdef"}`,
	}
	recordedHash := ComputeRequestHash(recorded)
	recorded.Matcher.StrictHash = recordedHash

	// Replay with reordered query params, relative path, and dummy Bearer token
	replayed := &cassette.Request{
		Method:   "POST",
		Endpoint: "/v1/messages?version=2&beta=true",
		RawBody:  `{"auth":"Bearer af-replay-dummy-key","prompt":"hello"}`,
	}

	res := strict.Match(recorded, replayed)
	if !res.Matched {
		t.Fatalf("expected secret-redacted hash + canonical query to match, got diff: %s", res.Diff)
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

func TestFuzzyMatcher_ToolDynamicKeysSliceRecursionAndBareIDPreserved(t *testing.T) {
	fuzzy := NewFuzzyMatcher(0.80)

	expected := &cassette.Request{
		ToolName: "VIEW_FILE",
		ToolInput: map[string]interface{}{
			"path":       "pkg/auth/auth.go",
			"timestamp":  "2026-11-05T20:00:00Z",
			"request_id": "req_111",
			"nonce":      12345,
			"items": []interface{}{
				map[string]interface{}{
					"name":       "item_a",
					"trace_id":   "tr_aaa",
					"created_at": "2026-01-01T00:00:00Z",
				},
			},
		},
	}

	actual := &cassette.Request{
		ToolName: "view_file",
		ToolInput: map[string]interface{}{
			"path":       "pkg/auth/auth.go",
			"timestamp":  "2026-11-06T12:00:00Z",
			"request_id": "req_999",
			"nonce":      99999,
			"items": []interface{}{
				map[string]interface{}{
					"name":       "item_a",
					"trace_id":   "tr_bbb",
					"created_at": "2026-02-02T00:00:00Z",
				},
			},
		},
	}

	res := fuzzy.Match(expected, actual)
	if !res.Matched || res.Score < 0.99 {
		t.Fatalf("expected fuzzy match despite nested slice dynamic keys, got %+v", res)
	}

	// Bare "id" MUST NOT be ignored! get_user(id=1) != get_user(id=2)
	user1 := &cassette.Request{
		ToolName:  "get_user",
		ToolInput: map[string]interface{}{"id": 1},
	}
	user2 := &cassette.Request{
		ToolName:  "get_user",
		ToolInput: map[string]interface{}{"id": 99999},
	}
	if fuzzy.Match(user1, user2).Matched {
		t.Fatalf("expected bare 'id' key difference to NOT be stripped as a dynamic key")
	}
}

func TestFuzzyMatcher_HTTPPromptNormalizationAndUTF8Runes(t *testing.T) {
	fuzzy := NewFuzzyMatcher(0.80)

	expected := &cassette.Request{
		Method:   "POST",
		Endpoint: "https://api.anthropic.com/v1/messages?beta=true&timestamp=111",
		RawBody:  "Current time is 2026-11-05T20:00:00Z.   Please inspect  session 12345678-1234-1234-1234-123456789abc.",
	}

	actual := &cassette.Request{
		Method:   "POST",
		Endpoint: "/v1/messages?beta=true&timestamp=999",
		RawBody:  "Current time is 2026-11-06T08:15:20Z. Please inspect session 98765432-4321-4321-4321-cba987654321.",
	}

	res := fuzzy.Match(expected, actual)
	if !res.Matched {
		t.Fatalf("expected fuzzy match after URL, timestamp, uuid, and whitespace normalization, got %+v", res)
	}

	// Verify UTF-8 multi-byte rune similarity (4 runes differing in 1 rune -> 0.75, NOT 1 - 1/12 = 0.916!)
	simUTF8 := StringSimilarity("日本語テスト", "日本語テスX") // 6 runes, 1 rune diff -> 5/6 = 0.8333
	if simUTF8 < 0.82 || simUTF8 > 0.85 {
		t.Errorf("expected UTF-8 rune similarity ~0.8333 (5/6), got %f", simUTF8)
	}
}

func TestFuzzyMatcher_100KBPromptPerformanceGuard(t *testing.T) {
	fuzzy := NewFuzzyMatcher(0.80)
	var sb1, sb2 strings.Builder
	for i := 0; i < 1500; i++ {
		sb1.WriteString(fmt.Sprintf("chunk_%04d: deterministic agent regression context payload line\n", i))
		if i%40 == 0 {
			sb2.WriteString(fmt.Sprintf("chunk_%04d: deterministic agent MODIFIED context payload line\n", i))
		} else {
			sb2.WriteString(fmt.Sprintf("chunk_%04d: deterministic agent regression context payload line\n", i))
		}
	}
	req1 := &cassette.Request{Method: "POST", Endpoint: "/v1/messages", RawBody: sb1.String()}
	req2 := &cassette.Request{Method: "POST", Endpoint: "/v1/messages", RawBody: sb2.String()}

	// Warmup + measure
	_ = fuzzy.Match(req1, req2)
	start := time.Now()
	const runs = 10
	for i := 0; i < runs; i++ {
		res := fuzzy.Match(req1, req2)
		if !res.Matched {
			t.Fatalf("expected match on 100KB prompt, got %+v", res)
		}
	}
	avg := time.Since(start) / runs
	if avg > 5*time.Millisecond {
		t.Errorf("100KB prompt FuzzyMatcher too slow: avg %v per match", avg)
	}
	t.Logf("100KB prompt FuzzyMatcher average latency: %v", avg)
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
