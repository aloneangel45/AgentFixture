package runner

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
	"github.com/aloneangel45/AgentFixture/pkg/matcher"
	"github.com/aloneangel45/AgentFixture/pkg/proxy"
)

// DriftInfo records details of semantic drift detected during test execution.
type DriftInfo struct {
	InteractionID string  `json:"interaction_id"`
	ToolOrPath    string  `json:"tool_or_path"`
	Similarity    float64 `json:"similarity"`
	Reason        string  `json:"reason"`
}

// TestReport captures the full assertion results of a regression run.
type TestReport struct {
	CassetteID        string             `json:"cassette_id"`
	TotalExpected     int                `json:"total_expected"`
	MatchedCount      int                `json:"matched_count"`
	UnmatchedExpected []string           `json:"unmatched_expected,omitempty"`
	UnexpectedCalls   []cassette.Request `json:"unexpected_calls,omitempty"`
	DriftDetails      []DriftInfo        `json:"drift_details,omitempty"`
	CommandExitCode   int                `json:"command_exit_code"`
	CommandOutput     string             `json:"command_output,omitempty"`
	TokensSaved       int64              `json:"tokens_saved"`
	CostSavedUSD      float64            `json:"cost_saved_usd"`
	Duration          time.Duration      `json:"duration"`
	Passed            bool               `json:"passed"`
}

// FormatSummary renders a human-readable CLI summary table of the regression report.
func (r *TestReport) FormatSummary() string {
	var sb strings.Builder
	sb.WriteString("=================================================================\n")
	sb.WriteString(fmt.Sprintf(" AgentFixture Regression Report: %s\n", r.CassetteID))
	sb.WriteString("=================================================================\n")

	status := "PASSED"
	if !r.Passed {
		status = "FAILED (REGRESSION DETECTED)"
	}
	sb.WriteString(fmt.Sprintf(" Status:              %s\n", status))
	sb.WriteString(fmt.Sprintf(" Exit Code:           %d\n", r.CommandExitCode))
	sb.WriteString(fmt.Sprintf(" Interactions Replayed: %d / %d (%.1f%%)\n",
		r.MatchedCount, r.TotalExpected, float64(r.MatchedCount)/float64(max(1, r.TotalExpected))*100))
	sb.WriteString(fmt.Sprintf(" Estimated Tokens Saved: %d\n", r.TokensSaved))
	sb.WriteString(fmt.Sprintf(" Estimated Cost Saved:   $%.4f USD\n", r.CostSavedUSD))
	sb.WriteString(fmt.Sprintf(" Replay Run Duration:    %v\n", r.Duration))

	if len(r.UnmatchedExpected) > 0 {
		sb.WriteString("\n [!] Missing Expected Interactions (Cassette items not called):\n")
		for _, id := range r.UnmatchedExpected {
			sb.WriteString(fmt.Sprintf("     - %s\n", id))
		}
	}

	if len(r.UnexpectedCalls) > 0 {
		sb.WriteString(fmt.Sprintf("\n [!] Unexpected Calls (%d unmatched requests):\n", len(r.UnexpectedCalls)))
		for i, req := range r.UnexpectedCalls {
			sb.WriteString(fmt.Sprintf("     %d. %s %s %s\n", i+1, req.Method, req.Endpoint, req.ToolName))
		}
	}

	if len(r.DriftDetails) > 0 {
		sb.WriteString("\n [!] Semantic Drift Warnings:\n")
		for _, d := range r.DriftDetails {
			sb.WriteString(fmt.Sprintf("     - %s (%s, sim: %.2f): %s\n", d.InteractionID, d.ToolOrPath, d.Similarity, d.Reason))
		}
	}

	sb.WriteString("=================================================================\n")
	return sb.String()
}

// RunOptions configures the regression test execution.
type RunOptions struct {
	Cassette   *cassette.Cassette
	Matcher    matcher.Matcher
	Command    []string
	Env        map[string]string
	StrictMode bool
	Timeout    time.Duration
}

// Run executes the command against a deterministic replay proxy and asserts cassette expectations.
func Run(opts RunOptions) (*TestReport, error) {
	if opts.Cassette == nil {
		return nil, fmt.Errorf("cassette cannot be nil")
	}
	if len(opts.Command) == 0 {
		return nil, fmt.Errorf("command cannot be empty")
	}
	if opts.Matcher == nil {
		opts.Matcher = matcher.NewStrictMatcher()
	}
	if opts.Timeout == 0 {
		opts.Timeout = 60 * time.Second
	}

	// 1. Initialize deterministic replay proxy
	srv, err := proxy.NewServer(proxy.Config{
		Mode:          proxy.ModeReplay,
		Cassette:      opts.Cassette,
		Matcher:       opts.Matcher,
		SimulateDelay: false, // 0 delay for instant CI runs
	})
	if err != nil {
		return nil, fmt.Errorf("failed to start replay server: %w", err)
	}
	if err := srv.Start(); err != nil {
		return nil, fmt.Errorf("failed to bind replay server: %w", err)
	}
	defer srv.Close()

	// 2. Prepare environment variables
	proxyURL := srv.URL()
	envVars := os.Environ()
	envVars = append(envVars,
		"AGENTFIXTURE_PROXY_URL="+proxyURL,
		"ANTHROPIC_BASE_URL="+proxyURL,
		"OPENAI_BASE_URL="+proxyURL,
		"HTTP_PROXY="+proxyURL,
		"http_proxy="+proxyURL,
	)
	for k, v := range opts.Env {
		envVars = append(envVars, fmt.Sprintf("%s=%s", k, v))
	}

	// 3. Execute target command
	startTime := time.Now()
	cmd := exec.Command(opts.Command[0], opts.Command[1:]...)
	cmd.Env = envVars

	var outputBuf bytes.Buffer
	cmd.Stdout = &outputBuf
	cmd.Stderr = &outputBuf

	runErr := cmd.Run()
	duration := time.Since(startTime)

	exitCode := 0
	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	// 4. Collect assertions
	cas := opts.Cassette
	cas.ComputeMetrics()

	report := &TestReport{
		CassetteID:      cas.Metadata.CassetteID,
		TotalExpected:   len(cas.Interactions),
		MatchedCount:    srv.MatchedCount(),
		CommandExitCode: exitCode,
		CommandOutput:   outputBuf.String(),
		TokensSaved:     cas.Metadata.Metrics.EstimatedTokensSaved,
		CostSavedUSD:    cas.Metadata.Metrics.EstimatedCostSavedUSD,
		Duration:        duration,
		Passed:          exitCode == 0,
	}

	// Check for unexpected calls
	unmatchedCalls := srv.UnmatchedRequests()
	if len(unmatchedCalls) > 0 {
		report.UnexpectedCalls = unmatchedCalls
		report.Passed = false
	}

	// In strict mode, verify all cassette interactions were exercised
	if opts.StrictMode && report.MatchedCount < report.TotalExpected {
		report.Passed = false
	}

	return report, nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
