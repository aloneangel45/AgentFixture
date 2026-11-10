package cassette

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var (
	pemPrivateKeyRegex = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	bearerTokenRegex   = regexp.MustCompile(`(?i)\bBearer\s+([a-zA-Z0-9_\-\.~+/]+=*)`)
	anthropicKeyRegex  = regexp.MustCompile(`sk-ant-[a-zA-Z0-9_-]{16,}`)
	openaiProjRegex    = regexp.MustCompile(`sk-(?:proj|svcacct|admin)-[a-zA-Z0-9_-]{16,}`)
	openaiKeyRegex     = regexp.MustCompile(`sk-[a-zA-Z0-9_-]{20,}`)
	googleKeyRegex     = regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)
	githubPatRegex     = regexp.MustCompile(`github_pat_[a-zA-Z0-9_]{22,}`)
	githubClassicRegex = regexp.MustCompile(`\bgh[pousr]_[a-zA-Z0-9]{30,}\b`)
	awsAccessKeyRegex  = regexp.MustCompile(`\bA(?:KIA|SIA)[0-9A-Z]{16}\b`)
	slackTokenRegex    = regexp.MustCompile(`\bxox[baprs]-[0-9A-Za-z-]{10,}\b`)
	huggingFaceRegex   = regexp.MustCompile(`\bhf_[a-zA-Z0-9]{20,}\b`)
	urlBasicAuthRegex  = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^/\s:@\[\]]+:)([^@\s/\[\]]+)(@)`)
	querySecretRegex   = regexp.MustCompile(`(?i)([?&](?:api_key|apikey|access_token|client_secret|password|secret|x-goog-api-key)=)([^&\s"'\[\]]+)`)
)

var sensitiveHeaderNames = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"x-api-key":           true,
	"x-goog-api-key":      true,
	"api-key":             true,
	"anthropic-api-key":   true,
	"openai-api-key":      true,
	"x-auth-token":        true,
	"cookie":              true,
	"set-cookie":          true,
}

var sensitiveKeyNames = map[string]bool{
	"password":       true,
	"passwd":         true,
	"secret":         true,
	"api_key":        true,
	"apikey":         true,
	"x_api_key":      true,
	"x-api-key":      true,
	"x-goog-api-key": true,
	"access_token":   true,
	"refresh_token":  true,
	"id_token":       true,
	"private_key":    true,
	"token":          true,
	"client_secret":  true,
	"authorization":  true,
	"cookie":         true,
	"set-cookie":     true,
	"session_token":  true,
	"auth_token":     true,
}

