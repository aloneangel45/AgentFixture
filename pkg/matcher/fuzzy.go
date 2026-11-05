package matcher

import (
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strings"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
)

var dynamicKeys = map[string]bool{
	"timestamp":        true,
	"time":             true,
	"date":             true,
	"nonce":            true,
	"request_id":       true,
	"req_id":           true,
	"id":               true,
	"uuid":             true,
	"session_id":       true,
	"client_timestamp": true,
	"token":            true,
	"_timestamp":       true,
	"created_at":       true,
	"updated_at":       true,
}

var multiSpaceRegex = regexp.MustCompile(`\s+`)
var isoDateRegex = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})?`)
var uuidRegex = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// FuzzyMatcher performs semantic matching with whitespace normalization,
// case insensitivity, dynamic key exclusion, and Levenshtein similarity.
type FuzzyMatcher struct {
	Threshold float64
}

// NewFuzzyMatcher creates a FuzzyMatcher with a given or default similarity threshold (0.80).
func NewFuzzyMatcher(threshold float64) *FuzzyMatcher {
	if threshold <= 0 || threshold > 1.0 {
		threshold = 0.80
	}
	return &FuzzyMatcher{Threshold: threshold}
}

func (f *FuzzyMatcher) Name() string {
	return "fuzzy"
}

func (f *FuzzyMatcher) Match(expected, actual *cassette.Request) MatchResult {
	if expected == nil || actual == nil {
		return MatchResult{Matched: false, Score: 0.0, Diff: "nil request pointer"}
	}

	// 1. Tool execution comparison
	if expected.ToolName != "" || actual.ToolName != "" {
		return f.matchTool(expected, actual)
	}

	// 2. HTTP / General Request comparison
	return f.matchHTTP(expected, actual)
}

func (f *FuzzyMatcher) matchTool(expected, actual *cassette.Request) MatchResult {
	expName := strings.ToLower(strings.TrimSpace(expected.ToolName))
	actName := strings.ToLower(strings.TrimSpace(actual.ToolName))

	if expName != actName {
		return MatchResult{
			Matched: false,
			Score:   0.0,
			Diff:    fmt.Sprintf("tool name mismatch: expected %q, got %q", expected.ToolName, actual.ToolName),
		}
	}

	// Clean dynamic keys from tool arguments
	expArgs := filterDynamicKeys(expected.ToolInput)
	actArgs := filterDynamicKeys(actual.ToolInput)

	if reflect.DeepEqual(expArgs, actArgs) {
		return MatchResult{Matched: true, Score: 1.0}
	}

	// Compute similarity score across arguments
	score, diff := compareFilteredArgs(expArgs, actArgs)
	matched := score >= f.Threshold
	return MatchResult{
		Matched: matched,
		Score:   score,
		Diff:    diff,
	}
}

func (f *FuzzyMatcher) matchHTTP(expected, actual *cassette.Request) MatchResult {
	if !strings.EqualFold(expected.Method, actual.Method) {
		return MatchResult{
			Matched: false,
			Score:   0.0,
			Diff:    fmt.Sprintf("method mismatch: expected %s, got %s", expected.Method, actual.Method),
		}
	}

	expPath := normalizeEndpoint(expected.Endpoint)
	actPath := normalizeEndpoint(actual.Endpoint)
	if expPath != actPath {
		return MatchResult{
			Matched: false,
			Score:   0.0,
			Diff:    fmt.Sprintf("endpoint mismatch: expected %s, got %s", expPath, actPath),
		}
	}

	expBody := NormalizeText(expected.RawBody)
	actBody := NormalizeText(actual.RawBody)

	if expBody == actBody {
		return MatchResult{Matched: true, Score: 1.0}
	}

	score := StringSimilarity(expBody, actBody)
	matched := score >= f.Threshold

	var diff string
	if !matched {
		diff = fmt.Sprintf("semantic body similarity %.2f below threshold %.2f", score, f.Threshold)
	}

	return MatchResult{
		Matched: matched,
		Score:   score,
		Diff:    diff,
	}
}

// NormalizeText strips dynamic dates, UUIDs, collapses whitespace, and trims.
func NormalizeText(s string) string {
	if s == "" {
		return ""
	}
	res := isoDateRegex.ReplaceAllString(s, "[TIMESTAMP]")
	res = uuidRegex.ReplaceAllString(res, "[UUID]")
	res = multiSpaceRegex.ReplaceAllString(res, " ")
	return strings.TrimSpace(res)
}

func normalizeEndpoint(endpoint string) string {
	parts := strings.SplitN(endpoint, "?", 2)
	path := strings.TrimRight(parts[0], "/")
	return strings.ToLower(path)
}

func filterDynamicKeys(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}
	res := make(map[string]interface{}, len(m))
	for k, v := range m {
		lower := strings.ToLower(k)
		if dynamicKeys[lower] {
			continue
		}
		switch val := v.(type) {
		case map[string]interface{}:
			res[k] = filterDynamicKeys(val)
		case string:
			res[k] = NormalizeText(val)
		default:
			res[k] = val
		}
	}
	return res
}

func compareFilteredArgs(exp, act map[string]interface{}) (float64, string) {
	allKeys := make(map[string]bool)
	for k := range exp {
		allKeys[k] = true
	}
	for k := range act {
		allKeys[k] = true
	}

	if len(allKeys) == 0 {
		return 1.0, ""
	}

	var matchCount float64
	var diffs []string

	for k := range allKeys {
		vExp, hasExp := exp[k]
		vAct, hasAct := act[k]

		if !hasExp {
			diffs = append(diffs, fmt.Sprintf("unexpected arg '%s': %v", k, vAct))
			continue
		}
		if !hasAct {
			diffs = append(diffs, fmt.Sprintf("missing expected arg '%s'", k))
			continue
		}

		if reflect.DeepEqual(vExp, vAct) {
			matchCount += 1.0
		} else {
			sExp := fmt.Sprintf("%v", vExp)
			sAct := fmt.Sprintf("%v", vAct)
			sim := StringSimilarity(sExp, sAct)
			if sim >= 0.85 {
				matchCount += sim
			} else {
				diffs = append(diffs, fmt.Sprintf("arg '%s' drifted (exp: %v, got: %v)", k, vExp, vAct))
			}
		}
	}

	total := float64(len(allKeys))
	score := matchCount / total
	return score, strings.Join(diffs, "; ")
}

// StringSimilarity calculates a normalized similarity ratio (0.0 to 1.0) using Levenshtein distance.
func StringSimilarity(s1, s2 string) float64 {
	if s1 == s2 {
		return 1.0
	}
	len1 := len(s1)
	len2 := len(s2)
	if len1 == 0 || len2 == 0 {
		return 0.0
	}

	dist := LevenshteinDistance(s1, s2)
	maxLen := math.Max(float64(len1), float64(len2))
	return 1.0 - (float64(dist) / maxLen)
}

// LevenshteinDistance calculates the edit distance between two strings.
func LevenshteinDistance(s1, s2 string) int {
	r1, r2 := []rune(s1), []rune(s2)
	n, m := len(r1), len(r2)

	if n == 0 {
		return m
	}
	if m == 0 {
		return n
	}

	row := make([]int, m+1)
	for j := 0; j <= m; j++ {
		row[j] = j
	}

	for i := 1; i <= n; i++ {
		prev := i
		for j := 1; j <= m; j++ {
			var cost int
			if r1[i-1] != r2[j-1] {
				cost = 1
			}
			cur := row[j] + 1 // deletion
			if prev+1 < cur { // insertion
				cur = prev + 1
			}
			if row[j-1]+cost < cur { // substitution
				cur = row[j-1] + cost
			}
			row[j-1] = prev
			prev = cur
		}
		row[m] = prev
	}

	return row[m]
}
