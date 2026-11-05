package matcher

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
)

// StrictMatcher requires byte-for-byte canonical hash equality.
type StrictMatcher struct{}

// NewStrictMatcher returns an initialized StrictMatcher.
func NewStrictMatcher() *StrictMatcher {
	return &StrictMatcher{}
}

func (s *StrictMatcher) Name() string {
	return "strict"
}

func (s *StrictMatcher) Match(expected, actual *cassette.Request) MatchResult {
	if expected == nil || actual == nil {
		return MatchResult{
			Matched: false,
			Score:   0.0,
			Diff:    "one of expected or actual request is nil",
		}
	}

	expHash := expected.Matcher.StrictHash
	if expHash == "" {
		expHash = ComputeRequestHash(expected)
	}

	actHash := actual.Matcher.StrictHash
	if actHash == "" {
		actHash = ComputeRequestHash(actual)
	}

	if expHash == actHash {
		return MatchResult{
			Matched: true,
			Score:   1.0,
		}
	}

	// Produce actionable diff explanation
	diff := s.explainDiff(expected, actual)
	return MatchResult{
		Matched: false,
		Score:   0.0,
		Diff:    fmt.Sprintf("strict hash mismatch: expected %s, got %s. Details: %s", expHash, actHash, diff),
	}
}

func (s *StrictMatcher) explainDiff(expected, actual *cassette.Request) string {
	var diffs []string

	if expected.ToolName != "" || actual.ToolName != "" {
		if expected.ToolName != actual.ToolName {
			diffs = append(diffs, fmt.Sprintf("tool_name mismatch: expected %q, got %q", expected.ToolName, actual.ToolName))
		}
		if !reflect.DeepEqual(expected.ToolInput, actual.ToolInput) {
			diffs = append(diffs, fmt.Sprintf("tool_input args differ: expected %v, got %v", expected.ToolInput, actual.ToolInput))
		}
	} else {
		if !strings.EqualFold(expected.Method, actual.Method) {
			diffs = append(diffs, fmt.Sprintf("method mismatch: expected %s, got %s", expected.Method, actual.Method))
		}
		if expected.Endpoint != actual.Endpoint {
			diffs = append(diffs, fmt.Sprintf("endpoint mismatch: expected %s, got %s", expected.Endpoint, actual.Endpoint))
		}
		if expected.RawBody != actual.RawBody {
			diffs = append(diffs, "body content mismatch")
		}
	}

	if len(diffs) == 0 {
		return "payload structural differences in whitespace, headers, or encoding"
	}
	return strings.Join(diffs, "; ")
}
