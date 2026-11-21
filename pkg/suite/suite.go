// Package suite implements a declarative, parallel regression test suite runner
// for AgentFixture cassettes, supporting HTTP/SSE replay, MCP stdio replay,
// chaos fault injection, token/cost/cache budget gates, and tool/turn assertions.
package suite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aloneangel45/AgentFixture/pkg/budget"
	"github.com/aloneangel45/AgentFixture/pkg/cassette"
	"github.com/aloneangel45/AgentFixture/pkg/chaos"
	"github.com/aloneangel45/AgentFixture/pkg/matcher"
	"github.com/aloneangel45/AgentFixture/pkg/proxy"
	"github.com/aloneangel45/AgentFixture/pkg/runner"
	"gopkg.in/yaml.v3"
)

// CurrentSuiteVersion is the supported declarative suite manifest schema version.
const CurrentSuiteVersion = "agentfixture/suite/v1"

// SuiteSpec defines a declarative YAML test suite manifest.
type SuiteSpec struct {
	Version  string       `json:"version" yaml:"version"`
	Name     string       `json:"name,omitempty" yaml:"name,omitempty"`
	Defaults CaseDefaults `json:"defaults,omitempty" yaml:"defaults,omitempty"`
	Cases    []CaseSpec   `json:"cases" yaml:"cases"`
}

// CaseDefaults specifies shared default settings inherited by all cases in a suite.
type CaseDefaults struct {
	Matcher     string            `json:"matcher,omitempty" yaml:"matcher,omitempty"`
	Threshold   float64           `json:"threshold,omitempty" yaml:"threshold,omitempty"`
	Timing      string            `json:"timing,omitempty" yaml:"timing,omitempty"`
	Strict      *bool             `json:"strict,omitempty" yaml:"strict,omitempty"`
	Timeout     string            `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	PricingFile string            `json:"pricing_file,omitempty" yaml:"pricing_file,omitempty"`
	Budget      budget.Thresholds `json:"budget,omitempty" yaml:"budget,omitempty"`
}

// AssertionsSpec defines semantic tool, turn, and exit-code assertions for a suite case.
type AssertionsSpec struct {
	ExpectedTools  []string `json:"expected_tools,omitempty" yaml:"expected_tools,omitempty"`
	ForbiddenTools []string `json:"forbidden_tools,omitempty" yaml:"forbidden_tools,omitempty"`
	MaxTurns       int      `json:"max_turns,omitempty" yaml:"max_turns,omitempty"`
	ExpectExitCode *int     `json:"expect_exit_code,omitempty" yaml:"expect_exit_code,omitempty"`
}

// CaseSpec defines a single regression test case inside a SuiteSpec.
type CaseSpec struct {
	Name       string            `json:"name" yaml:"name"`
	Cassette   string            `json:"cassette" yaml:"cassette"`
	Mode       string            `json:"mode,omitempty" yaml:"mode,omitempty"`
	Command    string            `json:"command,omitempty" yaml:"command,omitempty"`
	Args       []string          `json:"args,omitempty" yaml:"args,omitempty"`
	Env        map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	Matcher    string            `json:"matcher,omitempty" yaml:"matcher,omitempty"`
	Threshold  float64           `json:"threshold,omitempty" yaml:"threshold,omitempty"`
	Timing     string            `json:"timing,omitempty" yaml:"timing,omitempty"`
	Strict     *bool             `json:"strict,omitempty" yaml:"strict,omitempty"`
	Timeout    string            `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	Chaos      chaos.Config      `json:"chaos,omitempty" yaml:"chaos,omitempty"`
	Budget     budget.Thresholds `json:"budget,omitempty" yaml:"budget,omitempty"`
	Assertions AssertionsSpec    `json:"assertions,omitempty" yaml:"assertions,omitempty"`
}

// RunOptions configures parallel suite execution.
type RunOptions struct {
	Concurrency   int
	BaseDir       string
	Source        string
	PricingTable  *budget.PricingTable
	DefaultBudget budget.Thresholds
}

