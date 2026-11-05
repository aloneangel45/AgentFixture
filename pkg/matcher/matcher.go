package matcher

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
)

// MatchResult contains the outcome of a match comparison.
type MatchResult struct {
	Matched bool    `json:"matched"`
	Score   float64 `json:"score"`
	Diff    string  `json:"diff,omitempty"`
}

// Matcher evaluates whether an incoming request matches a recorded cassette interaction.
type Matcher interface {
	Name() string
	Match(expected, actual *cassette.Request) MatchResult
}

// CanonicalJSON recursively sorts map keys and returns a deterministic JSON byte slice.
func CanonicalJSON(v interface{}) ([]byte, error) {
	norm := canonicalizeValue(v)
	return json.Marshal(norm)
}

func canonicalizeValue(v interface{}) interface{} {
	switch val := v.(type) {
	case map[string]interface{}:
		sortedMap := make(map[string]interface{}, len(val))
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sortedMap[k] = canonicalizeValue(val[k])
		}
		return sortedMap
	case []interface{}:
		items := make([]interface{}, len(val))
		for i, el := range val {
			items[i] = canonicalizeValue(el)
		}
		return items
	default:
		return val
	}
}

// CanonicalHash returns a sha256: hex string for arbitrary data.
func CanonicalHash(data []byte) string {
	h := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(h[:])
}

// ComputeToolHash calculates a deterministic SHA-256 hash for tool execution requests.
func ComputeToolHash(toolName string, args map[string]interface{}) string {
	type canonicalTool struct {
		Name string      `json:"name"`
		Args interface{} `json:"args"`
	}
	t := canonicalTool{
		Name: strings.TrimSpace(toolName),
		Args: canonicalizeValue(args),
	}
	b, _ := json.Marshal(t)
	return CanonicalHash(b)
}

// ComputeRequestHash computes a canonical SHA-256 hash for any Request.
func ComputeRequestHash(req *cassette.Request) string {
	if req.ToolName != "" {
		return ComputeToolHash(req.ToolName, req.ToolInput)
	}

	var parts []string
	if req.Method != "" {
		parts = append(parts, strings.ToUpper(strings.TrimSpace(req.Method)))
	}
	if req.Endpoint != "" {
		parts = append(parts, strings.TrimSpace(req.Endpoint))
	}

	if req.Body != nil {
		cb, err := CanonicalJSON(req.Body)
		if err == nil {
			parts = append(parts, string(cb))
		}
	} else if req.RawBody != "" {
		parts = append(parts, strings.TrimSpace(req.RawBody))
	}

	payload := strings.Join(parts, "\n")
	return CanonicalHash([]byte(payload))
}
