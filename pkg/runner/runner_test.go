package runner

import (
	"strings"
	"testing"
	"time"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
	"github.com/aloneangel45/AgentFixture/pkg/matcher"
)

func TestRunner_BasicExecution(t *testing.T) {
	cas := cassette.NewCassette("cas_runner_test")
	cas.Interactions = append(cas.Interactions, cassette.Interaction{
		ID:       "int_001",
		Sequence: 1,
		Type:     cassette.InteractionTypeHTTP,
		Request: cassette.Request{
			Method:   "GET",
			Endpoint: "/status",
		},
		Response: cassette.Response{
			StatusCode: 200,
			RawBody:    "OK",
			LatencyMs:  10,
		},
	})

	report, err := Run(RunOptions{
		Cassette: cas,
		Matcher:  matcher.NewStrictMatcher(),
		Command:  []string{"go", "version"},
		Timeout:  10 * time.Second,
	})

	if err != nil {
		t.Fatalf("unexpected error running regression runner: %v", err)
	}

	if !report.Passed {
		t.Errorf("expected report to pass for valid command, got failed")
	}
	if report.CommandExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", report.CommandExitCode)
	}
	if !strings.Contains(report.CommandOutput, "go version") {
		t.Errorf("expected command output to contain go version, got %q", report.CommandOutput)
	}

	summary := report.FormatSummary()
	if !strings.Contains(summary, "PASSED") {
		t.Errorf("expected summary to contain PASSED, got: %s", summary)
	}
}

func TestTestReport_FormatSummary_RegressionDetected(t *testing.T) {
	report := &TestReport{
		CassetteID:        "cas_failed_run",
		TotalExpected:     5,
		MatchedCount:      3,
		UnmatchedExpected: []string{"int_004", "int_005"},
		UnexpectedCalls: []cassette.Request{
			{
				Method:   "POST",
				Endpoint: "/v1/unexpected",
			},
		},
		CommandExitCode: 1,
		Passed:          false,
	}

	summary := report.FormatSummary()
	if !strings.Contains(summary, "FAILED (REGRESSION DETECTED)") {
		t.Errorf("expected failure banner in summary, got: %s", summary)
	}
	if !strings.Contains(summary, "int_004") {
		t.Errorf("expected missing expected interaction listed, got: %s", summary)
	}
	if !strings.Contains(summary, "/v1/unexpected") {
		t.Errorf("expected unexpected call listed, got: %s", summary)
	}
}
