package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
	"github.com/aloneangel45/AgentFixture/pkg/importer"
	"github.com/aloneangel45/AgentFixture/pkg/matcher"
	"github.com/aloneangel45/AgentFixture/pkg/proxy"
	"github.com/aloneangel45/AgentFixture/pkg/runner"
)

const (
	Version     = "1.0.0"
	ProgramName = "AgentFixture"
	Banner      = "Zero-Dollar Deterministic CI/CD Regression Testing Cassette for AI Agents"
)

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
		fmt.Fprintf(os.Stderr, "Unknown command: %s\nRun 'agentfixture help' for usage.\n", subcommand)
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func printVersion() {
	fmt.Printf("%s v%s (%s)\n", ProgramName, Version, Banner)
}

func printUsage() {
	printVersion()
	fmt.Println(`
Usage:
  agentfixture <command> [options] [-- <command>]

Commands:
  record     Record agent API/tool interactions into a cassette
  replay     Replay recorded cassette deterministically for regression tests
  verify     Validate cassette integrity, syntax, and check for unmasked secrets
  import     Ingest TracePort flight recordings and convert to testing cassette
  diff       Compare two cassettes and report interaction/argument drifts
  version    Show version information

Examples:
  agentfixture verify fixtures/auth_flow.yaml
  agentfixture import -traceport flight.json -out cassette.yaml
  agentfixture replay -cassette cassette.yaml -- go test ./...
  agentfixture record -cassette cassette.yaml -upstream https://api.anthropic.com -- python agent.py
  agentfixture diff cassette_v1.yaml cassette_v2.yaml`)
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

	cas.ComputeMetrics()
	fmt.Printf("[✓] Cassette %s is VALID and SECURE\n", casPath)
	fmt.Printf("    Version:              %s\n", cas.Version)
	fmt.Printf("    ID:                   %s\n", cas.Metadata.CassetteID)
	fmt.Printf("    Interactions:         %d\n", cas.Metadata.Metrics.TotalInteractions)
	fmt.Printf("    Estimated Tokens Saved: %d\n", cas.Metadata.Metrics.EstimatedTokensSaved)
	fmt.Printf("    Estimated Cost Saved:   $%.4f USD\n", cas.Metadata.Metrics.EstimatedCostSavedUSD)
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

	fmt.Printf("[✓] Successfully imported TracePort flight recording!\n")
	fmt.Printf("    Source:       %s\n", *tpPath)
	fmt.Printf("    Destination:  %s\n", *outPath)
	fmt.Printf("    Interactions: %d\n", len(cas.Interactions))
	fmt.Printf("    Tokens Saved: %d\n", cas.Metadata.Metrics.EstimatedTokensSaved)
	return nil
}

func handleDiff(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: agentfixture diff <cassette1> <cassette2>")
	}
	cas1, err := cassette.Load(args[0])
	if err != nil {
		return fmt.Errorf("failed to load %s: %w", args[0], err)
	}
	cas2, err := cassette.Load(args[1])
	if err != nil {
		return fmt.Errorf("failed to load %s: %w", args[1], err)
	}

	fmt.Printf("=================================================================\n")
	fmt.Printf(" AgentFixture Diff: %s <-> %s\n", args[0], args[1])
	fmt.Printf("=================================================================\n")

	n1, n2 := len(cas1.Interactions), len(cas2.Interactions)
	fmt.Printf(" Interactions Count: %d vs %d\n", n1, n2)

	minLen := n1
	if n2 < minLen {
		minLen = n2
	}

	strict := matcher.NewStrictMatcher()
	diffFound := false

	for i := 0; i < minLen; i++ {
		i1 := &cas1.Interactions[i]
		i2 := &cas2.Interactions[i]

		res := strict.Match(&i1.Request, &i2.Request)
		if !res.Matched {
			diffFound = true
			fmt.Printf("\n[!] Interaction #%d Mismatch:\n", i+1)
			fmt.Printf("    %s (%s %s)\n", i1.ID, i1.Request.Method, i1.Request.ToolName)
			fmt.Printf("    %s (%s %s)\n", i2.ID, i2.Request.Method, i2.Request.ToolName)
			fmt.Printf("    Diff: %s\n", res.Diff)
		}
	}

	if n1 > minLen {
		diffFound = true
		fmt.Printf("\n[!] Extra interactions in %s:\n", args[0])
		for i := minLen; i < n1; i++ {
			fmt.Printf("    + #%d (%s)\n", i+1, cas1.Interactions[i].ID)
		}
	}
	if n2 > minLen {
		diffFound = true
		fmt.Printf("\n[!] Extra interactions in %s:\n", args[1])
		for i := minLen; i < n2; i++ {
			fmt.Printf("    + #%d (%s)\n", i+1, cas2.Interactions[i].ID)
		}
	}

	if !diffFound {
		fmt.Println("\n[✓] Both cassettes are structurally identical!")
	}
	fmt.Printf("=================================================================\n")
	return nil
}

