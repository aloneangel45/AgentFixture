package matcher_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
	"github.com/aloneangel45/AgentFixture/pkg/matcher"
)

func TestAdversarial_MatcherEdgeCases(t *testing.T) {
	strict := matcher.NewStrictMatcher()
	fuzzy := matcher.NewFuzzyMatcher(0.80)

	// 1. Large JSON Payload (thousands of keys & deep hierarchy)
	t.Run("Large JSON Payload with Out-of-Order Keys", func(t *testing.T) {
		largeMap1 := make(map[string]interface{})
		largeMap2 := make(map[string]interface{})

		// Populate 2,000 keys in different insertion orders
		for i := 0; i < 2000; i++ {
			k1 := fmt.Sprintf("key_%04d", i)
			k2 := fmt.Sprintf("key_%04d", 1999-i)
			val := fmt.Sprintf("value_payload_%d", i)

			largeMap1[k1] = val
			largeMap2[k2] = fmt.Sprintf("value_payload_%d", 1999-i)
		}

		b1, err := matcher.CanonicalJSON(largeMap1)
		if err != nil {
			t.Fatalf("CanonicalJSON failed on largeMap1: %v", err)
		}
		b2, err := matcher.CanonicalJSON(largeMap2)
		if err != nil {
			t.Fatalf("CanonicalJSON failed on largeMap2: %v", err)
		}

		if string(b1) != string(b2) {
			t.Fatalf("CanonicalJSON diverged on 2,000 reversed keys!")
		}

		req1 := &cassette.Request{
			ToolName:  "process_batch",
			ToolInput: largeMap1,
		}
		req2 := &cassette.Request{
			ToolName:  "process_batch",
			ToolInput: largeMap2,
		}

		resStrict := strict.Match(req1, req2)
		if !resStrict.Matched {
			t.Fatalf("StrictMatcher failed on large payload out-of-order keys: %s", resStrict.Diff)
		}

		resFuzzy := fuzzy.Match(req1, req2)
		if !resFuzzy.Matched {
			t.Fatalf("FuzzyMatcher failed on large payload: %s", resFuzzy.Diff)
		}
	})

	// 2. Floats vs Ints
	t.Run("Floats vs Ints", func(t *testing.T) {
		// In JSON unmarshaling, 42 and 42.0 are identical in JSON text
		rawJSONInt := `{"count":42,"rate":1.0}`
		rawJSONFloat := `{"rate":1,"count":42.0}`

		var mapInt, mapFloat map[string]interface{}
		_ = json.Unmarshal([]byte(rawJSONInt), &mapInt)
		_ = json.Unmarshal([]byte(rawJSONFloat), &mapFloat)

		// After unmarshaling via standard Go JSON, both numbers become float64
		cb1, err1 := matcher.CanonicalJSON(mapInt)
		cb2, err2 := matcher.CanonicalJSON(mapFloat)
		if err1 != nil || err2 != nil {
			t.Fatalf("CanonicalJSON error: %v, %v", err1, err2)
		}

		if string(cb1) != string(cb2) {
			t.Errorf("expected canonical JSON to match for float vs int JSON equivalence, got %s vs %s", string(cb1), string(cb2))
		}

		// Also test in Go native types: int(42) vs float64(42.0)
		nativeIntMap := map[string]interface{}{"value": int(42)}
		nativeFloatMap := map[string]interface{}{"value": float64(42)}
		bInt, _ := matcher.CanonicalJSON(nativeIntMap)
		bFloat, _ := matcher.CanonicalJSON(nativeFloatMap)
		if string(bInt) != string(bFloat) {
			t.Errorf("CanonicalJSON differed for native int vs float64: %s vs %s", string(bInt), string(bFloat))
		}
	})

	// 3. Nulls and Missing/Empty Values
	t.Run("Null and Empty Values", func(t *testing.T) {
		nullMap1 := map[string]interface{}{
			"nullable_field": nil,
			"valid_field":    "ok",
		}
		nullMap2 := map[string]interface{}{
			"valid_field":    "ok",
			"nullable_field": nil,
		}

		b1, _ := matcher.CanonicalJSON(nullMap1)
		b2, _ := matcher.CanonicalJSON(nullMap2)
		if string(b1) != string(b2) {
			t.Errorf("CanonicalJSON failed on null fields: %s vs %s", string(b1), string(b2))
		}

		req1 := &cassette.Request{
			ToolName:  "test_null",
			ToolInput: nullMap1,
		}
		req2 := &cassette.Request{
			ToolName:  "test_null",
			ToolInput: nullMap2,
		}
		res := strict.Match(req1, req2)
		if !res.Matched {
			t.Errorf("StrictMatcher failed on null values: %s", res.Diff)
		}
	})

	// 4. Whitespace Variations in HTTP Bodies
	t.Run("Whitespace Variations", func(t *testing.T) {
		bodyDense := `{"prompt":"Hello world","tags":["a","b"]}`
		bodySpaced := `{
			"prompt":   "Hello   world",
			"tags": [
				"a",
				"b"
			]
		}`

		// FuzzyMatcher should normalize multi-spaces and handle formatting differences
		reqDense := &cassette.Request{
			Method:   "POST",
			Endpoint: "https://api.anthropic.com/v1/messages",
			RawBody:  bodyDense,
		}
		reqSpaced := &cassette.Request{
			Method:   "POST",
			Endpoint: "https://api.anthropic.com/v1/messages",
			RawBody:  bodySpaced,
		}

		res := fuzzy.Match(reqDense, reqSpaced)
		t.Logf("FuzzyMatcher on dense vs multi-line body: Matched=%v, Score=%.2f, Diff=%s", res.Matched, res.Score, res.Diff)

		// Test NormalizeText directly
		denseNorm := matcher.NormalizeText("This   is   a    test   with\n\nnewlines\tand   tabs.")
		expectedNorm := "This is a test with newlines and tabs."
		if denseNorm != expectedNorm {
			t.Errorf("NormalizeText mismatch: expected %q, got %q", expectedNorm, denseNorm)
		}
	})

	// 5. Deeply Nested Arrays and Polymorphic Maps
	t.Run("Deeply Nested Arrays & Polymorphic Maps", func(t *testing.T) {
		nested1 := map[string]interface{}{
			"level1": []interface{}{
				map[string]interface{}{
					"c": 3,
					"b": 2,
					"a": 1,
				},
				"simple_string",
				nil,
				false,
			},
		}
		nested2 := map[string]interface{}{
			"level1": []interface{}{
				map[string]interface{}{
					"a": 1,
					"c": 3,
					"b": 2,
				},
				"simple_string",
				nil,
				false,
			},
		}

		b1, _ := matcher.CanonicalJSON(nested1)
		b2, _ := matcher.CanonicalJSON(nested2)
		if string(b1) != string(b2) {
			t.Errorf("CanonicalJSON diverged on nested array of maps: %s vs %s", string(b1), string(b2))
		}
	})
}
