package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
	"github.com/aloneangel45/AgentFixture/pkg/importer"
	"github.com/aloneangel45/AgentFixture/pkg/matcher"
	"github.com/aloneangel45/AgentFixture/pkg/proxy"
	"github.com/aloneangel45/AgentFixture/pkg/runner"
)

// Version is injected at build time via -ldflags="-X main.Version=...".
var Version = "1.1.0"

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
		clean = "1.1.0"
	}
	return fmt.Sprintf("%s v%s (%s)", ProgramName, clean, Banner)
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
  record     Record HTTP, SSE streaming, or MCP stdio interactions into a cassette
  replay     Replay recorded cassette deterministically with zero outbound network calls
  verify     Validate cassette integrity, schema, and check for unmasked secrets
  inspect    Display cassette telemetry, interaction summary, and reconstructed SSE streams
  redact     Scrub secrets from a cassette file and recompute secret-safe hashes
  import     Ingest TracePort flight recordings and convert to testing cassette
  diff       Compare two cassettes for request, response, or semantic SSE stream drift
  version    Show version information

Examples:
  agentfixture verify fixtures/auth_flow.yaml
  agentfixture inspect fixtures/anthropic_sse.yaml
  agentfixture redact fixtures/raw_capture.yaml
  agentfixture import -traceport flight.json -out cassette.yaml
  agentfixture replay -cassette cassette.yaml -timing instant -- go test ./...
  agentfixture record -cassette cassette.yaml -upstream https://api.anthropic.com -- python agent.py
  agentfixture record -mcp -cassette mcp_tools.yaml -- npx -y @modelcontextprotocol/server-filesystem .
  agentfixture diff -exit-code cassette_v1.yaml cassette_v2.yaml`)
}

func handleVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	strictCheck := fs.Bool("strict", true, "Enforce strict schema validation")
	if err := fs.Parse(args); err != nil {
		return err
	}

	rest := fs.Args()
	if len(rest) < 1 {
		return fmt.Errorf("missing cassette path: agentfixture verify <path>")
	}
	casPath := rest[0]

	cas, err := cassette.Load(casPath)
	if err != nil {
		return fmt.Errorf("failed to load cassette: %w", err)
	}

	if *strictCheck {
		if err := cas.Validate(); err != nil {
			return fmt.Errorf("cassette validation failed: %w", err)
		}
	}

	hasSecrets, secretReason := cassette.CheckForUnredactedSecrets(cas)
	if hasSecrets {
		return fmt.Errorf("security violation: %s", secretReason)
	}

	sseCount := 0
	for _, inter := range cas.Interactions {
		if inter.Type == cassette.InteractionTypeSSE || len(inter.Response.SSEEvents) > 0 {
			sseCount++
		}
	}

	cas.ComputeMetrics()
	fmt.Printf("[OK] Cassette %s is VALID and SECURE\n", casPath)
	fmt.Printf("    Version:                %s\n", cas.Version)
	fmt.Printf("    ID:                     %s\n", cas.Metadata.CassetteID)
	fmt.Printf("    Interactions:           %d (SSE streams: %d)\n", cas.Metadata.Metrics.TotalInteractions, sseCount)
	fmt.Printf("    Estimated Tokens Saved: %d\n", cas.Metadata.Metrics.EstimatedTokensSaved)
	fmt.Printf("    Estimated Cost Saved:   $%.4f USD\n", cas.Metadata.Metrics.EstimatedCostSavedUSD)
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
	if err := fs.Parse(args); err != nil {
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
			Usage:      inter.Response.Usage,
		}
		if inter.Type == cassette.InteractionTypeSSE || len(inter.Response.SSEEvents) > 0 {
			sum := cassette.ReconstructStreamSummary(inter.Response.SSEEvents)
			view.StreamSummary = &sum
			if view.Usage.InputTokens == 0 && view.Usage.OutputTokens == 0 && view.Usage.TotalTokens == 0 {
				view.Usage = sum.Usage
			}
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
	if err := fs.Parse(args); err != nil {
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
		} else if inter.Request.RawBody != "" && inter.Request.Matcher.FuzzySignature.NormalizedPrompt != "" {
			inter.Request.Matcher.FuzzySignature.NormalizedPrompt = matcher.NormalizeText(inter.Request.RawBody)
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
	if err := fs.Parse(args); err != nil {
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
	if err := fs.Parse(args); err != nil {
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
			(len(i1.Response.SSEEvents) == 0 && len(i2.Response.SSEEvents) == 0 &&
				(i1.Response.RawBody != i2.Response.RawBody || i1.Response.ToolResult != i2.Response.ToolResult)) {
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
	if err := fs.Parse(flagArgs); err != nil {
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

	flagArgs, cmdArgs := splitDashArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
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
		replayer := proxy.NewStdioReplayer(cas, m)
		return replayer.ProcessStream(os.Stdin, os.Stdout)
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