// RedactString sanitizes known API keys, tokens, PEM keys, URL credentials, and secrets from arbitrary text.
func RedactString(input string) string {
	if len(input) < 6 {
		return input
	}
	s := input
	if strings.Contains(s, "PRIVATE KEY-----") {
		s = pemPrivateKeyRegex.ReplaceAllString(s, "[REDACTED_PRIVATE_KEY]")
	}
	if hasBearerCandidate(s) {
		s = bearerTokenRegex.ReplaceAllString(s, "Bearer [REDACTED_TOKEN]")
	}
	if strings.Contains(s, "sk-") {
		if strings.Contains(s, "sk-ant-") {
			s = anthropicKeyRegex.ReplaceAllString(s, "[REDACTED_ANTHROPIC_KEY]")
		}
		s = openaiProjRegex.ReplaceAllString(s, "[REDACTED_OPENAI_KEY]")
		s = openaiKeyRegex.ReplaceAllString(s, "[REDACTED_OPENAI_KEY]")
	}
	if strings.Contains(s, "AIza") {
		s = googleKeyRegex.ReplaceAllString(s, "[REDACTED_GOOGLE_KEY]")
	}
	if strings.Contains(s, "github_pat_") {
		s = githubPatRegex.ReplaceAllString(s, "[REDACTED_GITHUB_TOKEN]")
	}
	if strings.Contains(s, "gh") && (strings.Contains(s, "ghp_") || strings.Contains(s, "gho_") || strings.Contains(s, "ghu_") || strings.Contains(s, "ghs_") || strings.Contains(s, "ghr_")) {
		s = githubClassicRegex.ReplaceAllString(s, "[REDACTED_GITHUB_TOKEN]")
	}
	if strings.Contains(s, "IA") && (strings.Contains(s, "AKIA") || strings.Contains(s, "ASIA")) {
		s = awsAccessKeyRegex.ReplaceAllString(s, "[REDACTED_AWS_KEY]")
	}
	if strings.Contains(s, "xox") {
		s = slackTokenRegex.ReplaceAllString(s, "[REDACTED_SLACK_TOKEN]")
	}
	if strings.Contains(s, "hf_") {
		s = huggingFaceRegex.ReplaceAllString(s, "[REDACTED_HF_TOKEN]")
	}
	if strings.IndexByte(s, '@') != -1 && strings.Contains(s, "://") {
		s = urlBasicAuthRegex.ReplaceAllString(s, "${1}[REDACTED]${3}")
	}
	if strings.IndexByte(s, '=') != -1 && (strings.IndexByte(s, '?') != -1 || strings.IndexByte(s, '&') != -1) {
		s = querySecretRegex.ReplaceAllString(s, "${1}[REDACTED]")
	}
	return s
}

func hasBearerCandidate(s string) bool {
	n := len(s)
	if n < 7 {
		return false
	}
	for _, first := range [2]byte{'B', 'b'} {
		for off := 0; off+6 < n; {
			idx := strings.IndexByte(s[off:n-6], first)
			if idx < 0 {
				break
			}
			p := off + idx
			if (s[p+1]|0x20) == 'e' && (s[p+2]|0x20) == 'a' && (s[p+3]|0x20) == 'r' && (s[p+4]|0x20) == 'e' && (s[p+5]|0x20) == 'r' {
				return true
			}
			off = p + 1
		}
	}
	return false
}

// RedactHeaders masks any known sensitive authentication or cookie headers.
func RedactHeaders(headers map[string]string) map[string]string {
	if headers == nil {
		return nil
	}
	redacted := make(map[string]string, len(headers))
	for k, v := range headers {
		lower := strings.ToLower(strings.TrimSpace(k))
		if sensitiveHeaderNames[lower] {
			redacted[k] = "[REDACTED]"
		} else {
			redacted[k] = RedactString(v)
		}
	}
	return redacted
}

// RedactStringMap scrubs sensitive keys and string values in string maps (tags, metadata).
func RedactStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	res := make(map[string]string, len(m))
	for k, v := range m {
		lower := strings.ToLower(strings.TrimSpace(k))
		if sensitiveKeyNames[lower] || sensitiveHeaderNames[lower] {
			res[k] = "[REDACTED]"
		} else {
			res[k] = RedactString(v)
		}
	}
	return res
}

// RedactMap recursively scrubs sensitive fields and masks known patterns in map values.
func RedactMap(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}
	res := make(map[string]interface{}, len(m))
	for k, v := range m {
		lower := strings.ToLower(strings.TrimSpace(k))
		if sensitiveKeyNames[lower] || sensitiveHeaderNames[lower] {
			res[k] = "[REDACTED]"
			continue
		}
		res[k] = RedactValue(v)
	}
	return res
}

// RedactValue recursively scrubs strings, maps, and slices for secret values.
func RedactValue(val interface{}) interface{} {
	switch v := val.(type) {
	case string:
		return RedactString(v)
	case map[string]interface{}:
		return RedactMap(v)
	case map[string]string:
		return RedactStringMap(v)
	case []interface{}:
		items := make([]interface{}, len(v))
		for i, el := range v {
			items[i] = RedactValue(el)
		}
		return items
	default:
		return val
	}
}

