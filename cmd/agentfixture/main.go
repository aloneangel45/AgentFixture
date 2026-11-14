package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
	"github.com/aloneangel45/AgentFixture/pkg/importer"
	"github.com/aloneangel45/AgentFixture/pkg/junit"
	"github.com/aloneangel45/AgentFixture/pkg/matcher"
	"github.com/aloneangel45/AgentFixture/pkg/proxy"
	"github.com/aloneangel45/AgentFixture/pkg/runner"
)

// Version is injected at build time via -ldflags="-X main.Version=...".
var Version = "1.2.1"

const (
	ProgramName = "AgentFixture"
	Banner      = "Zero-Dollar Deterministic CI/CD Regression Testing Cassette for AI Agents"
)

var errDiffDetected = errors.New("cassette drift detected")

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	subcommand := os.Args[1]
	args := os.Args[2:]

	var err error
	switch subcommand {
	case "version", "-v", "--version":
		printVersion()
	case "verify":
		err = handleVerify(args)
	case "inspect":
		err = handleInspect(args)
	case "redact":
		err = handleRedact(args)
	case "import":
		err = handleImport(args)
	case "diff":
		err = handleDiff(args)
	case "record":
		err = handleRecord(args)
	case "replay":
		err = handleReplay(args)
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "[FAIL] Unknown command: %s\nRun 'agentfixture help' for usage.\n", subcommand)
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "[FAIL] Error: %v\n", err)
		os.Exit(1)
	}
}

func formatVersionString(ver string) string {
	clean := strings.TrimPrefix(strings.TrimSpace(ver), "v")
	if clean == "" {
		clean = "1.2.1"
	}
	return fmt.Sprintf("%s v%s (%s)", ProgramName, clean, Banner)
}

// permuteFlagsFirst reorders args so flags (and their values) precede positional arguments,
// allowing idiomatic CLI invocations such as `agentfixture verify fixtures/ -json -junit out.xml`
// while preserving `--` argument termination.
func permuteFlagsFirst(fs *flag.FlagSet, args []string) []string {
	if len(args) == 0 {
		return args
	}
	flags := make([]string, 0, len(args))
	positional := make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i:]...)
			break
		}
		if len(arg) < 2 || arg[0] != '-' {
			positional = append(positional, arg)
			continue
		}
		name := strings.TrimPrefix(arg, "-")
		if strings.HasPrefix(name, "-") {
			name = name[1:]
		}
		if strings.Contains(name, "=") {
			flags = append(flags, arg)
			continue
		}
		f := fs.Lookup(name)
		if f == nil {
			flags = append(flags, arg)
			continue
		}
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
			flags = append(flags, arg)
			continue
		}
		flags = append(flags, arg)
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positional...)
}

func printVersion() {
	fmt.Println(formatVersionString(Version))
}

func printUsage() {
	printVersion()
	fmt.Println(`
Usage:
  agentfixture <command> [options] [-- <command>]

Commands:
  record     Record HTTP, SSE streaming (Anthropic, OpenAI, Gemini), or MCP stdio interactions
  replay     Replay recorded cassette deterministically with zero outbound network calls
  verify     Validate single cassette or recursive directory batch, schema, and secret safety
  inspect    Display cassette telemetry, interaction summary, and reconstructed SSE streams
  redact     Scrub secrets from a cassette file and recompute secret-safe hashes
  import     Ingest TracePort flight recordings and convert to testing cassette
  diff       Compare two cassettes for request, response, or semantic SSE stream drift
  version    Show version information

Examples:
  agentfixture verify fixtures/
  agentfixture verify -json -junit verify-report.xml fixtures/
  agentfixture inspect fixtures/gemini_sse.yaml
  agentfixture redact fixtures/raw_capture.yaml
  agentfixture import -traceport flight.json -out cassette.yaml
  agentfixture replay -cassette cassette.yaml -timing instant -junit replay-report.xml -- go test ./...
  agentfixture record -cassette cassette.yaml -upstream https://generativelanguage.googleapis.com -- python agent.py
  agentfixture record -mcp -cassette mcp_tools.yaml -- npx -y @modelcontextprotocol/server-filesystem .
  agentfixture diff -exit-code cassette_v1.yaml cassette_v2.yaml`)
}

type verifyFileResult struct {
	Path                  string  `json:"path"`
	Valid                 bool    `json:"valid"`
	Version               string  `json:"version,omitempty"`
	CassetteID            string  `json:"cassette_id,omitempty"`
	Interactions          int     `json:"interactions"`
	SSEStreams            int     `json:"sse_streams"`
	EstimatedTokensSaved  int64   `json:"estimated_tokens_saved"`
	EstimatedCostSavedUSD float64 `json:"estimated_cost_saved_usd"`
	DurationMs            int64   `json:"duration_ms"`
	ErrorType             string  `json:"error_type,omitempty"`
	Error                 string  `json:"error,omitempty"`
	duration              time.Duration
}

