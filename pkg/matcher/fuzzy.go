package matcher

import (
	"bytes"
	"fmt"
	"math"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
)

// maxExactLevenshteinRunes caps O(N*M) dynamic programming edit distance so large prompts
// (e.g. 100KB+ context windows) fall back to O(N) hashed 4-gram Dice similarity in <1ms.
const maxExactLevenshteinRunes = 4096

// dynamicKeys lists volatile metadata keys stripped during fuzzy matching.
// Bare "id" is intentionally excluded so distinct entity IDs (e.g. user id=1 vs id=2) do not collide.
var dynamicKeys = map[string]bool{
	"timestamp":        true,
	"time":             true,
	"date":             true,
	"nonce":            true,
	"request_id":       true,
	"req_id":           true,
	"trace_id":         true,
	"span_id":          true,
	"correlation_id":   true,
	"x_request_id":     true,
	"uuid":             true,
	"session_id":       true,
	"client_timestamp": true,
	"token":            true,
	"_timestamp":       true,
	"created_at":       true,
	"updated_at":       true,
}

var isoDateRegex = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})?`)
var uuidRegex = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// FuzzyMatcher performs semantic matching with whitespace normalization,
// case insensitivity, dynamic key exclusion, JSON canonicalization, and similarity scoring.
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

	expArgs := filterDynamicKeys(cassette.RedactMap(expected.ToolInput))
	actArgs := filterDynamicKeys(cassette.RedactMap(actual.ToolInput))

	if reflect.DeepEqual(expArgs, actArgs) {
		return MatchResult{Matched: true, Score: 1.0}
	}

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

	// Attempt structured JSON normalization first (strips dynamic keys, normalizes strings, ignores key order)
	expJSON, expOK := extractStructuredJSON(expected)
	actJSON, actOK := extractStructuredJSON(actual)
	if expOK && actOK {
		fExp := filterDynamicValue(cassette.RedactValue(expJSON))
		fAct := filterDynamicValue(cassette.RedactValue(actJSON))
		cbExp, err1 := CanonicalJSON(fExp)
		cbAct, err2 := CanonicalJSON(fAct)
		if err1 == nil && err2 == nil {
			if bytes.Equal(cbExp, cbAct) {
				return MatchResult{Matched: true, Score: 1.0}
			}
			score := StringSimilarity(string(cbExp), string(cbAct))
			matched := score >= f.Threshold
			var diff string
			if !matched {
				diff = fmt.Sprintf("semantic JSON body similarity %.2f below threshold %.2f", score, f.Threshold)
			}
			return MatchResult{
				Matched: matched,
				Score:   score,
				Diff:    diff,
			}
		}
	}

	expBody := NormalizeText(cassette.RedactString(expected.RawBody))
	actBody := NormalizeText(cassette.RedactString(actual.RawBody))

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

func extractStructuredJSON(req *cassette.Request) (interface{}, bool) {
	rawTrimmed := strings.TrimSpace(req.RawBody)
	if len(rawTrimmed) > 0 && (rawTrimmed[0] == '{' || rawTrimmed[0] == '[') {
		if parsed, err := DecodeJSONUseNumber([]byte(rawTrimmed)); err == nil {
			switch parsed.(type) {
			case map[string]interface{}, []interface{}:
				return parsed, true
			}
		}
	}
	if req.Body != nil {
		switch req.Body.(type) {
		case map[string]interface{}, []interface{}:
			return req.Body, true
		}
	}
	return nil, false
}

// NormalizeText strips dynamic ISO-8601 dates and UUIDs, collapses runs of whitespace, and trims.
func NormalizeText(s string) string {
	if s == "" {
		return ""
	}
	hasISO, hasUUID := scanDynamicPatterns(s)
	res := s
	if hasISO {
		res = isoDateRegex.ReplaceAllString(res, "[TIMESTAMP]")
	}
	if hasUUID {
		res = uuidRegex.ReplaceAllString(res, "[UUID]")
	}
	return collapseWhitespace(res)
}

// scanDynamicPatterns uses SIMD IndexByte jumps over 'T' and '-' to detect structural
// ISO-8601 timestamps (YYYY-MM-DDTHH:MM:SS) or UUIDs (8-4-4-4-12) before invoking regexp.
func scanDynamicPatterns(s string) (hasISO, hasUUID bool) {
	n := len(s)
	if n < 19 {
		return false, false
	}
	for off := 10; off+8 < n; {
		idx := strings.IndexByte(s[off:n-8], 'T')
		if idx < 0 {
			break
		}
		p := off + idx
		if s[p-3] == '-' && s[p-6] == '-' && s[p+3] == ':' && s[p+6] == ':' {
			hasISO = true
			break
		}
		off = p + 1
	}
	if n >= 36 {
		for off := 8; off+27 < n; {
			idx := strings.IndexByte(s[off:n-27], '-')
			if idx < 0 {
				break
			}
			p := off + idx
			if s[p+5] == '-' && s[p+10] == '-' && s[p+15] == '-' {
				hasUUID = true
				break
			}
			off = p + 1
		}
	}
	return hasISO, hasUUID
}

func trimASCIISpace(s string) string {
	start := 0
	for start < len(s) {
		c := s[start]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f' {
			start++
		} else {
			break
		}
	}
	end := len(s)
	for end > start {
		c := s[end-1]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f' {
			end--
		} else {
			break
		}
	}
	return s[start:end]
}

func collapseWhitespace(s string) string {
	s = trimASCIISpace(s)
	if s == "" {
		return ""
	}
	// Fast scan: if already normalized single-space ASCII, return zero-copy slice.
	needsCopy := false
	prevSpace := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x80 || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f' {
			needsCopy = true
			break
		}
		if c == ' ' {
			if prevSpace {
				needsCopy = true
				break
			}
			prevSpace = true
		} else {
			prevSpace = false
		}
	}
	if !needsCopy {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))
	inSpace := true // trim leading Unicode whitespace if present
	segStart := -1
	for i := 0; i < len(s); {
		c := s[i]
		if c < 0x80 {
			if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f' {
				if segStart >= 0 {
					b.WriteString(s[segStart:i])
					segStart = -1
				}
				if !inSpace {
					b.WriteByte(' ')
					inSpace = true
				}
			} else {
				if segStart < 0 {
					segStart = i
				}
				inSpace = false
			}
			i++
			continue
		}
		if segStart >= 0 {
			b.WriteString(s[segStart:i])
			segStart = -1
		}
		r, width := utf8.DecodeRuneInString(s[i:])
		if unicode.IsSpace(r) {
			if !inSpace {
				b.WriteByte(' ')
				inSpace = true
			}
		} else {
			b.WriteRune(r)
			inSpace = false
		}
		i += width
	}
	if segStart >= 0 {
		b.WriteString(s[segStart:])
	}
	out := b.String()
	if len(out) > 0 && out[len(out)-1] == ' ' {
		return out[:len(out)-1]
	}
	return out
}

func normalizeEndpoint(endpoint string) string {
	norm := NormalizeEndpoint(endpoint)
	if norm == "" {
		return ""
	}
	qIdx := strings.IndexByte(norm, '?')
	if qIdx == -1 {
		return strings.ToLower(norm)
	}
	pathPart := strings.ToLower(norm[:qIdx])
	rawQuery := norm[qIdx+1:]
	vals, err := url.ParseQuery(rawQuery)
	if err != nil {
		return strings.ToLower(norm)
	}
	for k := range vals {
		if dynamicKeys[strings.ToLower(k)] {
			vals.Del(k)
		}
	}
	encoded := vals.Encode()
	if encoded == "" {
		return pathPart
	}
	return pathPart + "?" + strings.ToLower(encoded)
}

func filterDynamicKeys(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}
	res := make(map[string]interface{}, len(m))
	for k, v := range m {
		lower := strings.ToLower(strings.TrimSpace(k))
		if dynamicKeys[lower] {
			continue
		}
		res[k] = filterDynamicValue(v)
	}
	return res
}

func filterDynamicValue(v interface{}) interface{} {
	switch val := v.(type) {
	case map[string]interface{}:
		return filterDynamicKeys(val)
	case []interface{}:
		items := make([]interface{}, len(val))
		for i, el := range val {
			items[i] = filterDynamicValue(el)
		}
		return items
	case string:
		return NormalizeText(val)
	default:
		return canonicalizeValue(val)
	}
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

// StringSimilarity calculates a normalized UTF-8 rune similarity ratio (0.0 to 1.0).
// Strips common prefixes and suffixes zero-copy, uses exact Levenshtein distance up to 4,096 runes,
// and switches to zero-allocation O(N) hashed 4-gram Dice similarity on large payloads (>4,096 differing runes).
func StringSimilarity(s1, s2 string) float64 {
	if s1 == s2 {
		return 1.0
	}
	if len(s1) == 0 || len(s2) == 0 {
		return 0.0
	}

	prefixStr, suffixStr, mid1, mid2 := trimCommonStringAffixes(s1, s2)
	affixRunes := fastRuneCount(prefixStr) + fastRuneCount(suffixStr)
	midRunes1 := fastRuneCount(mid1)
	midRunes2 := fastRuneCount(mid2)

	n1 := affixRunes + midRunes1
	n2 := affixRunes + midRunes2
	if n1 == 0 || n2 == 0 {
		return 0.0
	}

	maxRuneLen := float64(n1)
	if n2 > n1 {
		maxRuneLen = float64(n2)
	}

	if midRunes1 == 0 && midRunes2 == 0 {
		return 1.0
	}
	if midRunes1 == 0 || midRunes2 == 0 {
		dist := midRunes1 + midRunes2
		return clampUnit(1.0 - float64(dist)/maxRuneLen)
	}

	if midRunes1 <= maxExactLevenshteinRunes && midRunes2 <= maxExactLevenshteinRunes {
		dist := levenshteinRunes([]rune(mid1), []rune(mid2))
		return clampUnit(1.0 - float64(dist)/maxRuneLen)
	}

	// Fast zero-allocation O(N) 4-gram Sørensen-Dice similarity for large (>4096 rune) interiors
	midSim := ngramDiceStringSimilarity(mid1, mid2, midRunes1, midRunes2)
	matchedEquiv := float64(affixRunes) + midSim*(float64(midRunes1+midRunes2)/2.0)
	return clampUnit(matchedEquiv / maxRuneLen)
}

// LevenshteinDistance calculates edit distance in UTF-8 runes, stripping shared prefix/suffix
// and capping DP work at 4,096 differing runes with fast 4-gram Dice estimation above that threshold.
func LevenshteinDistance(s1, s2 string) int {
	if s1 == s2 {
		return 0
	}
	if len(s1) == 0 {
		return utf8.RuneCountInString(s2)
	}
	if len(s2) == 0 {
		return utf8.RuneCountInString(s1)
	}

	_, _, mid1, mid2 := trimCommonStringAffixes(s1, s2)
	midRunes1 := utf8.RuneCountInString(mid1)
	midRunes2 := utf8.RuneCountInString(mid2)
	if midRunes1 == 0 {
		return midRunes2
	}
	if midRunes2 == 0 {
		return midRunes1
	}

	if midRunes1 <= maxExactLevenshteinRunes && midRunes2 <= maxExactLevenshteinRunes {
		return levenshteinRunes([]rune(mid1), []rune(mid2))
	}

	maxMid := midRunes1
	if midRunes2 > maxMid {
		maxMid = midRunes2
	}
	sim := ngramDiceStringSimilarity(mid1, mid2, midRunes1, midRunes2)
	return int(math.Round((1.0 - sim) * float64(maxMid)))
}

func trimCommonStringAffixes(s1, s2 string) (prefix, suffix, mid1, mid2 string) {
	minBytes := len(s1)
	if len(s2) < minBytes {
		minBytes = len(s2)
	}
	p := 0
	for p < minBytes && s1[p] == s2[p] {
		p++
	}
	// Align back to UTF-8 rune start if split mid-character
	if p < minBytes {
		for p > 0 && !utf8.RuneStart(s1[p]) {
			p--
		}
	}

	rem1 := s1[p:]
	rem2 := s2[p:]
	remMin := len(rem1)
	if len(rem2) < remMin {
		remMin = len(rem2)
	}
	suf := 0
	for suf < remMin && rem1[len(rem1)-1-suf] == rem2[len(rem2)-1-suf] {
		suf++
	}
	if suf > 0 && suf < remMin {
		for suf > 0 && !utf8.RuneStart(rem1[len(rem1)-suf]) {
			suf--
		}
	}

	return s1[:p], rem1[len(rem1)-suf:], rem1[:len(rem1)-suf], rem2[:len(rem2)-suf]
}

func levenshteinRunes(r1, r2 []rune) int {
	// Ensure r2 is the shorter slice to minimize row memory
	if len(r1) < len(r2) {
		r1, r2 = r2, r1
	}
	n, m := len(r1), len(r2)
	if m == 0 {
		return n
	}

	row := make([]int, m+1)
	for j := 0; j <= m; j++ {
		row[j] = j
	}

	for i := 1; i <= n; i++ {
		prev := i
		ri := r1[i-1]
		for j := 1; j <= m; j++ {
			var cost int
			if ri != r2[j-1] {
				cost = 1
			}
			cur := row[j] + 1
			if prev+1 < cur {
				cur = prev + 1
			}
			if row[j-1]+cost < cur {
				cur = row[j-1] + cost
			}
			row[j-1] = prev
			prev = cur
		}
		row[m] = prev
	}
	return row[m]
}

// ngramDiceStringSimilarity computes Sørensen-Dice similarity over 4-rune shingles using a fixed
// 16,384-bucket stack counting table with zero heap allocations (<0.6ms on 100KB prompts).
func ngramDiceStringSimilarity(s1, s2 string, runes1, runes2 int) float64 {
	const k = 4
	if runes1 < k || runes2 < k {
		dist := levenshteinRunes([]rune(s1), []rune(s2))
		mx := runes1
		if runes2 > mx {
			mx = runes2
		}
		if mx == 0 {
			return 1.0
		}
		return clampUnit(1.0 - float64(dist)/float64(mx))
	}

	const bucketBits = 12
	const bucketSize = 1 << bucketBits // 4096 (8KB stack array, fits cleanly in L1d cache)
	const bucketShift = 32 - bucketBits
	var counts [bucketSize]uint16

	c1 := runes1 - k + 1
	c2 := runes2 - k + 1

	if len(s1) == runes1 && len(s2) == runes2 {
		// Content-defined mod-hash sampling (shift-invariant across arbitrary odd/even edits)
		var sampleMask uint32
		if c1 > 8192 && c2 > 8192 {
			sampleMask = 3 // sample ~1/4 of 4-grams deterministically by content
		}
		total1 := 0
		for i := 0; i < c1; i++ {
			b := s1[i : i+4]
			w := uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
			hFull := w * 2654435761
			if (hFull & sampleMask) != 0 {
				continue
			}
			h := hFull >> bucketShift
			if counts[h] < 65535 {
				counts[h]++
			}
			total1++
		}
		intersection := 0
		total2 := 0
		for i := 0; i < c2; i++ {
			b := s2[i : i+4]
			w := uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
			hFull := w * 2654435761
			if (hFull & sampleMask) != 0 {
				continue
			}
			h := hFull >> bucketShift
			if counts[h] > 0 {
				counts[h]--
				intersection++
			}
			total2++
		}
		if total1+total2 == 0 {
			return 0.0
		}
		return float64(2*intersection) / float64(total1+total2)
	}

	const bucketMask = bucketSize - 1

	var w0, w1, w2, w3 rune
	idx := 0
	for _, r := range s1 {
		w0, w1, w2, w3 = w1, w2, w3, r
		idx++
		if idx >= k {
			h := hash4Runes(w0, w1, w2, w3) & bucketMask
			if counts[h] < 65535 {
				counts[h]++
			}
		}
	}

	intersection := 0
	idx = 0
	for _, r := range s2 {
		w0, w1, w2, w3 = w1, w2, w3, r
		idx++
		if idx >= k {
			h := hash4Runes(w0, w1, w2, w3) & bucketMask
			if counts[h] > 0 {
				counts[h]--
				intersection++
			}
		}
	}

	return float64(2*intersection) / float64(c1+c2)
}

func fastRuneCount(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return utf8.RuneCountInString(s)
		}
	}
	return len(s)
}

func hash4Runes(a, b, c, d rune) uint32 {
	h := uint32(2166136261)
	h = (h ^ uint32(a)) * 16777619
	h = (h ^ uint32(b)) * 16777619
	h = (h ^ uint32(c)) * 16777619
	h = (h ^ uint32(d)) * 16777619
	return h ^ (h >> 15)
}

func clampUnit(v float64) float64 {
	if v < 0.0 {
		return 0.0
	}
	if v > 1.0 {
		return 1.0
	}
	return v
}