// RedactRequest scrubs secrets from all fields of a Request.
func RedactRequest(req *Request) {
	if req == nil {
		return
	}
	req.Endpoint = RedactString(req.Endpoint)
	req.Headers = RedactHeaders(req.Headers)
	req.RawBody = RedactString(req.RawBody)
	if req.Body != nil {
		req.Body = RedactValue(req.Body)
	}
	if req.ToolInput != nil {
		req.ToolInput = RedactMap(req.ToolInput)
	}
	req.Matcher.FuzzySignature.NormalizedPrompt = RedactString(req.Matcher.FuzzySignature.NormalizedPrompt)
	if req.Matcher.FuzzySignature.NormalizedArgs != nil {
		req.Matcher.FuzzySignature.NormalizedArgs = RedactMap(req.Matcher.FuzzySignature.NormalizedArgs)
	}
}

// RedactInteraction applies secret scrubbing to request, response, SSE events, and metadata fields.
func RedactInteraction(inter *Interaction) {
	if inter == nil {
		return
	}
	RedactRequest(&inter.Request)
	inter.Metadata = RedactStringMap(inter.Metadata)

	inter.Response.Headers = RedactHeaders(inter.Response.Headers)
	inter.Response.RawBody = RedactString(inter.Response.RawBody)
	inter.Response.ToolResult = RedactString(inter.Response.ToolResult)
	inter.Response.Error = RedactString(inter.Response.Error)
	if inter.Response.Body != nil {
		inter.Response.Body = RedactValue(inter.Response.Body)
	}
	for i := range inter.Response.SSEEvents {
		inter.Response.SSEEvents[i].Data = RedactString(inter.Response.SSEEvents[i].Data)
		inter.Response.SSEEvents[i].Comment = RedactString(inter.Response.SSEEvents[i].Comment)
		inter.Response.SSEEvents[i].ID = RedactString(inter.Response.SSEEvents[i].ID)
	}
}

// RedactCassette scrubs all interactions and metadata within a Cassette.
func RedactCassette(c *Cassette) {
	if c == nil {
		return
	}
	c.Metadata.Description = RedactString(c.Metadata.Description)
	c.Metadata.Tags = RedactStringMap(c.Metadata.Tags)
	for i := range c.Interactions {
		RedactInteraction(&c.Interactions[i])
	}
}

func isRedactedPlaceholder(val string) bool {
	trimmed := strings.TrimSpace(val)
	if trimmed == "" {
		return true
	}
	if strings.HasPrefix(trimmed, "[REDACTED") {
		return true
	}
	if strings.HasPrefix(strings.ToLower(trimmed), "bearer [redacted") {
		return true
	}
	return false
}

