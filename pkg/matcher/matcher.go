package matcher

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/url"
	"sort"
	"strconv"
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

// DecodeJSONUseNumber parses JSON bytes using json.Decoder.UseNumber() so 64-bit integers
// never lose precision via IEEE-754 float64 conversion.
func DecodeJSONUseNumber(data []byte) (interface{}, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty json input")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var extra interface{}
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("trailing tokens in json input")
	}
	return v, nil
}

// CanonicalJSON recursively sorts map keys, normalizes numbers without 64-bit precision loss,
// and returns a deterministic JSON byte slice.
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
	case json.Number:
		return normalizeJSONNumber(val)
	case float64:
		if !math.IsInf(val, 0) && !math.IsNaN(val) && math.Trunc(val) == val &&
			val >= -9007199254740992 && val <= 9007199254740992 {
			return int64(val)
		}
		return val
	case int:
		return int64(val)
	case int32:
		return int64(val)
	default:
		return val
	}
}

func normalizeJSONNumber(num json.Number) interface{} {
	s := num.String()
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i
	}
	if u, err := strconv.ParseUint(s, 10, 64); err == nil {
		return u
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		if !math.IsInf(f, 0) && !math.IsNaN(f) && math.Trunc(f) == f &&
			f >= -9007199254740992 && f <= 9007199254740992 {
			return int64(f)
		}
		return f
	}
	return s
}

// CanonicalHash returns a sha256: hex string for arbitrary data.
func CanonicalHash(data []byte) string {
	h := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(h[:])
}

// NormalizeEndpoint converts full URLs or relative paths into a canonical relative path
// with deterministically sorted query parameters and redacted credentials.
func NormalizeEndpoint(raw string) string {
	s := cassette.RedactString(strings.TrimSpace(raw))
	if s == "" {
		return ""
	}

	var pathPart, rawQuery string
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		if u, err := url.Parse(s); err == nil {
			pathPart = u.Path
			rawQuery = u.RawQuery
		} else {
			pathPart = s
		}
	} else {
		if qIdx := strings.IndexByte(s, '?'); qIdx != -1 {
			pathPart = s[:qIdx]
			rawQuery = s[qIdx+1:]
		} else {
			pathPart = s
		}
	}

	if pathPart == "" {
		pathPart = "/"
	} else if !strings.HasPrefix(pathPart, "/") {
		pathPart = "/" + pathPart
	}
	if len(pathPart) > 1 {
		pathPart = strings.TrimRight(pathPart, "/")
		if pathPart == "" {
			pathPart = "/"
		}
	}

	if rawQuery != "" {
		if vals, err := url.ParseQuery(rawQuery); err == nil {
			for k, list := range vals {
				if cassette.IsSensitiveQueryParam(k) {
					vals.Set(k, "[REDACTED]")
					continue
				}
				for i, item := range list {
					if strings.TrimSpace(item) != "" && cassette.IsRedactedPlaceholder(item) {
						list[i] = "[REDACTED]"
					}
				}
			}
			encoded := vals.Encode()
			if encoded != "" {
				return pathPart + "?" + encoded
			}
		} else {
			return pathPart + "?" + rawQuery
		}
	}
	return pathPart
}

// ComputeToolHash calculates a deterministic, secret-redacted SHA-256 hash for tool execution requests.
func ComputeToolHash(toolName string, args map[string]interface{}) string {
	type canonicalTool struct {
		Name string      `json:"name"`
		Args interface{} `json:"args"`
	}
	redactedArgs := cassette.RedactMap(args)
	t := canonicalTool{
		Name: strings.TrimSpace(toolName),
		Args: canonicalizeValue(redactedArgs),
	}
	b, _ := json.Marshal(t)
	return CanonicalHash(b)
}

// ComputeRequestHash computes a canonical, secret-redacted SHA-256 hash for any Request.
// Redacting secrets prior to hashing eliminates any secret hash oracle and allows CI dummy keys to match.
func ComputeRequestHash(req *cassette.Request) string {
	if req == nil {
		return ""
	}
	if req.ToolName != "" {
		return ComputeToolHash(req.ToolName, req.ToolInput)
	}

	var parts []string
	if req.Method != "" {
		parts = append(parts, strings.ToUpper(strings.TrimSpace(req.Method)))
	}
	if req.Endpoint != "" {
		parts = append(parts, NormalizeEndpoint(req.Endpoint))
	}

	rawTrimmed := strings.TrimSpace(req.RawBody)
	if rawTrimmed != "" {
		redactedRaw := cassette.RedactString(rawTrimmed)
		if parsed, err := DecodeJSONUseNumber([]byte(redactedRaw)); err == nil {
			redactedVal := cassette.RedactValue(parsed)
			if cb, err := CanonicalJSON(redactedVal); err == nil {
				parts = append(parts, string(cb))
			} else {
				parts = append(parts, redactedRaw)
			}
		} else if req.Body != nil {
			redactedBody := cassette.RedactValue(req.Body)
			if cb, err := CanonicalJSON(redactedBody); err == nil {
				parts = append(parts, string(cb))
			} else {
				parts = append(parts, redactedRaw)
			}
		} else {
			parts = append(parts, redactedRaw)
		}
	} else if req.Body != nil {
		redactedBody := cassette.RedactValue(req.Body)
		if cb, err := CanonicalJSON(redactedBody); err == nil {
			parts = append(parts, string(cb))
		}
	}

	payload := strings.Join(parts, "\n")
	return CanonicalHash([]byte(payload))
}