// CaseResult captures the full execution, assertion, chaos, and budget outcome of one case.
type CaseResult struct {
	Name          string        `json:"name"`
	Cassette      string        `json:"cassette"`
	Mode          string        `json:"mode"`
	Passed        bool          `json:"passed"`
	DurationMs    int64         `json:"duration_ms"`
	Duration      time.Duration `json:"-"`
	Interactions  int           `json:"interactions"`
	MatchedCount  int           `json:"matched_count"`
	Turns         int           `json:"turns"`
	ExitCode      int           `json:"exit_code"`
	ObservedTools []string      `json:"observed_tools,omitempty"`
	Budget        budget.Report `json:"budget"`
	ChaosStats    chaos.Stats   `json:"chaos_stats,omitempty"`
	Failures      []string      `json:"failures,omitempty"`
	Error         string        `json:"error,omitempty"`
}

// SuiteReport aggregates results across all cases in a suite run.
type SuiteReport struct {
	Name        string        `json:"name"`
	Source      string        `json:"source"`
	Passed      bool          `json:"passed"`
	TotalCases  int           `json:"total_cases"`
	PassedCases int           `json:"passed_cases"`
	FailedCases int           `json:"failed_cases"`
	Concurrency int           `json:"concurrency"`
	DurationMs  int64         `json:"duration_ms"`
	Duration    time.Duration `json:"-"`
	Budget      budget.Report `json:"budget"`
	Cases       []CaseResult  `json:"cases"`
}

// LoadSuiteFile parses and validates a declarative YAML/JSON suite manifest.
func LoadSuiteFile(path string) (*SuiteSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read suite file %q: %w", path, err)
	}
	var spec SuiteSpec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("failed to parse suite file %q: %w", path, err)
	}
	if err := spec.Validate(); err != nil {
		return nil, fmt.Errorf("invalid suite manifest %q: %w", path, err)
	}
	if spec.Name == "" {
		spec.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	return &spec, nil
}

// Validate verifies required fields and bounds on a SuiteSpec.
func (s *SuiteSpec) Validate() error {
	if strings.TrimSpace(s.Version) != CurrentSuiteVersion {
		return fmt.Errorf("unsupported suite version %q (expected %q)", s.Version, CurrentSuiteVersion)
	}
	if len(s.Cases) == 0 {
		return errors.New("suite must define at least one case")
	}
	if err := s.Defaults.Budget.Validate(); err != nil {
		return fmt.Errorf("defaults.budget: %w", err)
	}
	seenNames := make(map[string]bool, len(s.Cases))
	for i, c := range s.Cases {
		name := strings.TrimSpace(c.Name)
		if name == "" {
			return fmt.Errorf("case[%d] missing required 'name'", i)
		}
		if seenNames[name] {
			return fmt.Errorf("duplicate case name %q", name)
		}
		seenNames[name] = true
		if strings.TrimSpace(c.Cassette) == "" {
			return fmt.Errorf("case %q missing required 'cassette'", name)
		}
		mode := strings.ToLower(strings.TrimSpace(c.Mode))
		if mode != "" && mode != "http" && mode != "mcp" && mode != "verify" {
			return fmt.Errorf("case %q has unsupported mode %q (expected http, mcp, or verify)", name, c.Mode)
		}
		if err := c.Chaos.Validate(); err != nil {
			return fmt.Errorf("case %q chaos: %w", name, err)
		}
		if err := c.Budget.Validate(); err != nil {
			return fmt.Errorf("case %q budget: %w", name, err)
		}
		if c.Assertions.MaxTurns < 0 {
			return fmt.Errorf("case %q assertions.max_turns must be >= 0", name)
		}
	}
	return nil
}

// DiscoverDirectorySuite scans dir recursively for cassette files (.yaml, .yml, .json),
// skipping suite manifest files, and builds a declarative SuiteSpec for static schema/budget/assertion checks.
func DiscoverDirectorySuite(dir string, defaults CaseDefaults) (*SuiteSpec, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		switch strings.ToLower(filepath.Ext(d.Name())) {
		case ".yaml", ".yml", ".json":
			if isSuiteManifestFile(path) {
				return nil
			}
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to scan directory %q: %w", dir, err)
	}
	sort.Slice(files, func(i, j int) bool {
		return filepath.ToSlash(files[i]) < filepath.ToSlash(files[j])
	})
	if len(files) == 0 {
		return nil, fmt.Errorf("no cassette files (.yaml, .yml, .json) found in directory %q", dir)
	}

	spec := &SuiteSpec{
		Version:  CurrentSuiteVersion,
		Name:     filepath.Base(filepath.Clean(dir)),
		Defaults: defaults,
		Cases:    make([]CaseSpec, 0, len(files)),
	}
	for _, f := range files {
		rel, rErr := filepath.Rel(dir, f)
		if rErr != nil {
			rel = f
		}
		spec.Cases = append(spec.Cases, CaseSpec{
			Name:     filepath.ToSlash(rel),
			Cassette: f,
			Mode:     "verify",
		})
	}
	return spec, nil
}