type verifySummaryReport struct {
	Target                string             `json:"target"`
	Mode                  string             `json:"mode"`
	Passed                bool               `json:"passed"`
	TotalCassettes        int                `json:"total_cassettes"`
	ValidCassettes        int                `json:"valid_cassettes"`
	FailedCassettes       int                `json:"failed_cassettes"`
	TotalInteractions     int                `json:"total_interactions"`
	TotalSSEStreams       int                `json:"total_sse_streams"`
	EstimatedTokensSaved  int64              `json:"estimated_tokens_saved"`
	EstimatedCostSavedUSD float64            `json:"estimated_cost_saved_usd"`
	Results               []verifyFileResult `json:"results"`
}

func verifySingleCassetteFile(casPath string, strictCheck bool) (verifyFileResult, error) {
	start := time.Now()
	res := verifyFileResult{
		Path: filepath.ToSlash(casPath),
	}

	cas, err := cassette.LoadUnvalidated(casPath)
	if err != nil {
		res.duration = time.Since(start)
		res.DurationMs = res.duration.Milliseconds()
		res.ErrorType = "LoadError"
		res.Error = fmt.Sprintf("failed to load cassette: %v", err)
		return res, fmt.Errorf("failed to load cassette: %w", err)
	}

	if strictCheck {
		if vErr := cas.Validate(); vErr != nil {
			res.duration = time.Since(start)
			res.DurationMs = res.duration.Milliseconds()
			res.ErrorType = "ValidationError"
			res.Error = fmt.Sprintf("cassette validation failed: %v", vErr)
			return res, fmt.Errorf("cassette validation failed: %w", vErr)
		}
	}

	if hasSecrets, secretReason := cassette.CheckForUnredactedSecrets(cas); hasSecrets {
		res.duration = time.Since(start)
		res.DurationMs = res.duration.Milliseconds()
		res.ErrorType = "SecurityViolation"
		res.Error = fmt.Sprintf("security violation: %s", secretReason)
		return res, fmt.Errorf("security violation: %s", secretReason)
	}

	sseCount := 0
	for _, inter := range cas.Interactions {
		if inter.Type == cassette.InteractionTypeSSE || len(inter.Response.SSEEvents) > 0 {
			sseCount++
		}
	}

	cas.ComputeMetrics()
	res.duration = time.Since(start)
	res.DurationMs = res.duration.Milliseconds()
	res.Valid = true
	res.Version = cas.Version
	res.CassetteID = cas.Metadata.CassetteID
	res.Interactions = cas.Metadata.Metrics.TotalInteractions
	res.SSEStreams = sseCount
	res.EstimatedTokensSaved = cas.Metadata.Metrics.EstimatedTokensSaved
	res.EstimatedCostSavedUSD = cas.Metadata.Metrics.EstimatedCostSavedUSD
	return res, nil
}

