package runner

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
	"github.com/aloneangel45/AgentFixture/pkg/matcher"
	"github.com/aloneangel45/AgentFixture/pkg/proxy"
)

func TestRunner_BasicExecutionAndStrictUnmatched(t *testing.T) {
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
		t.Errorf("expected report to pass when StrictMode=false, got failed")
	}
	if len(report.UnmatchedExpected) != 1 || report.UnmatchedExpected[0] != "int_001" {
		t.Errorf("expected UnmatchedExpected=[int_001], got %+v", report.UnmatchedExpected)
	}
	if !strings.Contains(report.CommandOutput, "go version") {
		t.Errorf("expected command output to contain go version, got %q", report.CommandOutput)
	}

	// With StrictMode=true, unexercised int_001 fails report.Passed
	strictReport, err := Run(RunOptions{
		Cassette:   cas,
		Matcher:    matcher.NewStrictMatcher(),
		Command:    []string{"go", "version"},
		StrictMode: true,
		Timeout:    10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strictReport.Passed {
		t.Errorf("expected StrictMode=true to fail when cassette interaction is uncalled")
	}
}

func TestRunner_BuildChildEnv_CaseInsensitiveWindowsDeduplication(t *testing.T) {
	base := []string{
		"=C:=C:\\Work",
		"Path=C:\\Windows\\System32",
		"http_proxy=http://old-proxy:8080",
		"OPENAI_API_KEY=",
	}
	overrides := map[string]string{
		"HTTP_PROXY": "http://127.0.0.1:5555",
		"NO_PROXY":   "127.0.0.1,localhost",
	}
	fallbacks := map[string]string{
		"ANTHROPIC_API_KEY": "af-dummy",
		"OPENAI_API_KEY":    "af-dummy-openai",
	}

	merged := buildChildEnv(base, overrides, fallbacks, true)
	proxyCount := 0
	hasAnthropic := false
	hasOpenAI := false
	for _, kv := range merged {
		upper := strings.ToUpper(kv)
		if strings.HasPrefix(upper, "HTTP_PROXY=") {
			proxyCount++
			if kv != "HTTP_PROXY=http://127.0.0.1:5555" {
				t.Errorf("unexpected HTTP_PROXY entry: %s", kv)
			}
		}
		if kv == "ANTHROPIC_API_KEY=af-dummy" {
			hasAnthropic = true
		}
		if kv == "OPENAI_API_KEY=af-dummy-openai" {
			hasOpenAI = true
		}
	}
	if proxyCount != 1 {
		t.Errorf("expected case-insensitive deduplication of http_proxy/HTTP_PROXY to 1 entry, got %d (%v)", proxyCount, merged)
	}
	if !hasAnthropic || !hasOpenAI {
		t.Errorf("expected fallback dummy API keys to be populated when missing/empty: %v", merged)
	}
}

func TestRunner_RecordModeWithLiveServer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"recorded":true}`))
	}))
	defer upstream.Close()

	cas := cassette.NewCassette("rec_runner")
	srv, err := proxy.NewServer(proxy.Config{
		Mode:        proxy.ModeRecord,
		UpstreamURL: upstream.URL,
		Cassette:    cas,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	// Make a request during command execution via Go test helper or verify Server wiring
	resp, err := http.Get(srv.URL() + "/ping")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	rep, err := Run(RunOptions{
		Cassette: cas,
		Mode:     proxy.ModeRecord,
		Server:   srv,
		Command:  []string{"go", "version"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Passed || rep.MatchedCount != 1 || len(cas.Interactions) != 1 {
		t.Fatalf("expected recorded interaction preserved in ModeRecord runner, got report=%+v len=%d", rep, len(cas.Interactions))
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
		DriftDetails: []DriftInfo{
			{
				InteractionID: "int_001",
				ToolOrPath:    "/v1/messages",
				Similarity:    0.88,
				Reason:        "fuzzy match",
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
	if !strings.Contains(summary, "0.88") {
		t.Errorf("expected drift details listed, got: %s", summary)
	}
}

func TestRunner_TimeoutWithGrandchildHoldingPipesDoesNotHang(t *testing.T) {
	if os.Getenv("AGENTFIXTURE_RUNNER_HANG_ROLE") == "child" {
		// Spawn an external OS command (not runner.test.exe, which Windows locks in go-build temp dir)
		// that inherits stdout/stderr pipes and outlives the parent process when killed on timeout.
		var gc *exec.Cmd
		if runtime.GOOS == "windows" {
			gc = exec.Command("ping", "-n", "15", "127.0.0.1")
		} else {
			gc = exec.Command("sleep", "15")
		}
		gc.Stdout = os.Stdout
		gc.Stderr = os.Stderr
		if err := gc.Start(); err == nil && gc.Process != nil {
			if pidFile := os.Getenv("AGENTFIXTURE_GC_PID_FILE"); pidFile != "" {
				_ = os.WriteFile(pidFile, []byte(strconv.Itoa(gc.Process.Pid)), 0600)
			}
		}
		time.Sleep(15 * time.Second)
		os.Exit(0)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(t.TempDir(), "gc.pid")
	defer func() {
		if raw, rErr := os.ReadFile(pidFile); rErr == nil {
			if pid, pErr := strconv.Atoi(strings.TrimSpace(string(raw))); pErr == nil && pid > 0 {
				if proc, fErr := os.FindProcess(pid); fErr == nil {
					_ = proc.Kill()
				}
			}
		}
	}()

	start := time.Now()
	report, err := Run(RunOptions{
		Cassette: cassette.NewCassette("cas_timeout_waitdelay"),
		Command:  []string{exe, "-test.run=^TestRunner_TimeoutWithGrandchildHoldingPipesDoesNotHang$"},
		Env: map[string]string{
			"AGENTFIXTURE_RUNNER_HANG_ROLE": "child",
			"AGENTFIXTURE_GC_PID_FILE":      pidFile,
		},
		Timeout: 500 * time.Millisecond,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error from timed-out Run: %v", err)
	}
	if report.CommandExitCode != 124 || report.Passed {
		t.Fatalf("expected timeout exit code 124 and Passed=false, got code=%d passed=%v", report.CommandExitCode, report.Passed)
	}
	// WaitDelay is 3s + 200ms timeout -> must return ~3.2s, well before the 15s grandchild sleep!
	if elapsed > 8*time.Second {
		t.Fatalf("expected cmd.WaitDelay to prevent 15s grandchild pipe hang, but Run took %v", elapsed)
	}
}