func isSuiteManifestFile(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var hdr struct {
		Version string `yaml:"version"`
	}
	if yaml.Unmarshal(data, &hdr) != nil {
		return false
	}
	return strings.TrimSpace(hdr.Version) == CurrentSuiteVersion
}

// Run executes all cases in spec concurrently using up to opts.Concurrency workers (default 4),
// returning results in deterministic spec order along with aggregated budget telemetry.
func Run(spec *SuiteSpec, opts RunOptions) (*SuiteReport, error) {
	if spec == nil {
		return nil, errors.New("suite spec cannot be nil")
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if err := opts.DefaultBudget.Validate(); err != nil {
		return nil, err
	}

	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = 4
	}
	if concurrency > len(spec.Cases) {
		concurrency = len(spec.Cases)
	}

	pricingTable := opts.PricingTable
	if pricingTable == nil {
		if strings.TrimSpace(spec.Defaults.PricingFile) != "" {
			pPath := resolvePath(opts.BaseDir, spec.Defaults.PricingFile)
			var err error
			pricingTable, err = budget.LoadPricingFile(pPath)
			if err != nil {
				return nil, err
			}
		} else {
			pricingTable = budget.DefaultPricingTable()
		}
	}

	suiteStart := time.Now()
	results := make([]CaseResult, len(spec.Cases))
	jobs := make(chan int, len(spec.Cases))
	for i := range spec.Cases {
		jobs <- i
	}
	close(jobs)

	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				results[idx] = executeCase(spec.Cases[idx], spec.Defaults, opts.BaseDir, pricingTable, opts.DefaultBudget)
			}
		}()
	}
	wg.Wait()

	totalDur := time.Since(suiteStart)
	caseBudgets := make([]budget.Report, 0, len(results))
	passedCount := 0
	failedCount := 0
	for _, r := range results {
		caseBudgets = append(caseBudgets, r.Budget)
		if r.Passed {
			passedCount++
		} else {
			failedCount++
		}
	}

	// Enforce CLI-level budget thresholds across the aggregate suite when provided.
	aggBudget := budget.Aggregate(caseBudgets, opts.DefaultBudget)
	overallPassed := failedCount == 0 && aggBudget.Passed

	source := opts.Source
	if source == "" {
		source = spec.Name
	}

	return &SuiteReport{
		Name:        spec.Name,
		Source:      filepath.ToSlash(source),
		Passed:      overallPassed,
		TotalCases:  len(results),
		PassedCases: passedCount,
		FailedCases: failedCount,
		Concurrency: concurrency,
		DurationMs:  totalDur.Milliseconds(),
		Duration:    totalDur,
		Budget:      aggBudget,
		Cases:       results,
	}, nil
}