// CheckForUnredactedSecrets inspects a cassette and returns true if raw secrets remain.
// Matched secret values are strictly masked in the returned reason string so secret bytes never leak to logs.
func CheckForUnredactedSecrets(c *Cassette) (bool, string) {
	if c == nil {
		return false, ""
	}

	data, err := json.Marshal(c)
	if err != nil {
		return false, ""
	}
	raw := string(data)

	// 1. Check known provider credential formats across all serialized fields
	if pemPrivateKeyRegex.MatchString(raw) {
		return true, "unredacted PEM private key detected (-----BEGIN *** PRIVATE KEY-----)"
	}
	if anthropicKeyRegex.MatchString(raw) {
		return true, "unredacted Anthropic key detected (sk-ant-***)"
	}
	if openaiProjRegex.MatchString(raw) {
		return true, "unredacted OpenAI project key detected (sk-proj-***)"
	}
	if openaiKeyRegex.MatchString(raw) {
		return true, "unredacted OpenAI key detected (sk-***)"
	}
	if googleKeyRegex.MatchString(raw) {
		return true, "unredacted Google API key detected (AIza***)"
	}
	if githubPatRegex.MatchString(raw) {
		return true, "unredacted GitHub PAT detected (github_pat_***)"
	}
	if githubClassicRegex.MatchString(raw) {
		return true, "unredacted GitHub token detected (gh*_**)"
	}
	if awsAccessKeyRegex.MatchString(raw) {
		return true, "unredacted AWS access key detected (AKIA/ASIA***)"
	}
	if slackTokenRegex.MatchString(raw) {
		return true, "unredacted Slack token detected (xox*-***)"
	}
	if huggingFaceRegex.MatchString(raw) {
		return true, "unredacted HuggingFace token detected (hf_***)"
	}
	if bearerTokenRegex.MatchString(raw) {
		return true, "unredacted Bearer token detected (Bearer ***)"
	}
	if urlBasicAuthRegex.MatchString(raw) {
		return true, "unredacted URL basic auth credentials detected (scheme://user:***@host)"
	}
	if querySecretRegex.MatchString(raw) {
		return true, "unredacted URL query secret parameter detected (?key=***)"
	}

	// 2. Check structured headers, tags, metadata, and maps for unmasked sensitive keys
	for k, v := range c.Metadata.Tags {
		lower := strings.ToLower(strings.TrimSpace(k))
		if (sensitiveKeyNames[lower] || sensitiveHeaderNames[lower]) && !isRedactedPlaceholder(v) {
			return true, fmt.Sprintf("unredacted sensitive metadata tag %q detected ([***])", k)
		}
	}

	for _, inter := range c.Interactions {
		for k, v := range inter.Request.Headers {
			if sensitiveHeaderNames[strings.ToLower(strings.TrimSpace(k))] && !isRedactedPlaceholder(v) {
				return true, fmt.Sprintf("unredacted sensitive request header %q detected ([***])", k)
			}
		}
		for k, v := range inter.Response.Headers {
			if sensitiveHeaderNames[strings.ToLower(strings.TrimSpace(k))] && !isRedactedPlaceholder(v) {
				return true, fmt.Sprintf("unredacted sensitive response header %q detected ([***])", k)
			}
		}
		for k, v := range inter.Metadata {
			lower := strings.ToLower(strings.TrimSpace(k))
			if (sensitiveKeyNames[lower] || sensitiveHeaderNames[lower]) && !isRedactedPlaceholder(v) {
				return true, fmt.Sprintf("unredacted sensitive interaction metadata %q detected ([***])", k)
			}
		}
		if keyName, found := findUnredactedMapKey(inter.Request.ToolInput); found {
			return true, fmt.Sprintf("unredacted sensitive tool_input field %q detected ([***])", keyName)
		}
		if m, ok := inter.Request.Body.(map[string]interface{}); ok {
			if keyName, found := findUnredactedMapKey(m); found {
				return true, fmt.Sprintf("unredacted sensitive request body field %q detected ([***])", keyName)
			}
		}
		if m, ok := inter.Response.Body.(map[string]interface{}); ok {
			if keyName, found := findUnredactedMapKey(m); found {
				return true, fmt.Sprintf("unredacted sensitive response body field %q detected ([***])", keyName)
			}
		}
	}

	return false, ""
}

func findUnredactedMapKey(m map[string]interface{}) (string, bool) {
	for k, v := range m {
		lower := strings.ToLower(strings.TrimSpace(k))
		if sensitiveKeyNames[lower] || sensitiveHeaderNames[lower] {
			switch s := v.(type) {
			case string:
				if !isRedactedPlaceholder(s) {
					return k, true
				}
			default:
				if v != nil {
					return k, true
				}
			}
			continue
		}
		if nested, ok := v.(map[string]interface{}); ok {
			if nk, found := findUnredactedMapKey(nested); found {
				return nk, true
			}
		}
		if slice, ok := v.([]interface{}); ok {
			for _, item := range slice {
				if nested, ok := item.(map[string]interface{}); ok {
					if nk, found := findUnredactedMapKey(nested); found {
						return nk, true
					}
				}
			}
		}
	}
	return "", false
}