func discoverCassetteFiles(rootDir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(rootDir, func(path string, d fs.DirEntry, walkErr error) error {
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
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool {
		return filepath.ToSlash(files[i]) < filepath.ToSlash(files[j])
	})
	return files, nil
}

func toJUnitVerifyCases(results []verifyFileResult) []junit.VerifyCaseInput {
	cases := make([]junit.VerifyCaseInput, 0, len(results))
	for _, r := range results {
		cases = append(cases, junit.VerifyCaseInput{
			Path:                  r.Path,
			CassetteID:            r.CassetteID,
			Valid:                 r.Valid,
			Interactions:          r.Interactions,
			SSEStreams:            r.SSEStreams,
			EstimatedTokensSaved:  r.EstimatedTokensSaved,
			EstimatedCostSavedUSD: r.EstimatedCostSavedUSD,
			Duration:              r.duration,
			ErrorType:             r.ErrorType,
			Error:                 r.Error,
		})
	}
	return cases
}

func handleVerify(args []string) error {
	flagSet := flag.NewFlagSet("verify", flag.ContinueOnError)
	strictCheck := flagSet.Bool("strict", true, "Enforce strict schema validation")
	jsonOut := flagSet.Bool("json", false, "Emit structured JSON verification report")
	junitPath := flagSet.String("junit", "", "Write JUnit XML verification report to path")
	if err := flagSet.Parse(permuteFlagsFirst(flagSet, args)); err != nil {
		return err
	}

	rest := flagSet.Args()
	if len(rest) < 1 {
		return fmt.Errorf("missing cassette path: agentfixture verify [flags] <file-or-dir>")
	}
	targetPath := rest[0]
	suiteStart := time.Now()

	info, statErr := os.Stat(targetPath)
	if statErr != nil {
		failRes := verifyFileResult{
			Path:      filepath.ToSlash(targetPath),
			Valid:     false,
			ErrorType: "LoadError",
			Error:     statErr.Error(),
		}
		if *junitPath != "" {
			suites := junit.BuildVerifySuites(targetPath, toJUnitVerifyCases([]verifyFileResult{failRes}), 0, 0, time.Since(suiteStart))
			if wErr := junit.WriteFile(*junitPath, suites); wErr != nil {
				return wErr
			}
		}
		if *jsonOut {
			rep := verifySummaryReport{
				Target:          filepath.ToSlash(targetPath),
				Mode:            "file",
				Passed:          false,
				TotalCassettes:  1,
				FailedCassettes: 1,
				Results:         []verifyFileResult{failRes},
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(rep)
		}
		return fmt.Errorf("failed to load cassette: %w", statErr)
	}

	if !info.IsDir() {
		res, verifyErr := verifySingleCassetteFile(targetPath, *strictCheck)
		rep := verifySummaryReport{
			Target:                filepath.ToSlash(targetPath),
			Mode:                  "file",
			Passed:                res.Valid,
			TotalCassettes:        1,
			TotalInteractions:     res.Interactions,
			TotalSSEStreams:       res.SSEStreams,
			EstimatedTokensSaved:  res.EstimatedTokensSaved,
			EstimatedCostSavedUSD: res.EstimatedCostSavedUSD,
			Results:               []verifyFileResult{res},
		}
		if res.Valid {
			rep.ValidCassettes = 1
		} else {
			rep.FailedCassettes = 1
		}

		if *junitPath != "" {
			suites := junit.BuildVerifySuites(targetPath, toJUnitVerifyCases(rep.Results), rep.EstimatedTokensSaved, rep.EstimatedCostSavedUSD, time.Since(suiteStart))
			if wErr := junit.WriteFile(*junitPath, suites); wErr != nil {
				return wErr
			}
		}

		if *jsonOut {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if encErr := enc.Encode(rep); encErr != nil {
				return encErr
			}
			return verifyErr
		}

		if verifyErr != nil {
			return verifyErr
		}

		fmt.Printf("[OK] Cassette %s is VALID and SECURE\n", targetPath)
		fmt.Printf("    Version:                %s\n", res.Version)
		fmt.Printf("    ID:                     %s\n", res.CassetteID)
		fmt.Printf("    Interactions:           %d (SSE streams: %d)\n", res.Interactions, res.SSEStreams)
		fmt.Printf("    Estimated Tokens Saved: %d\n", res.EstimatedTokensSaved)
		fmt.Printf("    Estimated Cost Saved:   $%.4f USD\n", res.EstimatedCostSavedUSD)
		return nil
	}

	// Directory-wide batch verification
	files, err := discoverCassetteFiles(targetPath)
	if err != nil {
		return fmt.Errorf("failed to scan directory %s: %w", targetPath, err)
	}
	if len(files) == 0 {
		emptyErr := fmt.Errorf("no cassette files (.yaml, .yml, .json) found in directory %s", targetPath)
		failRes := verifyFileResult{
			Path:      filepath.ToSlash(targetPath),
			Valid:     false,
			ErrorType: "EmptyDirectory",
			Error:     emptyErr.Error(),
		}
		if *junitPath != "" {
			suites := junit.BuildVerifySuites(targetPath, toJUnitVerifyCases([]verifyFileResult{failRes}), 0, 0, time.Since(suiteStart))
			if wErr := junit.WriteFile(*junitPath, suites); wErr != nil {
				return wErr
			}
		}
		if *jsonOut {
			rep := verifySummaryReport{
				Target:  filepath.ToSlash(targetPath),
				Mode:    "directory",
				Passed:  false,
				Results: []verifyFileResult{},
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(rep)
		}
		return emptyErr
	}

	rep := verifySummaryReport{
		Target:         filepath.ToSlash(targetPath),
		Mode:           "directory",
		TotalCassettes: len(files),
		Results:        make([]verifyFileResult, 0, len(files)),
	}

	for _, fPath := range files {
		res, _ := verifySingleCassetteFile(fPath, *strictCheck)
		if res.Valid {
			rep.ValidCassettes++
			rep.TotalInteractions += res.Interactions
			rep.TotalSSEStreams += res.SSEStreams
			rep.EstimatedTokensSaved += res.EstimatedTokensSaved
			rep.EstimatedCostSavedUSD += res.EstimatedCostSavedUSD
		} else {
			rep.FailedCassettes++
		}
		rep.Results = append(rep.Results, res)
	}
	rep.Passed = rep.FailedCassettes == 0

	if *junitPath != "" {
		suites := junit.BuildVerifySuites(targetPath, toJUnitVerifyCases(rep.Results), rep.EstimatedTokensSaved, rep.EstimatedCostSavedUSD, time.Since(suiteStart))
		if wErr := junit.WriteFile(*junitPath, suites); wErr != nil {
			return wErr
		}
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if encErr := enc.Encode(rep); encErr != nil {
			return encErr
		}
		if !rep.Passed {
			return fmt.Errorf("batch verification failed: %d of %d cassette(s) failed", rep.FailedCassettes, rep.TotalCassettes)
		}
		return nil
	}

	fmt.Println("=================================================================")
	fmt.Printf(" AgentFixture Batch Verification: %s\n", targetPath)
	fmt.Println("=================================================================")
	for _, r := range rep.Results {
		if r.Valid {
			fmt.Printf(" [OK]   %s (id=%s, interactions=%d, sse=%d, tokens=%d, saved=$%.4f)\n",
				r.Path, r.CassetteID, r.Interactions, r.SSEStreams, r.EstimatedTokensSaved, r.EstimatedCostSavedUSD)
		} else {
			fmt.Printf(" [FAIL] %s: %s\n", r.Path, r.Error)
		}
	}
	fmt.Println("-----------------------------------------------------------------")
	fmt.Printf(" Total Cassettes:        %d (valid: %d, failed: %d)\n",
		rep.TotalCassettes, rep.ValidCassettes, rep.FailedCassettes)
	fmt.Printf(" Total Interactions:     %d (SSE streams: %d)\n",
		rep.TotalInteractions, rep.TotalSSEStreams)
	fmt.Printf(" Estimated Tokens Saved: %d\n", rep.EstimatedTokensSaved)
	fmt.Printf(" Estimated Cost Saved:   $%.4f USD\n", rep.EstimatedCostSavedUSD)
	fmt.Println("=================================================================")

	if !rep.Passed {
		return fmt.Errorf("batch verification failed: %d of %d cassette(s) failed", rep.FailedCassettes, rep.TotalCassettes)
	}
	return nil
}

type inspectInteractionView struct {
	Sequence      int                      `json:"sequence"`
	ID            string                   `json:"id"`
	Type          cassette.InteractionType `json:"type"`
	Method        string                   `json:"method,omitempty"`
	Endpoint      string                   `json:"endpoint,omitempty"`
	ToolName      string                   `json:"tool_name,omitempty"`
	StatusCode    int                      `json:"status_code"`
	LatencyMs     int64                    `json:"latency_ms,omitempty"`
	Usage         cassette.Usage           `json:"usage,omitempty"`
	StreamSummary *cassette.StreamSummary  `json:"stream_summary,omitempty"`
}

type inspectReport struct {
	Path         string                   `json:"path"`
	Version      string                   `json:"version"`
	Metadata     cassette.Metadata        `json:"metadata"`
	TypeCounts   map[string]int           `json:"type_counts"`
	Interactions []inspectInteractionView `json:"interactions"`
}

func handleInspect(args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "Emit structured JSON telemetry")
	if err := fs.Parse(permuteFlagsFirst(fs, args)); err != nil {
		return err
	}

	rest := fs.Args()
	if len(rest) < 1 {
		return fmt.Errorf("usage: agentfixture inspect [-json] <cassette>")
	}
	casPath := rest[0]

	cas, err := cassette.Load(casPath)
	if err != nil {
		return fmt.Errorf("failed to load cassette: %w", err)
	}
	cas.ComputeMetrics()

	rep := inspectReport{
		Path:         casPath,
		Version:      cas.Version,
		Metadata:     cas.Metadata,
		TypeCounts:   make(map[string]int),
		Interactions: make([]inspectInteractionView, 0, len(cas.Interactions)),
	}

	for _, inter := range cas.Interactions {
		rep.TypeCounts[string(inter.Type)]++
		view := inspectInteractionView{
			Sequence:   inter.Sequence,
			ID:         inter.ID,
			Type:       inter.Type,
			Method:     inter.Request.Method,
			Endpoint:   inter.Request.Endpoint,
			ToolName:   inter.Request.ToolName,
			StatusCode: inter.Response.StatusCode,
			LatencyMs:  inter.Response.LatencyMs,
			Usage:      cassette.ExtractResponseUsage(&inter.Response),
		}
		if inter.Type == cassette.InteractionTypeSSE || len(inter.Response.SSEEvents) > 0 {
			sum := cassette.ReconstructStreamSummary(inter.Response.SSEEvents)
			view.StreamSummary = &sum
		}
		rep.Interactions = append(rep.Interactions, view)
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}

	fmt.Println("=================================================================")
	fmt.Printf(" AgentFixture Inspection: %s\n", casPath)
	fmt.Println("=================================================================")
	fmt.Printf(" Cassette ID:            %s\n", cas.Metadata.CassetteID)
	fmt.Printf(" Schema Version:         %s\n", cas.Version)
	if cas.Metadata.Model != "" {
		fmt.Printf(" Model:                  %s\n", cas.Metadata.Model)
	}
	if cas.Metadata.Source != "" {
		fmt.Printf(" Source:                 %s\n", cas.Metadata.Source)
	}
	fmt.Printf(" Recorded At:            %s\n", cas.Metadata.RecordedAt.Format(time.RFC3339))
	fmt.Printf(" Total Interactions:     %d\n", cas.Metadata.Metrics.TotalInteractions)
	fmt.Printf(" Recorded Duration:      %d ms\n", cas.Metadata.Metrics.RecordedDurationMs)
	fmt.Printf(" Estimated Tokens Saved: %d\n", cas.Metadata.Metrics.EstimatedTokensSaved)
	fmt.Printf(" Estimated Cost Saved:   $%.4f USD\n", cas.Metadata.Metrics.EstimatedCostSavedUSD)

	if len(rep.Interactions) > 0 {
		fmt.Println("-----------------------------------------------------------------")
		fmt.Println(" Interactions:")
		for _, v := range rep.Interactions {
			target := strings.TrimSpace(v.Method + " " + v.Endpoint)
			if v.ToolName != "" {
				target = "tool:" + v.ToolName
			}
			fmt.Printf("  #%02d [%s] %-14s %s (status=%d, latency=%dms)\n",
				v.Sequence, v.ID, v.Type, target, v.StatusCode, v.LatencyMs)
			if v.StreamSummary != nil {
				s := v.StreamSummary
				var toolNames []string
				for _, tc := range s.ToolCalls {
					toolNames = append(toolNames, tc.Name)
				}
				fmt.Printf("      SSE Summary: provider=%s model=%s events=%d stop=%s in=%d out=%d thinking=%d tools=%v\n",
					s.Provider, s.Model, s.EventCount, s.StopReason,
					s.Usage.InputTokens, s.Usage.OutputTokens,
					len(s.ThinkingBlocks), toolNames)
			}
		}
	}
	fmt.Println("=================================================================")
	return nil
}

func handleRedact(args []string) error {
	fs := flag.NewFlagSet("redact", flag.ContinueOnError)
	outPath := fs.String("out", "", "Destination path (defaults to in-place overwrite)")
	if err := fs.Parse(permuteFlagsFirst(fs, args)); err != nil {
		return err
	}

	rest := fs.Args()
	if len(rest) < 1 {
		return fmt.Errorf("usage: agentfixture redact [-out <dest>] <cassette>")
	}
	srcPath := rest[0]
	dest := *outPath
	if dest == "" {
		dest = srcPath
	}

	cas, err := cassette.Load(srcPath)
	if err != nil {
		return fmt.Errorf("failed to load cassette: %w", err)
	}

	cassette.RedactCassette(cas)
	for i := range cas.Interactions {
		inter := &cas.Interactions[i]
		inter.Request.Matcher.StrictHash = matcher.ComputeRequestHash(&inter.Request)
		if inter.Request.ToolName != "" {
			inter.Request.Matcher.FuzzySignature.NormalizedToolName = strings.ToLower(inter.Request.ToolName)
			inter.Request.Matcher.FuzzySignature.NormalizedArgs = cassette.RedactMap(inter.Request.ToolInput)
		} else {
			prompt, model := matcher.ExtractPromptAndModel(&inter.Request)
			if prompt != "" {
				inter.Request.Matcher.FuzzySignature.NormalizedPrompt = prompt
			} else if inter.Request.Matcher.FuzzySignature.NormalizedPrompt != "" {
				inter.Request.Matcher.FuzzySignature.NormalizedPrompt = matcher.NormalizeText(inter.Request.Matcher.FuzzySignature.NormalizedPrompt)
			}
			if model != "" && inter.Request.Matcher.FuzzySignature.Model == "" {
				inter.Request.Matcher.FuzzySignature.Model = model
			}
		}
	}

	if hasSecrets, reason := cassette.CheckForUnredactedSecrets(cas); hasSecrets {
		return fmt.Errorf("post-redaction check failed: %s", reason)
	}

	if err := cassette.Save(dest, cas); err != nil {
		return fmt.Errorf("failed to write redacted cassette: %w", err)
	}

	fmt.Printf("[OK] Redacted cassette saved to %s (%d interactions scrubbed)\n", dest, len(cas.Interactions))
	return nil
}

func handleImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	tpPath := fs.String("traceport", "", "Path to TracePort flight recording JSON file")
	outPath := fs.String("out", "cassette.yaml", "Output path for the generated cassette file")
	if err := fs.Parse(permuteFlagsFirst(fs, args)); err != nil {
		return err
	}

	if *tpPath == "" {
		return fmt.Errorf("-traceport <file.json> is required")
	}

	cas, err := importer.ImportTracePortFile(*tpPath, *outPath)
	if err != nil {
		return err
	}

	fmt.Printf("[OK] Imported TracePort flight recording\n")
	fmt.Printf("    Source:       %s\n", *tpPath)
	fmt.Printf("    Destination:  %s\n", *outPath)
	fmt.Printf("    Interactions: %d\n", len(cas.Interactions))
	fmt.Printf("    Tokens Saved: %d\n", cas.Metadata.Metrics.EstimatedTokensSaved)
	return nil
}

func handleDiff(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	exitCode := fs.Bool("exit-code", false, "Exit with non-zero status code when differences are found")
	if err := fs.Parse(permuteFlagsFirst(fs, args)); err != nil {
		return err
	}

	rest := fs.Args()
	if len(rest) < 2 {
		return fmt.Errorf("usage: agentfixture diff [-exit-code] <cassette1> <cassette2>")
	}
	path1, path2 := rest[0], rest[1]

	cas1, err := cassette.Load(path1)
	if err != nil {
		return fmt.Errorf("failed to load %s: %w", path1, err)
	}
	cas2, err := cassette.Load(path2)
	if err != nil {
		return fmt.Errorf("failed to load %s: %w", path2, err)
	}

	fmt.Printf("=================================================================\n")
	fmt.Printf(" AgentFixture Diff: %s <-> %s\n", path1, path2)
	fmt.Printf("=================================================================\n")

	n1, n2 := len(cas1.Interactions), len(cas2.Interactions)
	fmt.Printf(" Interactions Count: %d vs %d\n", n1, n2)

	minLen := n1
	if n2 < minLen {
		minLen = n2
	}

	strict := matcher.NewStrictMatcher()
	fuzzy := matcher.NewFuzzyMatcher(0.80)
	diffFound := false

	for i := 0; i < minLen; i++ {
		i1 := &cas1.Interactions[i]
		i2 := &cas2.Interactions[i]

		res := strict.Match(&i1.Request, &i2.Request)
		if !res.Matched {
			diffFound = true
			fuzzyRes := fuzzy.Match(&i1.Request, &i2.Request)
			fmt.Printf("\n[WARN] Interaction #%d Request Drift:\n", i+1)
			fmt.Printf("    Left:  %s (%s %s %s)\n", i1.ID, i1.Request.Method, i1.Request.Endpoint, i1.Request.ToolName)
			fmt.Printf("    Right: %s (%s %s %s)\n", i2.ID, i2.Request.Method, i2.Request.Endpoint, i2.Request.ToolName)
			fmt.Printf("    Semantic Similarity: %.2f\n", fuzzyRes.Score)
			fmt.Printf("    Diff: %s\n", res.Diff)
		}

		if sseDiffs := diffSSEResponses(i1, i2); len(sseDiffs) > 0 {
			diffFound = true
			fmt.Printf("\n[WARN] Interaction #%d SSE Stream Semantic Drift:\n", i+1)
			for _, d := range sseDiffs {
				fmt.Printf("    - %s\n", d)
			}
		} else if i1.Response.StatusCode != i2.Response.StatusCode ||
			i1.Response.Error != i2.Response.Error ||
			(len(i1.Response.SSEEvents) == 0 && len(i2.Response.SSEEvents) == 0 &&
				(i1.Response.RawBody != i2.Response.RawBody ||
					i1.Response.ToolResult != i2.Response.ToolResult ||
					!canonicalBodyEqual(i1.Response.Body, i2.Response.Body))) {
			diffFound = true
			fmt.Printf("\n[WARN] Interaction #%d Response Drift (status %d vs %d)\n",
				i+1, i1.Response.StatusCode, i2.Response.StatusCode)
		}
	}

	if n1 > minLen {
		diffFound = true
		fmt.Printf("\n[WARN] Extra interactions in %s:\n", path1)
		for i := minLen; i < n1; i++ {
			fmt.Printf("    + #%d (%s)\n", i+1, cas1.Interactions[i].ID)
		}
	}
	if n2 > minLen {
		diffFound = true
		fmt.Printf("\n[WARN] Extra interactions in %s:\n", path2)
		for i := minLen; i < n2; i++ {
			fmt.Printf("    + #%d (%s)\n", i+1, cas2.Interactions[i].ID)
		}
	}

	if !diffFound {
		fmt.Println("\n[OK] Both cassettes are structurally and semantically identical")
	}
	fmt.Printf("=================================================================\n")

	if diffFound && *exitCode {
		return errDiffDetected
	}
	return nil
}

func diffSSEResponses(i1, i2 *cassette.Interaction) []string {
	has1 := i1.Type == cassette.InteractionTypeSSE || len(i1.Response.SSEEvents) > 0
	has2 := i2.Type == cassette.InteractionTypeSSE || len(i2.Response.SSEEvents) > 0
	if !has1 && !has2 {
		return nil
	}
	if has1 != has2 {
		return []string{fmt.Sprintf("interaction type changed: %s vs %s", i1.Type, i2.Type)}
	}

	s1 := cassette.ReconstructStreamSummary(i1.Response.SSEEvents)
	s2 := cassette.ReconstructStreamSummary(i2.Response.SSEEvents)

	var diffs []string
	if s1.Provider != s2.Provider && (s1.Provider != "" || s2.Provider != "") {
		diffs = append(diffs, fmt.Sprintf("provider changed: %q -> %q", s1.Provider, s2.Provider))
	}
	if s1.Model != s2.Model {
		diffs = append(diffs, fmt.Sprintf("model changed: %q -> %q", s1.Model, s2.Model))
	}
	if s1.StopReason != s2.StopReason {
		diffs = append(diffs, fmt.Sprintf("stop_reason changed: %q -> %q", s1.StopReason, s2.StopReason))
	}
	if s1.Text != s2.Text {
		diffs = append(diffs, fmt.Sprintf("assistant text changed (len %d -> %d)", len(s1.Text), len(s2.Text)))
	}
	if !reflect.DeepEqual(s1.ThinkingBlocks, s2.ThinkingBlocks) {
		diffs = append(diffs, fmt.Sprintf("thinking blocks changed (%d -> %d)", len(s1.ThinkingBlocks), len(s2.ThinkingBlocks)))
	}
	if len(s1.ToolCalls) != len(s2.ToolCalls) {
		diffs = append(diffs, fmt.Sprintf("tool_use count changed: %d -> %d", len(s1.ToolCalls), len(s2.ToolCalls)))
	} else {
		for idx := range s1.ToolCalls {
			tc1, tc2 := s1.ToolCalls[idx], s2.ToolCalls[idx]
			if tc1.Name != tc2.Name || !reflect.DeepEqual(tc1.Input, tc2.Input) {
				diffs = append(diffs, fmt.Sprintf("tool_use[%d] changed: %s(%v) -> %s(%v)",
					idx, tc1.Name, tc1.Input, tc2.Name, tc2.Input))
			}
		}
	}
	return diffs
}

func canonicalBodyEqual(a, b interface{}) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	ca, err1 := matcher.CanonicalJSON(a)
	cb, err2 := matcher.CanonicalJSON(b)
	if err1 != nil || err2 != nil {
		return reflect.DeepEqual(a, b)
	}
	return string(ca) == string(cb)
}