func executeCase(c CaseSpec, defaults CaseDefaults, baseDir string, table *budget.PricingTable, cliBudget budget.Thresholds) (res CaseResult) {
	start := time.Now()
	casPath := resolvePath(baseDir, c.Cassette)
	mode := resolveMode(c)
	res = CaseResult{
		Name:     c.Name,
		Cassette: filepath.ToSlash(casPath),
		Mode:     mode,
		Passed:   true,
	}
	defer func() {
		res.Duration = time.Since(start)
		res.DurationMs = res.Duration.Milliseconds()
		if len(res.Failures) > 0 || res.Error != "" {
			res.Passed = false
		}
	}()

	// 1. Load, schema-validate, and secret-check cassette
	cas, err := cassette.LoadUnvalidated(casPath)
	if err != nil {
		res.Error = fmt.Sprintf("failed to load cassette: %v", err)
		res.Failures = append(res.Failures, res.Error)
		return res
	}
	if err := cas.Validate(); err != nil {
		res.Error = fmt.Sprintf("cassette validation failed: %v", err)
		res.Failures = append(res.Failures, res.Error)
		return res
	}
	if hasSecrets, reason := cassette.CheckForUnredactedSecrets(cas); hasSecrets {
		res.Error = fmt.Sprintf("security violation: %s", reason)
		res.Failures = append(res.Failures, res.Error)
		return res
	}
	res.Interactions = len(cas.Interactions)

	// 2. Build matcher, timing, timeout, and chaos injector
	m := buildMatcher(c, defaults)
	sseTiming, err := buildSSETiming(c, defaults)
	if err != nil {
		res.Error = err.Error()
		res.Failures = append(res.Failures, res.Error)
		return res
	}
	timeout, err := buildTimeout(c, defaults)
	if err != nil {
		res.Error = err.Error()
		res.Failures = append(res.Failures, res.Error)
		return res
	}
	strict := true
	if defaults.Strict != nil {
		strict = *defaults.Strict
	}
	if c.Strict != nil {
		strict = *c.Strict
	}

	var chaosInj *chaos.Injector
	if c.Chaos.Enabled() {
		chaosInj, err = chaos.NewInjector(c.Chaos)
		if err != nil {
			res.Error = fmt.Sprintf("invalid chaos config: %v", err)
			res.Failures = append(res.Failures, res.Error)
			return res
		}
	}

	// 3. Execute mode
	var matchedIDs map[string]bool
	hasCmd := strings.TrimSpace(c.Command) != ""
	expectedExit := 0
	if c.Assertions.ExpectExitCode != nil {
		expectedExit = *c.Assertions.ExpectExitCode
	}

	switch mode {
	case "http":
		if hasCmd {
			cmdArgs := append([]string{c.Command}, c.Args...)
			runRep, runErr := runner.Run(runner.RunOptions{
				Cassette:   cas,
				Matcher:    m,
				Command:    cmdArgs,
				Env:        c.Env,
				StrictMode: strict,
				Timeout:    timeout,
				Mode:       proxy.ModeReplay,
				SSETiming:  sseTiming,
				Chaos:      chaosInj,
			})
			if runErr != nil {
				res.Error = fmt.Sprintf("runner execution failed: %v", runErr)
				res.Failures = append(res.Failures, res.Error)
				return res
			}
			res.ExitCode = runRep.CommandExitCode
			res.MatchedCount = runRep.MatchedCount
			res.Turns = runRep.MatchedCount
			res.ChaosStats = runRep.ChaosStats
			matchedIDs = runRep.MatchedIDs

			if runRep.CommandExitCode != expectedExit {
				res.Failures = append(res.Failures, fmt.Sprintf("command exited with code %d (expected %d)", runRep.CommandExitCode, expectedExit))
			}
			if len(runRep.UnexpectedCalls) > 0 {
				res.Failures = append(res.Failures, fmt.Sprintf("%d unexpected unmatched HTTP request(s)", len(runRep.UnexpectedCalls)))
			}
			if strict && runRep.MatchedCount < runRep.TotalExpected {
				res.Failures = append(res.Failures, fmt.Sprintf("strict replay incomplete: matched %d of %d expected interactions (unmatched: %s)",
					runRep.MatchedCount, runRep.TotalExpected, strings.Join(runRep.UnmatchedExpected, ", ")))
			}
		} else {
			res.MatchedCount = len(cas.Interactions)
			res.Turns = len(cas.Interactions)
		}

	case "mcp":
		if hasCmd {
			exitCode, mcpMatched, mcpChaos, runErr := runMCPCaseCommand(cas, m, chaosInj, c, timeout)
			if runErr != nil {
				res.Error = fmt.Sprintf("mcp case execution failed: %v", runErr)
				res.Failures = append(res.Failures, res.Error)
				return res
			}
			res.ExitCode = exitCode
			res.MatchedCount = len(mcpMatched)
			res.Turns = len(mcpMatched)
			res.ChaosStats = mcpChaos
			matchedIDs = mcpMatched

			if exitCode != expectedExit {
				res.Failures = append(res.Failures, fmt.Sprintf("mcp command exited with code %d (expected %d)", exitCode, expectedExit))
			}
			if strict && len(mcpMatched) < len(cas.Interactions) {
				res.Failures = append(res.Failures, fmt.Sprintf("strict mcp replay incomplete: matched %d of %d expected interactions",
					len(mcpMatched), len(cas.Interactions)))
			}
		} else {
			res.MatchedCount = len(cas.Interactions)
			res.Turns = len(cas.Interactions)
		}

	default: // "verify" static mode
		res.MatchedCount = len(cas.Interactions)
		res.Turns = len(cas.Interactions)
	}

	// 4. Evaluate budget (merge defaults -> case -> CLI overrides if unset on case)
	effBudget := mergeThresholds(defaults.Budget, c.Budget, cliBudget)
	res.Budget = budget.Evaluate(cas, table, effBudget)
	if !res.Budget.Passed {
		for _, v := range res.Budget.Violations {
			res.Failures = append(res.Failures, "budget: "+v.Message)
		}
	}

	// 5. Evaluate semantic tool & turn assertions
	observedTools := ExtractObservedTools(cas, matchedIDs)
	res.ObservedTools = observedTools
	toolSet := make(map[string]bool, len(observedTools))
	for _, t := range observedTools {
		toolSet[t] = true
	}

	for _, reqTool := range c.Assertions.ExpectedTools {
		if !toolSet[reqTool] {
			res.Failures = append(res.Failures, fmt.Sprintf("assertion failed: expected tool %q was not invoked (observed: %v)", reqTool, observedTools))
		}
	}
	for _, badTool := range c.Assertions.ForbiddenTools {
		if toolSet[badTool] {
			res.Failures = append(res.Failures, fmt.Sprintf("assertion failed: forbidden tool %q was invoked", badTool))
		}
	}
	if c.Assertions.MaxTurns > 0 && res.Turns > c.Assertions.MaxTurns {
		res.Failures = append(res.Failures, fmt.Sprintf("assertion failed: turns %d exceeded max_turns %d", res.Turns, c.Assertions.MaxTurns))
	}

	return res
}

