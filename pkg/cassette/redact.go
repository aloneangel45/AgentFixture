package cassette

import (
	"encoding/json"
	"regexp"
	"strings"
)

var (
	anthropicKeyRegex = regexp.MustCompile(`sk-ant-[a-zA-Z0-9_-]{20,}`)
	openaiProjRegex   = regexp.MustCompile(`sk-proj-[a-zA-Z0-9_-]{20,}`)
	openaiKeyRegex    = regexp.MustCompile(`sk-[a-zA-Z0-9_-]{24,}`)
	githubPatRegex    = regexp.MustCompile(`github_pat_[a-zA-Z0-9_]{50,}`)
	githubGhpRegex    = regexp.MustCompile(`ghp_[a-zA-Z0-9]{36}`)
	bearerTokenRegex  = regexp.MustCompile(`(?i)\bBearer\s+[a-zA-Z0-9_\-\.~+/]+=*`)
	awsAccessKeyRegex = regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)
)

var sensitiveHeaderNames = map[string]bool{
	"authorization":     true,
	"x-api-key":         true,
	"anthropic-api-key": true,
	"openai-api-key":    true,
	"x-auth-token":      true,
	"proxy-authorization": true,
}

var sensitiveKeyNames = map[string]bool{
	"password":      true,
	"secret":        true,
	"api_key":       true,
	"apikey":        true,
	"access_token":  true,
	"private_key":   true,
	"token":         true,
	"client_secret": true,
}

// RedactString sanitizes known API keys, tokens, and credentials from arbitrary text.
func RedactString(input string) string {
	if input == "" {
		return ""
	}
	s := bearerTokenRegex.ReplaceAllString(input, "Bearer [REDACTED_TOKEN]")
	s = anthropicKeyRegex.ReplaceAllString(s, "[REDACTED_ANTHROPIC_KEY]")
	s = openaiProjRegex.ReplaceAllString(s, "[REDACTED_OPENAI_KEY]")
	s = openaiKeyRegex.ReplaceAllString(s, "[REDACTED_OPENAI_KEY]")
	s = githubPatRegex.ReplaceAllString(s, "[REDACTED_GITHUB_TOKEN]")
	s = githubGhpRegex.ReplaceAllString(s, "[REDACTED_GITHUB_TOKEN]")
	s = awsAccessKeyRegex.ReplaceAllString(s, "[REDACTED_AWS_KEY]")
	return s
}

// RedactHeaders masks any known sensitive authentication headers.
func RedactHeaders(headers map[string]string) map[string]string {
	if headers == nil {
		return nil
	}
	redacted := make(map[string]string, len(headers))
	for k, v := range headers {
		lower := strings.ToLower(k)
		if sensitiveHeaderNames[lower] {
			redacted[k] = "[REDACTED]"
		} else {
			redacted[k] = RedactString(v)
		}
	}
	return redacted
}

// RedactMap recursively scrubs sensitive fields and masks known patterns in map values.
func RedactMap(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}
	res := make(map[string]interface{}, len(m))
	for k, v := range m {
		lower := strings.ToLower(k)
		if sensitiveKeyNames[lower] {
			res[k] = "[REDACTED]"
			continue
		}
		res[k] = redactValue(v)
	}
	return res
}

func redactValue(val interface{}) interface{} {
	switch v := val.(type) {
	case string:
		return RedactString(v)
	case map[string]interface{}:
		return RedactMap(v)
	case []interface{}:
		items := make([]interface{}, len(v))
		for i, el := range v {
			items[i] = redactValue(el)
		}
		return items
	default:
		return val
	}
}

// RedactInteraction applies secret scrubbing to request and response fields.
func RedactInteraction(inter *Interaction) {
	inter.Request.Headers = RedactHeaders(inter.Request.Headers)
	inter.Request.RawBody = RedactString(inter.Request.RawBody)
	if inter.Request.Body != nil {
		inter.Request.Body = redactValue(inter.Request.Body)
	}
	if inter.Request.ToolInput != nil {
		inter.Request.ToolInput = RedactMap(inter.Request.ToolInput)
	}
	inter.Request.Matcher.FuzzySignature.NormalizedPrompt = RedactString(inter.Request.Matcher.FuzzySignature.NormalizedPrompt)
	if inter.Request.Matcher.FuzzySignature.NormalizedArgs != nil {
		inter.Request.Matcher.FuzzySignature.NormalizedArgs = RedactMap(inter.Request.Matcher.FuzzySignature.NormalizedArgs)
	}

	inter.Response.Headers = RedactHeaders(inter.Response.Headers)
	inter.Response.RawBody = RedactString(inter.Response.RawBody)
	inter.Response.ToolResult = RedactString(inter.Response.ToolResult)
	inter.Response.Error = RedactString(inter.Response.Error)
	if inter.Response.Body != nil {
		inter.Response.Body = redactValue(inter.Response.Body)
	}
}

// RedactCassette scrubs all interactions and metadata within a Cassette.
func RedactCassette(c *Cassette) {
	c.Metadata.Description = RedactString(c.Metadata.Description)
	for i := range c.Interactions {
		RedactInteraction(&c.Interactions[i])
	}
}

// CheckForUnredactedSecrets inspects a cassette and returns true if raw secrets remain.
func CheckForUnredactedSecrets(c *Cassette) (bool, string) {
	data, err := json.Marshal(c)
	if err != nil {
		return false, ""
	}
	raw := string(data)

	if loc := anthropicKeyRegex.FindString(raw); loc != "" {
		return true, "unredacted Anthropic key detected: " + loc[:12] + "..."
	}
	if loc := openaiProjRegex.FindString(raw); loc != "" {
		return true, "unredacted OpenAI project key detected: " + loc[:12] + "..."
	}
	if loc := openaiKeyRegex.FindString(raw); loc != "" {
		return true, "unredacted OpenAI key detected: " + loc[:8] + "..."
	}
	if loc := githubPatRegex.FindString(raw); loc != "" {
		return true, "unredacted GitHub PAT detected: " + loc[:15] + "..."
	}
	if loc := githubGhpRegex.FindString(raw); loc != "" {
		return true, "unredacted GitHub personal token detected: " + loc[:8] + "..."
	}
	if loc := awsAccessKeyRegex.FindString(raw); loc != "" {
		return true, "unredacted AWS key detected: " + loc[:8] + "..."
	}
	return false, ""
}
