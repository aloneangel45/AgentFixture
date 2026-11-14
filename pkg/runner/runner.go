package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
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

// TestReport captures the full assertion results of a regression or recording run.
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
	sb.WriteString(fmt.Sprintf(" Status:                 %s\n", status))
	sb.WriteString(fmt.Sprintf(" Exit Code:              %d\n", r.CommandExitCode))
	sb.WriteString(fmt.Sprintf(" Interactions Replayed:  %d / %d (%.1f%%)\n",
		r.MatchedCount, r.TotalExpected, float64(r.MatchedCount)/float64(max(1, r.TotalExpected))*100))
	sb.WriteString(fmt.Sprintf(" Estimated Tokens Saved: %d\n", r.TokensSaved))
	sb.WriteString(fmt.Sprintf(" Estimated Cost Saved:   $%.4f USD\n", r.CostSavedUSD))
	sb.WriteString(fmt.Sprintf(" Replay Run Duration:    %v\n", r.Duration))

	if len(r.UnmatchedExpected) > 0 {
		sb.WriteString("\n [WARN] Missing Expected Interactions (Cassette items not called):\n")
		for _, id := range r.UnmatchedExpected {
			sb.WriteString(fmt.Sprintf("     - %s\n", id))
		}
	}

	if len(r.UnexpectedCalls) > 0 {
		sb.WriteString(fmt.Sprintf("\n [FAIL] Unexpected Calls (%d unmatched requests):\n", len(r.UnexpectedCalls)))
		for i, req := range r.UnexpectedCalls {
			sb.WriteString(fmt.Sprintf("     %d. %s %s %s\n", i+1, req.Method, req.Endpoint, req.ToolName))
		}
	}

	if len(r.DriftDetails) > 0 {
		sb.WriteString("\n [WARN] Semantic Drift Warnings:\n")
		for _, d := range r.DriftDetails {
			sb.WriteString(fmt.Sprintf("     - %s (%s, sim: %.2f): %s\n", d.InteractionID, d.ToolOrPath, d.Similarity, d.Reason))
		}
	}

	sb.WriteString("=================================================================\n")
	return sb.String()
}

// RunOptions configures the regression or recording command execution.
type RunOptions struct {
	Cassette    *cassette.Cassette
	Matcher     matcher.Matcher
	Command     []string
	Env         map[string]string
	StrictMode  bool
	Timeout     time.Duration
	Mode        proxy.Mode
	UpstreamURL string
	Server      *proxy.Server
	SSETiming   cassette.SSETimingConfig
}