func runMCPCaseCommand(cas *cassette.Cassette, m matcher.Matcher, chaosInj *chaos.Injector, c CaseSpec, timeout time.Duration) (int, map[string]bool, chaos.Stats, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.Command, c.Args...)
	cmd.WaitDelay = 3 * time.Second
	if len(c.Env) > 0 {
		env := append([]string(nil), os.Environ()...)
		for k, v := range c.Env {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}

	childStdin, err := cmd.StdinPipe()
	if err != nil {
		return 1, nil, chaos.Stats{}, err
	}
	childStdout, err := cmd.StdoutPipe()
	if err != nil {
		return 1, nil, chaos.Stats{}, err
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return 1, nil, chaos.Stats{}, err
	}

	replayer := proxy.NewStdioReplayer(cas, m)
	if chaosInj != nil {
		replayer.SetChaos(chaosInj)
	}

	replayDone := make(chan error, 1)
	go func() {
		rErr := replayer.ProcessStream(childStdout, childStdin)
		_ = childStdin.Close()
		replayDone <- rErr
	}()

	waitErr := cmd.Wait()
	rErr := <-replayDone
	if rErr != nil && !errors.Is(rErr, io.EOF) && !errors.Is(rErr, os.ErrClosed) {
		// Pipe close errors after child exit are expected; ignore if process already finished.
		if waitErr == nil && ctx.Err() == nil {
			// Keep going if child exited cleanly.
		}
	}

	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			exitCode = 124
		} else if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	return exitCode, replayer.MatchedIDs(), replayer.ChaosStats(), nil
}

