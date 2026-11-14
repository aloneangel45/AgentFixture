package junit

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
	"github.com/aloneangel45/AgentFixture/pkg/runner"
)

func TestBuildVerifySuites_MarshalAndControlCharSanitization(t *testing.T) {
	cases := []VerifyCaseInput{
		{
			Path:                  `fixtures\gemini_sse.yaml`,
			CassetteID:            "cas_gem_01",
			Valid:                 true,
			Interactions:          3,
			SSEStreams:            2,
			EstimatedTokensSaved:  1250,
			EstimatedCostSavedUSD: 0.0095,
			Duration:              4 * time.Millisecond,
		},
		{
			Path:      `fixtures\sub\leaky.json`,
			Valid:     false,
			Duration:  2 * time.Millisecond,
			ErrorType: "SecurityViolation",
			Error:     "security violation: \x1b[31munredacted Google API key detected (AIza***)\x00",
		},
	}

	suites := BuildVerifySuites("fixtures", cases, 1250, 0.0095, 6*time.Millisecond)
	if suites.Tests != 2 || suites.Failures != 1 {
		t.Fatalf("expected tests=2 failures=1, got tests=%d failures=%d", suites.Tests, suites.Failures)
	}

	outPath := filepath.Join(t.TempDir(), "reports", "verify-junit.xml")
	if err := WriteFile(outPath, suites); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if !strings.HasPrefix(string(raw), xml.Header) {
		t.Errorf("expected XML header declaration, got: %s", string(raw))
	}

	var parsed TestSuites
	if err := xml.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("generated JUnit XML failed standard xml.Unmarshal: %v\nXML:\n%s", err, string(raw))
	}
	if len(parsed.Suites) != 1 || len(parsed.Suites[0].TestCases) != 2 {
		t.Fatalf("unexpected parsed suite structure: %+v", parsed)
	}
	if parsed.Suites[0].TestCases[0].Name != "fixtures/gemini_sse.yaml" {
		t.Errorf("expected normalized slash path, got %q", parsed.Suites[0].TestCases[0].Name)
	}
	failCase := parsed.Suites[0].TestCases[1]
	if failCase.Failure == nil || failCase.Failure.Type != "SecurityViolation" {
		t.Fatalf("expected SecurityViolation failure node, got %+v", failCase.Failure)
	}
	if strings.ContainsRune(failCase.Failure.Message, '\x1b') || strings.ContainsRune(failCase.Failure.Message, '\x00') {
		t.Errorf("expected illegal XML 1.0 control bytes stripped, got %q", failCase.Failure.Message)
	}
}

func TestBuildReplaySuites_PassFailAndSkippedAssertions(t *testing.T) {
	cas := cassette.NewCassette("cas_replay_junit")
	cas.Interactions = []cassette.Interaction{
		{
			ID:       "int_001",
			Sequence: 1,
			Type:     cassette.InteractionTypeSSE,
			Request:  cassette.Request{Method: "POST", Endpoint: "/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse"},
		},
		{
			ID:       "int_002",
			Sequence: 2,
			Type:     cassette.InteractionTypeTool,
			Request:  cassette.Request{ToolName: "read_file"},
		},
	}

	// 1. Passing replay with semantic drift note and non-strict skipped interaction
	passRep := &runner.TestReport{
		CassetteID:        "cas_replay_junit",
		TotalExpected:     2,
		MatchedCount:      1,
		UnmatchedExpected: []string{"int_002"},
		DriftDetails: []runner.DriftInfo{
			{InteractionID: "int_001", ToolOrPath: "/v1beta/models/gemini-2.5-pro:streamGenerateContent", Similarity: 0.94, Reason: "fuzzy match"},
		},
		TokensSaved:  800,
		CostSavedUSD: 0.0052,
		Duration:     25 * time.Millisecond,
		Passed:       true,
	}
	passSuites := BuildReplaySuites(cas, "fixtures/gem.yaml", passRep)
	if passSuites.Failures != 0 || passSuites.Skipped != 1 {
		t.Fatalf("expected 0 failures and 1 skipped in non-strict pass, got failures=%d skipped=%d", passSuites.Failures, passSuites.Skipped)
	}
	if !strings.Contains(passSuites.Suites[0].TestCases[0].SystemOut, "similarity=0.94") {
		t.Errorf("expected drift telemetry in system-out, got %q", passSuites.Suites[0].TestCases[0].SystemOut)
	}

	// 2. Failing replay with strict unmatched interaction + unexpected call + command exit error
	failRep := &runner.TestReport{
		CassetteID:        "cas_replay_junit",
		TotalExpected:     2,
		MatchedCount:      1,
		UnmatchedExpected: []string{"int_002"},
		UnexpectedCalls: []cassette.Request{
			{Method: "POST", Endpoint: "/v1/chat/completions", RawBody: `{"model":"gpt-4o"}`},
		},
		CommandExitCode: 2,
		CommandOutput:   "FAIL: agent assertion failed",
		Duration:        30 * time.Millisecond,
		Passed:          false,
	}
	failSuites := BuildReplaySuites(cas, "fixtures/gem.yaml", failRep)
	if failSuites.Failures != 3 {
		t.Fatalf("expected 3 failures (1 unmatched + 1 unexpected + 1 command exit), got %d", failSuites.Failures)
	}
	data, err := Marshal(failSuites)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	var roundTrip TestSuites
	if err := xml.Unmarshal(data, &roundTrip); err != nil {
		t.Fatalf("round-trip XML unmarshal failed: %v", err)
	}
	if roundTrip.Failures != 3 || roundTrip.Tests != 4 {
		t.Errorf("expected tests=4 failures=3 after XML round-trip, got tests=%d failures=%d", roundTrip.Tests, roundTrip.Failures)
	}
}