// Run executes the target command against a deterministic replay or live recording proxy
// and asserts cassette expectations.
func Run(opts RunOptions) (*TestReport, error) {
	if opts.Cassette == nil && opts.Server == nil {
		return nil, fmt.Errorf("cassette cannot be nil")
	}
	if len(opts.Command) == 0 {
		return nil, fmt.Errorf("command cannot be empty")
	}
	if opts.Matcher == nil {
		opts.Matcher = matcher.NewStrictMatcher()
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 60 * time.Second
	}

	mode := opts.Mode
	if mode == "" {
		if opts.Server != nil {
			mode = opts.Server.Mode()
		} else {
			mode = proxy.ModeReplay
		}
	}

	// 1. Acquire or start proxy server
	srv := opts.Server
	ownsServer := false
	if srv == nil {
		var err error
		srv, err = proxy.NewServer(proxy.Config{
			Mode:          mode,
			UpstreamURL:   opts.UpstreamURL,
			Cassette:      opts.Cassette,
			Matcher:       opts.Matcher,
			SimulateDelay: false,
			SSETiming:     opts.SSETiming,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create %s server: %w", mode, err)
		}
		if err := srv.Start(); err != nil {
			return nil, fmt.Errorf("failed to bind %s server: %w", mode, err)
		}
		ownsServer = true
	} else if srv.Addr() == "" {
		if err := srv.Start(); err != nil {
			return nil, fmt.Errorf("failed to bind provided %s server: %w", mode, err)
		}
		ownsServer = true
	}
	if ownsServer {
		defer srv.Close()
	}

	// 2. Prepare deduplicated environment variables (case-insensitive on Windows!)
	proxyURL := srv.URL()
	overrides := map[string]string{
		"AGENTFIXTURE_PROXY_URL": proxyURL,
		"ANTHROPIC_BASE_URL":     proxyURL,
		"OPENAI_BASE_URL":        proxyURL,
		"GOOGLE_GEMINI_BASE_URL": proxyURL,
		"GEMINI_BASE_URL":        proxyURL,
		"HTTP_PROXY":             proxyURL,
		"NO_PROXY":               "127.0.0.1,localhost,::1",
	}
	if runtime.GOOS != "windows" {
		overrides["http_proxy"] = proxyURL
		overrides["no_proxy"] = "127.0.0.1,localhost,::1"
	}
	for k, v := range opts.Env {
		overrides[k] = v
	}

	var fallbacks map[string]string
	if mode == proxy.ModeReplay {
		// Provide dummy SDK keys in replay mode when unset so agent SDKs initialize cleanly in zero-secret CI.
		fallbacks = map[string]string{
			"ANTHROPIC_API_KEY": "af-replay-offline-key",
			"OPENAI_API_KEY":    "af-replay-offline-key",
			"GEMINI_API_KEY":    "AIzaSyAgentFixtureOfflineReplayKey00000",
			"GOOGLE_API_KEY":    "AIzaSyAgentFixtureOfflineReplayKey00000",
		}
	}

	envVars := buildChildEnv(os.Environ(), overrides, fallbacks, runtime.GOOS == "windows")

	// 3. Execute target command with context timeout
	ctx, cancel := context.WithTimeout(context.Background(), opts.Timeout)
	defer cancel()

	startTime := time.Now()
	cmd := exec.CommandContext(ctx, opts.Command[0], opts.Command[1:]...)
	cmd.Env = envVars

	var outputBuf bytes.Buffer
	cmd.Stdout = &outputBuf
	cmd.Stderr = &outputBuf

	runErr := cmd.Run()
	duration := time.Since(startTime)

	exitCode := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			exitCode = 124
		} else if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	// 4. Drain any in-flight proxy handler goroutines before reading cassette state
	srv.WaitIdle()
	activeCas := srv.GetCassette()
	activeCas.ComputeMetrics()

	if mode == proxy.ModeRecord {
		if opts.Cassette != nil {
			*opts.Cassette = *activeCas
		}
		return &TestReport{
			CassetteID:      activeCas.Metadata.CassetteID,
			TotalExpected:   len(activeCas.Interactions),
			MatchedCount:    len(activeCas.Interactions),
			CommandExitCode: exitCode,
			CommandOutput:   outputBuf.String(),
			TokensSaved:     activeCas.Metadata.Metrics.EstimatedTokensSaved,
			CostSavedUSD:    activeCas.Metadata.Metrics.EstimatedCostSavedUSD,
			Duration:        duration,
			Passed:          exitCode == 0,
		}, nil
	}

	matchedIDs := srv.MatchedIDs()
	var unmatchedExpected []string
	for _, inter := range activeCas.Interactions {
		if !matchedIDs[inter.ID] {
			unmatchedExpected = append(unmatchedExpected, inter.ID)
		}
	}

	rawDrifts := srv.DriftRecords()
	var driftDetails []DriftInfo
	for _, d := range rawDrifts {
		driftDetails = append(driftDetails, DriftInfo{
			InteractionID: d.InteractionID,
			ToolOrPath:    d.ToolOrPath,
			Similarity:    d.Similarity,
			Reason:        d.Reason,
		})
	}

	report := &TestReport{
		CassetteID:        activeCas.Metadata.CassetteID,
		TotalExpected:     len(activeCas.Interactions),
		MatchedCount:      srv.MatchedCount(),
		UnmatchedExpected: unmatchedExpected,
		DriftDetails:      driftDetails,
		CommandExitCode:   exitCode,
		CommandOutput:     outputBuf.String(),
		TokensSaved:       activeCas.Metadata.Metrics.EstimatedTokensSaved,
		CostSavedUSD:      activeCas.Metadata.Metrics.EstimatedCostSavedUSD,
		Duration:          duration,
		Passed:            exitCode == 0,
	}

	unmatchedCalls := srv.UnmatchedRequests()
	if len(unmatchedCalls) > 0 {
		report.UnexpectedCalls = unmatchedCalls
		report.Passed = false
	}

	if opts.StrictMode && report.MatchedCount < report.TotalExpected {
		report.Passed = false
	}

	return report, nil
}

type envEntry struct {
	key string
	val string
}

// buildChildEnv merges base OS environment variables with overrides and fallbacks,
// performing case-insensitive deduplication on Windows so duplicate keys (e.g. Path/PATH, http_proxy/HTTP_PROXY) never collide.
func buildChildEnv(base []string, overrides map[string]string, fallbacks map[string]string, caseInsensitive bool) []string {
	normKey := func(k string) string {
		if caseInsensitive {
			return strings.ToUpper(k)
		}
		return k
	}

	indexByNorm := make(map[string]int, len(base)+len(overrides)+len(fallbacks))
	entries := make([]envEntry, 0, len(base)+len(overrides)+len(fallbacks))

	setVar := func(k, v string) {
		nk := normKey(k)
		if idx, exists := indexByNorm[nk]; exists {
			entries[idx] = envEntry{key: k, val: v}
		} else {
			indexByNorm[nk] = len(entries)
			entries = append(entries, envEntry{key: k, val: v})
		}
	}

	for _, kv := range base {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			// Preserve special Windows drive vars like "=C:=C:\..."
			entries = append(entries, envEntry{key: "", val: kv})
			continue
		}
		setVar(kv[:eq], kv[eq+1:])
	}

	for k, v := range overrides {
		setVar(k, v)
	}

	for k, v := range fallbacks {
		nk := normKey(k)
		if idx, exists := indexByNorm[nk]; !exists || strings.TrimSpace(entries[idx].val) == "" {
			setVar(k, v)
		}
	}

	result := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.key == "" {
			result = append(result, e.val)
		} else {
			result = append(result, e.key+"="+e.val)
		}
	}
	return result
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