// ExtractObservedTools collects all unique tool/function names invoked across cassette interactions
// (MCP/tool_execution Request.ToolName, SSE tool_use/tool_calls/functionCall, and JSON response bodies).
func ExtractObservedTools(cas *cassette.Cassette, filterIDs map[string]bool) []string {
	if cas == nil {
		return nil
	}
	seen := make(map[string]bool)
	addTool := func(name string) {
		name = strings.TrimSpace(name)
		if name != "" {
			seen[name] = true
		}
	}

	for i := range cas.Interactions {
		inter := &cas.Interactions[i]
		if filterIDs != nil && !filterIDs[inter.ID] {
			continue
		}
		if inter.Request.ToolName != "" {
			addTool(inter.Request.ToolName)
		}
		if len(inter.Response.SSEEvents) > 0 {
			sum := cassette.ReconstructStreamSummary(inter.Response.SSEEvents)
			for _, tc := range sum.ToolCalls {
				addTool(tc.Name)
			}
		}
		if inter.Response.Body != nil {
			extractToolsFromJSONBody(inter.Response.Body, addTool)
		} else if raw := strings.TrimSpace(inter.Response.RawBody); len(raw) > 0 && raw[0] == '{' {
			var parsed interface{}
			if json.Unmarshal([]byte(raw), &parsed) == nil {
				extractToolsFromJSONBody(parsed, addTool)
			}
		}
	}

	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func extractToolsFromJSONBody(body interface{}, addTool func(string)) {
	m, ok := body.(map[string]interface{})
	if !ok {
		return
	}
	// Anthropic: content[].type == "tool_use" | "server_tool_use"
	if content, ok := m["content"].([]interface{}); ok {
		for _, item := range content {
			if block, ok := item.(map[string]interface{}); ok {
				bType, _ := block["type"].(string)
				if bType == "tool_use" || bType == "server_tool_use" {
					if name, ok := block["name"].(string); ok {
						addTool(name)
					}
				}
			}
		}
	}
	// OpenAI: choices[].message.tool_calls[].function.name
	if choices, ok := m["choices"].([]interface{}); ok {
		for _, item := range choices {
			cMap, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			msg, ok := cMap["message"].(map[string]interface{})
			if !ok {
				continue
			}
			tcs, ok := msg["tool_calls"].([]interface{})
			if !ok {
				continue
			}
			for _, tcItem := range tcs {
				tcMap, ok := tcItem.(map[string]interface{})
				if !ok {
					continue
				}
				if fn, ok := tcMap["function"].(map[string]interface{}); ok {
					if name, ok := fn["name"].(string); ok {
						addTool(name)
					}
				}
			}
		}
	}
	// Google Gemini: candidates[].content.parts[].functionCall.name
	if cands, ok := m["candidates"].([]interface{}); ok {
		for _, item := range cands {
			cMap, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			content, ok := cMap["content"].(map[string]interface{})
			if !ok {
				continue
			}
			parts, ok := content["parts"].([]interface{})
			if !ok {
				continue
			}
			for _, pItem := range parts {
				pMap, ok := pItem.(map[string]interface{})
				if !ok {
					continue
				}
				if fc, ok := pMap["functionCall"].(map[string]interface{}); ok {
					if name, ok := fc["name"].(string); ok {
						addTool(name)
					}
				}
			}
		}
	}
}

func resolvePath(baseDir, p string) string {
	if filepath.IsAbs(p) || baseDir == "" {
		return p
	}
	return filepath.Join(baseDir, p)
}

func resolveMode(c CaseSpec) string {
	m := strings.ToLower(strings.TrimSpace(c.Mode))
	if m != "" {
		return m
	}
	if strings.TrimSpace(c.Command) != "" {
		return "http"
	}
	return "verify"
}

func buildMatcher(c CaseSpec, d CaseDefaults) matcher.Matcher {
	mt := strings.TrimSpace(c.Matcher)
	if mt == "" {
		mt = strings.TrimSpace(d.Matcher)
	}
	th := c.Threshold
	if th <= 0 {
		th = d.Threshold
	}
	if th <= 0 {
		th = 0.80
	}
	if strings.EqualFold(mt, "fuzzy") {
		return matcher.NewFuzzyMatcher(th)
	}
	return matcher.NewStrictMatcher()
}

func buildSSETiming(c CaseSpec, d CaseDefaults) (cassette.SSETimingConfig, error) {
	raw := strings.TrimSpace(c.Timing)
	if raw == "" {
		raw = strings.TrimSpace(d.Timing)
	}
	mode, err := cassette.ParseSSETimingMode(raw)
	if err != nil {
		return cassette.SSETimingConfig{}, err
	}
	return cassette.SSETimingConfig{
		Mode:          mode,
		Scale:         1.0,
		MaxChunkDelay: 2 * time.Second,
	}, nil
}

func buildTimeout(c CaseSpec, d CaseDefaults) (time.Duration, error) {
	raw := strings.TrimSpace(c.Timeout)
	if raw == "" {
		raw = strings.TrimSpace(d.Timeout)
	}
	if raw == "" {
		return 60 * time.Second, nil
	}
	dur, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid timeout %q: %w", raw, err)
	}
	if dur <= 0 {
		return 60 * time.Second, nil
	}
	return dur, nil
}