func splitDashArgs(args []string) (flagArgs []string, cmdArgs []string) {
	for i, arg := range args {
		if arg == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

func handleRecord(args []string) error {
	fs := flag.NewFlagSet("record", flag.ContinueOnError)
	casPath := fs.String("cassette", "fixture.yaml", "Path to write recorded cassette")
	upstream := fs.String("upstream", "", "Upstream target URL to forward HTTP/SSE requests to")
	listenAddr := fs.String("addr", "127.0.0.1:0", "Local address to bind proxy on")
	mcpMode := fs.Bool("mcp", false, "Record an MCP stdio server subprocess instead of an HTTP proxy")

	flagArgs, cmdArgs := splitDashArgs(args)
	if err := fs.Parse(permuteFlagsFirst(fs, flagArgs)); err != nil {
		return err
	}

	cas := cassette.NewCassette(fmt.Sprintf("cas_record_%d", time.Now().Unix()))

	if *mcpMode {
		if len(cmdArgs) == 0 {
			return fmt.Errorf("-mcp record mode requires a target server command after '--'")
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		rec := proxy.NewStdioRecorder(cas)
		fmt.Fprintf(os.Stderr, "[INFO] AgentFixture MCP Stdio Recorder wrapping: %s\n", strings.Join(cmdArgs, " "))
		recCas, runErr := rec.RecordCommand(ctx, cmdArgs, os.Environ(), os.Stdin, os.Stdout)
		if saveErr := cassette.Save(*casPath, recCas); saveErr != nil {
			return fmt.Errorf("failed to save MCP cassette: %w", saveErr)
		}
		fmt.Fprintf(os.Stderr, "[OK] MCP cassette saved to %s (%d interactions recorded)\n", *casPath, len(recCas.Interactions))
		return runErr
	}

	cas.Metadata.Source = "agentfixture_record_proxy"
	if *upstream == "" {
		*upstream = "https://api.anthropic.com"
	}

	srv, err := proxy.NewServer(proxy.Config{
		Mode:        proxy.ModeRecord,
		UpstreamURL: *upstream,
		Cassette:    cas,
		ListenAddr:  *listenAddr,
	})
	if err != nil {
		return fmt.Errorf("failed to initialize recording proxy: %w", err)
	}
	if err := srv.Start(); err != nil {
		return fmt.Errorf("failed to start recording proxy: %w", err)
	}
	defer srv.Close()

	fmt.Printf("[INFO] AgentFixture Recording Proxy active on %s\n", srv.URL())
	fmt.Printf("       Forwarding to Upstream: %s\n", *upstream)

	var cmdExecErr error
	if len(cmdArgs) > 0 {
		fmt.Printf("[INFO] Spawning command: %s\n", strings.Join(cmdArgs, " "))
		rep, runErr := runner.Run(runner.RunOptions{
			Cassette: cas,
			Mode:     proxy.ModeRecord,
			Server:   srv,
			Command:  cmdArgs,
		})
		if runErr != nil {
			return runErr
		}
		fmt.Print(rep.FormatSummary())
		if !rep.Passed {
			cmdExecErr = fmt.Errorf("recorded command exited with code %d", rep.CommandExitCode)
		}
	} else {
		fmt.Println("[INFO] Running standalone proxy. Press Ctrl+C to save cassette and stop.")
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		<-sigChan
	}

	recCas := srv.GetCassette()
	if err := cassette.Save(*casPath, recCas); err != nil {
		return fmt.Errorf("failed to save cassette: %w", err)
	}

	fmt.Printf("[OK] Cassette saved to %s (%d interactions recorded)\n", *casPath, len(recCas.Interactions))
	return cmdExecErr
}

func handleReplay(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	casPath := fs.String("cassette", "", "Path to cassette file to replay")
	matcherType := fs.String("matcher", "strict", "Matcher type: strict or fuzzy")
	threshold := fs.Float64("threshold", 0.80, "Similarity threshold for fuzzy matcher (0.0-1.0)")
	strictMode := fs.Bool("strict", true, "Fail test if any cassette interaction is uncalled")
	listenAddr := fs.String("addr", "127.0.0.1:0", "Local address to bind proxy on")
	timingStr := fs.String("timing", "instant", "SSE stream chunk timing: instant, scaled, or exact")
	delayScale := fs.Float64("delay-scale", 1.0, "Multiplier applied to recorded SSE delta_ms when -timing=scaled")
	maxDelay := fs.Duration("max-delay", 2*time.Second, "Maximum per-chunk delay cap in scaled/exact SSE replay (0 for uncapped)")
	mcpMode := fs.Bool("mcp", false, "Run deterministic MCP JSON-RPC replay over stdin/stdout")
	junitPath := fs.String("junit", "", "Write JUnit XML regression report to path")

	flagArgs, cmdArgs := splitDashArgs(args)
	if err := fs.Parse(permuteFlagsFirst(fs, flagArgs)); err != nil {
		return err
	}

	if *casPath == "" {
		return fmt.Errorf("-cassette <path> is required")
	}

	sseMode, err := cassette.ParseSSETimingMode(*timingStr)
	if err != nil {
		return err
	}
	sseTiming := cassette.SSETimingConfig{
		Mode:          sseMode,
		Scale:         *delayScale,
		MaxChunkDelay: *maxDelay,
	}

	cas, err := cassette.Load(*casPath)
	if err != nil {
		return fmt.Errorf("failed to load cassette: %w", err)
	}

	var m matcher.Matcher
	if strings.EqualFold(*matcherType, "fuzzy") {
		m = matcher.NewFuzzyMatcher(*threshold)
	} else {
		m = matcher.NewStrictMatcher()
	}

	if *mcpMode {
		start := time.Now()
		replayer := proxy.NewStdioReplayer(cas, m)
		runErr := replayer.ProcessStream(os.Stdin, os.Stdout)
		if *junitPath != "" {
			suites := junit.BuildMCPReplaySuites(cas, *casPath, time.Since(start), runErr)
			if wErr := junit.WriteFile(*junitPath, suites); wErr != nil {
				return wErr
			}
		}
		return runErr
	}

	if len(cmdArgs) > 0 {
		report, err := runner.Run(runner.RunOptions{
			Cassette:   cas,
			Matcher:    m,
			Command:    cmdArgs,
			StrictMode: *strictMode,
			SSETiming:  sseTiming,
		})
		if err != nil {
			return err
		}
		if *junitPath != "" {
			suites := junit.BuildReplaySuites(cas, *casPath, report)
			if wErr := junit.WriteFile(*junitPath, suites); wErr != nil {
				return wErr
			}
		}
		fmt.Print(report.FormatSummary())
		if !report.Passed {
			return fmt.Errorf("regression check failed")
		}
		return nil
	}

	// Standalone replay server
	srv, err := proxy.NewServer(proxy.Config{
		Mode:       proxy.ModeReplay,
		Cassette:   cas,
		Matcher:    m,
		ListenAddr: *listenAddr,
		SSETiming:  sseTiming,
	})
	if err != nil {
		return err
	}
	if err := srv.Start(); err != nil {
		return err
	}
	defer srv.Close()

	fmt.Printf("[OK] AgentFixture Replay Server active on %s\n", srv.URL())
	fmt.Printf("    Loaded:     %s (%d interactions)\n", *casPath, len(cas.Interactions))
	fmt.Printf("    SSE Timing: %s\n", sseTiming.Mode)
	fmt.Println("    Running in zero-token offline replay mode. Press Ctrl+C to stop.")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	return nil
}
