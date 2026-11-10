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

	actHash := ComputeRequestHash(actual)
	if expected.Matcher.StrictHash != "" && expected.Matcher.StrictHash == actHash {
		return MatchResult{
			Matched: true,
			Score:   1.0,
		}
	}

	var expHash string
	if hasRequestContent(expected) {
		expHash = ComputeRequestHash(expected)
	} else {
		expHash = expected.Matcher.StrictHash
	}

	if expHash != "" && expHash == actHash {
		return MatchResult{
			Matched: true,
			Score:   1.0,
		}
	}

	diff := s.explainDiff(expected, actual)
	return MatchResult{
		Matched: false,
		Score:   0.0,
		Diff:    fmt.Sprintf("strict hash mismatch: expected %s, got %s. Details: %s", expHash, actHash, diff),
	}
}

func hasRequestContent(r *cassette.Request) bool {
	return r.ToolName != "" || r.Method != "" || r.Endpoint != "" || r.RawBody != "" || r.Body != nil
}

func (s *StrictMatcher) explainDiff(expected, actual *cassette.Request) string {
	var diffs []string

	if expected.ToolName != "" || actual.ToolName != "" {
		if expected.ToolName != actual.ToolName {
			diffs = append(diffs, fmt.Sprintf("tool_name mismatch: expected %q, got %q", expected.ToolName, actual.ToolName))
		}
		redExp := canonicalizeValue(cassette.RedactMap(expected.ToolInput))
		redAct := canonicalizeValue(cassette.RedactMap(actual.ToolInput))
		if !reflect.DeepEqual(redExp, redAct) {
			diffs = append(diffs, fmt.Sprintf("tool_input args differ: expected %v, got %v", redExp, redAct))
		}
	} else {
		if !strings.EqualFold(expected.Method, actual.Method) {
			diffs = append(diffs, fmt.Sprintf("method mismatch: expected %s, got %s", expected.Method, actual.Method))
		}
		normExpEnd := NormalizeEndpoint(expected.Endpoint)
		normActEnd := NormalizeEndpoint(actual.Endpoint)
		if normExpEnd != normActEnd {
			diffs = append(diffs, fmt.Sprintf("endpoint mismatch: expected %s, got %s", normExpEnd, normActEnd))
		}
		if expected.RawBody != actual.RawBody {
			diffs = append(diffs, "body content mismatch")
		}
	}

	if len(diffs) == 0 {
		return "payload structural differences in JSON values, precision, or encoding"
	}
	return strings.Join(diffs, "; ")
}
