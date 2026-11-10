package cassette

import (
	"strings"
	"testing"
)

func TestRedaction_APIKeys(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "Anthropic standard API key",
			input:    "Authorization: Bearer sk-ant-api03-abcdef1234567890abcdef1234567890",
			expected: "Authorization: Bearer [REDACTED_TOKEN]",
		},
		{
			name:     "Anthropic raw key in text",
			input:    "Connecting with sk-ant-abcdefghijklmnopqrstuvwxyz123456",
			expected: "Connecting with [REDACTED_ANTHROPIC_KEY]",
		},
		{
			name:     "OpenAI project key",
			input:    "Using key sk-proj-1234567890abcdef1234567890",
			expected: "Using key [REDACTED_OPENAI_KEY]",
		},
		{
			name:     "Google Gemini API key",
			input:    "Key AIzaSyDaGmWKa4JsXZ-HjGw7ISLn_3namBGewQe",
			expected: "Key [REDACTED_GOOGLE_KEY]",
		},
		{
			name:     "GitHub PAT",
			input:    "Token github_pat_11ABCD123456789012345678901234567890123456789012345678901234567890",
			expected: "Token [REDACTED_GITHUB_TOKEN]",
		},
		{
			name:     "GitHub ghp token",
			input:    "Token ghp_123456789012345678901234567890123456",
			expected: "Token [REDACTED_GITHUB_TOKEN]",
		},
		{
			name:     "GitHub gho OAuth token",
			input:    "Token gho_123456789012345678901234567890123456",
			expected: "Token [REDACTED_GITHUB_TOKEN]",
		},
		{
			name:     "AWS Access Key AKIA",
			input:    "Credentials AKIAIOSFODNN7EXAMPLE",
			expected: "Credentials [REDACTED_AWS_KEY]",
		},
		{
			name:     "AWS STS Key ASIA",
			input:    "Credentials ASIAIOSFODNN7EXAMPLE",
			expected: "Credentials [REDACTED_AWS_KEY]",
		},
		{
			name:     "Slack Bot Token",
			input:    "Slack " + strings.Join([]string{"xoxb", "123456789012", "1234567890123", "abcdefghijklmnopqrstuvwx"}, "-"),
			expected: "Slack [REDACTED_SLACK_TOKEN]",
		},
		{
			name:     "HuggingFace Token",
			input:    "HF " + strings.Join([]string{"hf", "abcdefghijklmnopqrstuvwxyzABCDEFGH"}, "_"),
			expected: "HF [REDACTED_HF_TOKEN]",
		},
		{
			name:     "URL Basic Auth Credentials",
			input:    "postgres://admin:superSecretPass99@db.internal:5432/prod",
			expected: "postgres://admin:[REDACTED]@db.internal:5432/prod",
		},
		{
			name:     "PEM Private Key",
			input:    "Key:\n-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA12345\n-----END RSA PRIVATE KEY-----",
			expected: "Key:\n[REDACTED_PRIVATE_KEY]",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactString(tc.input)
			if got != tc.expected {
				t.Errorf("expected %q, got %q", tc.expected, got)
			}
			// Verify idempotency
			gotAgain := RedactString(got)
			if gotAgain != got {
				t.Errorf("RedactString not idempotent: %q -> %q", got, gotAgain)
			}
		})
	}
}

func TestRedaction_Headers(t *testing.T) {
	headers := map[string]string{
		"Authorization":     "Bearer sk-ant-secret1234567890",
		"X-Api-Key":         "super_secret_token",
		"x-goog-api-key":    "AIzaSyDaGmWKa4JsXZ-HjGw7ISLn_3namBGewQe",
		"Cookie":            "session_id=xyz987secret",
		"Set-Cookie":        "auth=secret_cookie; HttpOnly",
		"Content-Type":      "application/json",
		"Anthropic-Version": "2023-06-01",
	}

	redacted := RedactHeaders(headers)
	for _, h := range []string{"Authorization", "X-Api-Key", "x-goog-api-key", "Cookie", "Set-Cookie"} {
		if redacted[h] != "[REDACTED]" {
			t.Errorf("expected %s to be [REDACTED], got %q", h, redacted[h])
		}
	}
	if redacted["Content-Type"] != "application/json" {
		t.Errorf("expected Content-Type to remain unmasked, got %q", redacted["Content-Type"])
	}
}

func TestRedaction_MapRecursion(t *testing.T) {
	m := map[string]interface{}{
		"username": "developer",
		"password": "unmasked_password_123",
		"nested": map[string]interface{}{
			"secret":  "nested_secret_xyz",
			"address": "https://api.anthropic.com?key=sk-ant-test1234567890123456",
		},
	}

	redacted := RedactMap(m)
	if redacted["password"] != "[REDACTED]" {
		t.Errorf("expected password to be [REDACTED], got %v", redacted["password"])
	}
	nested := redacted["nested"].(map[string]interface{})
	if nested["secret"] != "[REDACTED]" {
		t.Errorf("expected nested.secret to be [REDACTED], got %v", nested["secret"])
	}
	if !strings.Contains(nested["address"].(string), "[REDACTED_ANTHROPIC_KEY]") {
		t.Errorf("expected nested.address to redact Anthropic key, got %v", nested["address"])
	}
}

func TestCheckForUnredactedSecrets_MasksPreviewAndDetectsHeaders(t *testing.T) {
	secretSuffix := "UNIQUE_SECRET_SUFFIX_99887766"
	c := NewCassette("test-cas")
	c.Metadata.Tags["api_key"] = "raw_tag_secret"
	c.Interactions = append(c.Interactions, Interaction{
		ID:       "int_001",
		Sequence: 1,
		Type:     InteractionTypeLLMChat,
		Request: Request{
			Endpoint: "https://user:pass123@api.example.com/v1/chat?api_key=raw_q_key",
			Headers: map[string]string{
				"Cookie": "sid=unredacted_cookie",
			},
			RawBody: "Using raw token sk-ant-" + secretSuffix,
		},
		Metadata: map[string]string{
			"token": "raw_meta_token",
		},
	})

	hasSecret, reason := CheckForUnredactedSecrets(c)
	if !hasSecret {
		t.Fatalf("expected unredacted secret to be detected")
	}
	if !strings.Contains(reason, "Anthropic") {
		t.Errorf("expected reason to mention Anthropic, got %s", reason)
	}
	if strings.Contains(reason, "UNIQUE") || strings.Contains(reason, secretSuffix[:6]) {
		t.Fatalf("SECURITY LEAK: CheckForUnredactedSecrets leaked secret characters in error message: %s", reason)
	}

	// Also verify standalone Cookie / Bearer detection
	cHeader := NewCassette("test-cookie")
	cHeader.Interactions = append(cHeader.Interactions, Interaction{
		ID:       "int_cookie",
		Sequence: 1,
		Type:     InteractionTypeHTTP,
		Request: Request{
			Headers: map[string]string{"Cookie": "sess=raw_session_value"},
		},
	})
	if ok, r := CheckForUnredactedSecrets(cHeader); !ok {
		t.Fatalf("expected unredacted Cookie header to be flagged")
	} else if strings.Contains(r, "raw_session_value") {
		t.Fatalf("expected Cookie value masked in error reason: %s", r)
	}

	RedactCassette(c)
	hasSecretAfter, afterReason := CheckForUnredactedSecrets(c)
	if hasSecretAfter {
		t.Fatalf("expected cassette to be clean after RedactCassette, got: %s", afterReason)
	}
}
