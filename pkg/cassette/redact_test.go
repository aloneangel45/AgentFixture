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
			name:     "AWS Access Key",
			input:    "Credentials AKIAIOSFODNN7EXAMPLE",
			expected: "Credentials [REDACTED_AWS_KEY]",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactString(tc.input)
			if got != tc.expected {
				t.Errorf("expected %q, got %q", tc.expected, got)
			}
		})
	}
}

func TestRedaction_Headers(t *testing.T) {
	headers := map[string]string{
		"Authorization":     "Bearer sk-ant-secret1234567890",
		"X-Api-Key":         "super_secret_token",
		"Content-Type":      "application/json",
		"Anthropic-Version": "2023-06-01",
	}

	redacted := RedactHeaders(headers)
	if redacted["Authorization"] != "[REDACTED]" {
		t.Errorf("expected Authorization to be [REDACTED], got %q", redacted["Authorization"])
	}
	if redacted["X-Api-Key"] != "[REDACTED]" {
		t.Errorf("expected X-Api-Key to be [REDACTED], got %q", redacted["X-Api-Key"])
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

func TestCheckForUnredactedSecrets(t *testing.T) {
	c := NewCassette("test-cas")
	c.Interactions = append(c.Interactions, Interaction{
		ID:       "int_001",
		Sequence: 1,
		Type:     InteractionTypeLLMChat,
		Request: Request{
			RawBody: "Using raw token sk-ant-1234567890123456789012345",
		},
	})

	hasSecret, reason := CheckForUnredactedSecrets(c)
	if !hasSecret {
		t.Fatalf("expected unredacted secret to be detected")
	}
	if !strings.Contains(reason, "Anthropic") {
		t.Errorf("expected reason to mention Anthropic, got %s", reason)
	}

	RedactCassette(c)
	hasSecretAfter, _ := CheckForUnredactedSecrets(c)
	if hasSecretAfter {
		t.Fatalf("expected cassette to be clean after RedactCassette")
	}
}