func handleRecord(args []string) error {
	fs := flag.NewFlagSet("record", flag.ContinueOnError)
	casPath := fs.String("cassette", "fixture.yaml", "Path to write recorded cassette")
	upstream := fs.String("upstream", "", "Upstream target URL to forward requests to")
	listenAddr := fs.String("addr", "127.0.0.1:0", "Local address to bind proxy on")

	var cmdArgs []string
	dashIdx := -1
	for i, arg := range args {
		if arg == "--" {
			dashIdx = i
			break
		}
	}
	if dashIdx != -1 {
		if err := fs.Parse(args[:dashIdx]); err != nil {
			return err
		}
		cmdArgs = args[dashIdx+1:]
	} else {
		if err := fs.Parse(args); err != nil {
			return err
		}
	}

	cas := cassette.NewCassette(fmt.Sprintf("cas_record_%d", time.Now().Unix()))
	cas.Metadata.Source = "agentfixture_record_proxy"

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

	fmt.Printf("[•] AgentFixture Recording Proxy active on %s\n", srv.URL())
	if *upstream != "" {
		fmt.Printf("    Forwarding to Upstream: %s\n", *upstream)
	}

	if len(cmdArgs) > 0 {
		fmt.Printf("[•] Spawning command: %s\n", strings.Join(cmdArgs, " "))
		rep, err := runner.Run(runner.RunOptions{
			Cassette: cas,
			Command:  cmdArgs,
		})
		if err != nil {
			return err
		}
		fmt.Println(rep.FormatSummary())
	} else {
		fmt.Println("[•] Running standalone proxy. Press Ctrl+C to save cassette and stop.")
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		<-sigChan
	}

	recCas := srv.GetCassette()
	if err := cassette.Save(*casPath, recCas); err != nil {
		return fmt.Errorf("failed to save cassette: %w", err)
	}

	fmt.Printf("[✓] Cassette saved to %s (%d interactions recorded)\n", *casPath, len(recCas.Interactions))
	return nil
}

func handleReplay(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	casPath := fs.String("cassette", "", "Path to cassette file to replay")
	matcherType := fs.String("matcher", "strict", "Matcher type: strict or fuzzy")
	strictMode := fs.Bool("strict", true, "Fail test if any cassette interaction is uncalled")
	listenAddr := fs.String("addr", "127.0.0.1:0", "Local address to bind proxy on")

	var cmdArgs []string
	dashIdx := -1
	for i, arg := range args {
		if arg == "--" {
			dashIdx = i
			break
		}
	}
	if dashIdx != -1 {
		if err := fs.Parse(args[:dashIdx]); err != nil {
			return err
		}
		cmdArgs = args[dashIdx+1:]
	} else {
		if err := fs.Parse(args); err != nil {
			return err
		}
	}

	if *casPath == "" {
		return fmt.Errorf("-cassette <path> is required")
	}

	cas, err := cassette.Load(*casPath)
	if err != nil {
		return fmt.Errorf("failed to load cassette: %w", err)
	}

	var m matcher.Matcher
	if *matcherType == "fuzzy" {
		m = matcher.NewFuzzyMatcher(0.80)
	} else {
		m = matcher.NewStrictMatcher()
	}

	if len(cmdArgs) > 0 {
		report, err := runner.Run(runner.RunOptions{
			Cassette:   cas,
			Matcher:    m,
			Command:    cmdArgs,
			StrictMode: *strictMode,
		})
		if err != nil {
			return err
		}
		fmt.Print(report.FormatSummary())
		if !report.Passed {
			os.Exit(1)
		}
		return nil
	}

	// Standalone replay server
	srv, err := proxy.NewServer(proxy.Config{
		Mode:       proxy.ModeReplay,
		Cassette:   cas,
		Matcher:    m,
		ListenAddr: *listenAddr,
	})
	if err != nil {
		return err
	}
	if err := srv.Start(); err != nil {
		return err
	}
	defer srv.Close()

	fmt.Printf("[✓] AgentFixture Replay Server active on %s\n", srv.URL())
	fmt.Printf("    Loaded: %s (%d interactions)\n", *casPath, len(cas.Interactions))
	fmt.Println("    Running in zero-token replay mode. Press Ctrl+C to stop.")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	return nil
}