func mergeThresholds(def, perCase, cli budget.Thresholds) budget.Thresholds {
	out := def
	if perCase.MaxTokens > 0 {
		out.MaxTokens = perCase.MaxTokens
	}
	if perCase.MaxCostUSD > 0 {
		out.MaxCostUSD = perCase.MaxCostUSD
	}
	if perCase.MinCacheHitRate > 0 {
		out.MinCacheHitRate = perCase.MinCacheHitRate
	}
	// Apply CLI thresholds as fallback if neither defaults nor case set them.
	if out.MaxTokens == 0 && cli.MaxTokens > 0 {
		out.MaxTokens = cli.MaxTokens
	}
	if out.MaxCostUSD == 0 && cli.MaxCostUSD > 0 {
		out.MaxCostUSD = cli.MaxCostUSD
	}
	if out.MinCacheHitRate == 0 && cli.MinCacheHitRate > 0 {
		out.MinCacheHitRate = cli.MinCacheHitRate
	}
	return out
}

// FormatSummary renders a consolidated human-readable console summary of the SuiteReport.
func (r *SuiteReport) FormatSummary() string {
	var sb strings.Builder
	sb.WriteString("=================================================================\n")
	sb.WriteString(fmt.Sprintf(" AgentFixture Test Suite: %s (concurrency=%d)\n", r.Name, r.Concurrency))
	sb.WriteString("=================================================================\n")
	for _, c := range r.Cases {
		tag := "[PASS]"
		if !c.Passed {
			tag = "[FAIL]"
		}
		sb.WriteString(fmt.Sprintf(" %s %-28s mode=%-6s turns=%d tokens=%d cost=$%.4f cache=%.1f%% (%dms)\n",
			tag, c.Name, c.Mode, c.Turns, c.Budget.TotalTokens, c.Budget.EstimatedCostUSD, c.Budget.CacheHitRate*100, c.DurationMs))
		for _, f := range c.Failures {
			sb.WriteString(fmt.Sprintf("        -> %s\n", f))
		}
	}
	sb.WriteString("-----------------------------------------------------------------\n")
	status := "PASSED"
	if !r.Passed {
		status = "FAILED"
	}
	sb.WriteString(fmt.Sprintf(" Status:            %s (%d/%d passed, %d failed)\n", status, r.PassedCases, r.TotalCases, r.FailedCases))
	sb.WriteString(fmt.Sprintf(" Total Tokens:      %d\n", r.Budget.TotalTokens))
	sb.WriteString(fmt.Sprintf(" Estimated Cost:    $%.4f USD\n", r.Budget.EstimatedCostUSD))
	sb.WriteString(fmt.Sprintf(" Cache Hit Rate:    %.1f%%\n", r.Budget.CacheHitRate*100))
	sb.WriteString(fmt.Sprintf(" Suite Duration:    %dms\n", r.DurationMs))
	if len(r.Budget.Violations) > 0 {
		sb.WriteString(" Suite Budget Violations:\n")
		for _, v := range r.Budget.Violations {
			sb.WriteString(fmt.Sprintf("   - %s\n", v.Message))
		}
	}
	sb.WriteString("=================================================================\n")
	return sb.String()
}

// ToGitHubSummaryMarkdown formats the suite run as a GitHub Step Summary Markdown table.
func (r *SuiteReport) ToGitHubSummaryMarkdown() string {
	rows := make([]budget.SummaryRow, 0, len(r.Cases))
	for _, c := range r.Cases {
		notes := fmt.Sprintf("mode=%s, turns=%d", c.Mode, c.Turns)
		if len(c.Failures) > 0 {
			notes = strings.Join(c.Failures, "; ")
		}
		rows = append(rows, budget.SummaryRow{
			Name:         c.Name,
			Passed:       c.Passed,
			Interactions: c.Interactions,
			TotalTokens:  c.Budget.TotalTokens,
			CostUSD:      c.Budget.EstimatedCostUSD,
			CacheHitRate: c.Budget.CacheHitRate,
			Details:      notes,
		})
	}
	return budget.FormatSummaryMarkdown("Test Suite ("+r.Name+")", rows, r.Budget, r.Passed)
}
