package cassette_test

import (
	"strings"
	"testing"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
)

func TestAdversarial_SecretRedactor(t *testing.T) {
	fakeAnthropicKey := "sk-ant-api03-abcdef1234567890abcdef1234567890-XYZ"
	fakeOpenAIProjKey := "sk-proj-9876543210zyxwvutsrqponmlkjihgfedcba"
	fakeOpenAIStandardKey := "sk-1234567890abcdef1234567890abcdef"
	fakeBearerToken := "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.e30.t-IDcSemACt8x4iTMCda8Yhe3iZaWbvV5XKSTbuAn0M"
	fakePassword := "P@ssw0rd!#Adversarial_2026"

	// 1. Text redaction test
	corpus := strings.Join([]string{
		"Authorization: " + fakeBearerToken,
		"Anthropic key: " + fakeAnthropicKey,
		"OpenAI project: " + fakeOpenAIProjKey,
		"OpenAI standard: " + fakeOpenAIStandardKey,
		"URL query: https://api.anthropic.com/v1/messages?key=" + fakeAnthropicKey,
	}, "\n")

	redactedText := cassette.RedactString(corpus)

	if strings.Contains(redactedText, fakeAnthropicKey) {
		t.Fatalf("FAILED: unredacted Anthropic key leaked in RedactString: %s", redactedText)
	}
	if strings.Contains(redactedText, fakeOpenAIProjKey) {
		t.Fatalf("FAILED: unredacted OpenAI project key leaked in RedactString: %s", redactedText)
	}
	if strings.Contains(redactedText, fakeOpenAIStandardKey) {
		t.Fatalf("FAILED: unredacted OpenAI standard key leaked in RedactString: %s", redactedText)
	}
	if strings.Contains(redactedText, "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9") {
		t.Fatalf("FAILED: unredacted Bearer JWT leaked in RedactString: %s", redactedText)
	}

	// 2. Map & Password redaction test
	secretMap := map[string]interface{}{
		"username": "adversary",
		"password": fakePassword,
		"auth": map[string]interface{}{
			"secret":        "nested-secret-value-12345",
			"api_key":       fakeAnthropicKey,
			"access_token":  fakeBearerToken,
			"client_secret": "client-secret-9999",
			"public_info":   "safe-to-read",
		},
		"list_with_secrets": []interface{}{
			fakeOpenAIProjKey,
			map[string]interface{}{
				"token": "deeply-nested-token",
			},
		},
	}

	redactedMap := cassette.RedactMap(secretMap)

	if redactedMap["password"] != "[REDACTED]" {
		t.Errorf("expected password to be masked, got: %v", redactedMap["password"])
	}
	nestedAuth := redactedMap["auth"].(map[string]interface{})
	if nestedAuth["secret"] != "[REDACTED]" {
		t.Errorf("expected auth.secret to be masked, got: %v", nestedAuth["secret"])
	}
	if nestedAuth["api_key"] != "[REDACTED]" {
		t.Errorf("expected auth.api_key to be masked, got: %v", nestedAuth["api_key"])
	}
	if nestedAuth["client_secret"] != "[REDACTED]" {
		t.Errorf("expected auth.client_secret to be masked, got: %v", nestedAuth["client_secret"])
	}

	// 3. Full Cassette Redact and CheckForUnredactedSecrets Oracle
	cas := cassette.NewCassette("adversarial-secrets-cassette")
	cas.Interactions = append(cas.Interactions, cassette.Interaction{
		ID:       "int_adversarial_1",
		Sequence: 1,
		Type:     cassette.InteractionTypeLLMChat,
		Request: cassette.Request{
			Method:   "POST",
			Endpoint: "https://api.anthropic.com/v1/messages",
			Headers: map[string]string{
				"Authorization": fakeBearerToken,
				"X-Api-Key":     fakeAnthropicKey,
			},
			RawBody: `{"prompt":"Please execute using ` + fakeOpenAIProjKey + `"}`,
			ToolInput: map[string]interface{}{
				"password": fakePassword,
			},
		},
		Response: cassette.Response{
			StatusCode: 200,
			RawBody:    `{"result":"leaked key: ` + fakeAnthropicKey + `"}`,
		},
	})

	// Pre-redaction oracle check: MUST detect unredacted secrets
	hasSecretsPre, reasonPre := cassette.CheckForUnredactedSecrets(cas)
	if !hasSecretsPre {
		t.Fatalf("ORACLE FAILURE: CheckForUnredactedSecrets failed to detect raw secrets prior to redaction!")
	}
	t.Logf("Oracle correctly flagged pre-redaction secrets: %s", reasonPre)

	// Execute redaction
	cassette.RedactCassette(cas)

	// Post-redaction oracle check: MUST be 100% clean
	hasSecretsPost, reasonPost := cassette.CheckForUnredactedSecrets(cas)
	if hasSecretsPost {
		t.Fatalf("FAILED: CheckForUnredactedSecrets found remaining leak post-redaction: %s", reasonPost)
	}

	// Verify headers are masked
	if cas.Interactions[0].Request.Headers["Authorization"] != "[REDACTED]" {
		t.Errorf("Authorization header not masked: %v", cas.Interactions[0].Request.Headers["Authorization"])
	}
	if cas.Interactions[0].Request.Headers["X-Api-Key"] != "[REDACTED]" {
		t.Errorf("X-Api-Key header not masked: %v", cas.Interactions[0].Request.Headers["X-Api-Key"])
	}

	// Verify serialization to YAML and JSON also remains leak-free
	yamlBytes, err := cassette.ToYAML(cas)
	if err != nil {
		t.Fatalf("ToYAML failed: %v", err)
	}
	if strings.Contains(string(yamlBytes), fakeAnthropicKey) ||
		strings.Contains(string(yamlBytes), fakeOpenAIProjKey) ||
		strings.Contains(string(yamlBytes), fakePassword) {
		t.Fatalf("FAILED: Serialized YAML contains unredacted secrets!")
	}

	jsonBytes, err := cassette.ToJSON(cas)
	if err != nil {
		t.Fatalf("ToJSON failed: %v", err)
	}
	if strings.Contains(string(jsonBytes), fakeAnthropicKey) ||
		strings.Contains(string(jsonBytes), fakeOpenAIProjKey) ||
		strings.Contains(string(jsonBytes), fakePassword) {
		t.Fatalf("FAILED: Serialized JSON contains unredacted secrets!")
	}

	t.Logf("Adversarial Secret Redactor: 100%% redaction verified across strings, maps, headers, and serialized outputs.")
}

func TestAdversarial_MalformedYAML(t *testing.T) {
	malformedInputs := []struct {
		name string
		yaml string
	}{
		{
			name: "Tab indentation error",
			yaml: "version: agentfixture/v1\nmetadata:\n\tid: test\n",
		},
		{
			name: "Invalid syntax colon",
			yaml: "version: agentfixture/v1\nfoo:: bar\n",
		},
		{
			name: "Invalid type structure",
			yaml: "version: [1, 2, 3]\nmetadata: 12345\n",
		},
		{
			name: "Missing required version",
			yaml: "metadata:\n  id: test\ninteractions: []\n",
		},
		{
			name: "Empty document",
			yaml: "",
		},
		{
			name: "Huge garbage input",
			yaml: strings.Repeat("!#%@^&*()_+~`\x00\xff", 1000),
		},
	}

	for _, tc := range malformedInputs {
		t.Run(tc.name, func(t *testing.T) {
			cas, err := cassette.FromYAML([]byte(tc.yaml))
			if err == nil && cas != nil && cas.Validate() == nil {
				t.Fatalf("expected error for malformed YAML (%s), but succeeded", tc.name)
			}
			t.Logf("Clean rejection of malformed YAML [%s]: %v", tc.name, err)
		})
	}
}
