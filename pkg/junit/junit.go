package junit

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
	"github.com/aloneangel45/AgentFixture/pkg/runner"
)

// TestSuites is the root JUnit XML <testsuites> element consumed by GitHub Actions, GitLab CI, and CircleCI.
type TestSuites struct {
	XMLName  xml.Name    `xml:"testsuites"`
	Name     string      `xml:"name,attr,omitempty"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Errors   int         `xml:"errors,attr"`
	Skipped  int         `xml:"skipped,attr,omitempty"`
	Time     string      `xml:"time,attr"`
	Suites   []TestSuite `xml:"testsuite"`
}

// TestSuite represents a single <testsuite> block inside <testsuites>.
type TestSuite struct {
	XMLName    xml.Name    `xml:"testsuite"`
	Name       string      `xml:"name,attr"`
	Tests      int         `xml:"tests,attr"`
	Failures   int         `xml:"failures,attr"`
	Errors     int         `xml:"errors,attr"`
	Skipped    int         `xml:"skipped,attr"`
	Time       string      `xml:"time,attr"`
	Timestamp  string      `xml:"timestamp,attr,omitempty"`
	Properties *Properties `xml:"properties,omitempty"`
	TestCases  []TestCase  `xml:"testcase"`
}

// Properties wraps <property> metadata entries on a <testsuite>.
type Properties struct {
	Items []Property `xml:"property"`
}

// Property represents a key/value <property> attribute pair.
type Property struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

// TestCase represents an individual <testcase> assertion in JUnit XML.
type TestCase struct {
	XMLName   xml.Name `xml:"testcase"`
	Name      string   `xml:"name,attr"`
	ClassName string   `xml:"classname,attr"`
	File      string   `xml:"file,attr,omitempty"`
	Time      string   `xml:"time,attr"`
	Failure   *Failure `xml:"failure,omitempty"`
	Error     *Failure `xml:"error,omitempty"`
	Skipped   *Skipped `xml:"skipped,omitempty"`
	SystemOut string   `xml:"system-out,omitempty"`
}

// Failure represents a <failure> or <error> node inside a <testcase>.
type Failure struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr,omitempty"`
	Body    string `xml:",chardata"`
}

// Skipped represents a <skipped> node inside a <testcase>.
type Skipped struct {
	Message string `xml:"message,attr,omitempty"`
}

// VerifyCaseInput describes the verification result of one cassette file for JUnit conversion.
type VerifyCaseInput struct {
	Path                  string
	CassetteID            string
	Valid                 bool
	Interactions          int
	SSEStreams            int
	EstimatedTokensSaved  int64
	EstimatedCostSavedUSD float64
	Duration              time.Duration
	ErrorType             string
	Error                 string
}

func normalizeSlashPath(p string) string {
	return strings.ReplaceAll(filepath.ToSlash(p), "\\", "/")
}

// BuildVerifySuites constructs a standard JUnit XML report from single-file or batch cassette verification results.
func BuildVerifySuites(target string, cases []VerifyCaseInput, totalTokens int64, totalCostUSD float64, totalDuration time.Duration) TestSuites {
	normTarget := normalizeSlashPath(target)
	suite := TestSuite{
		Name:      "agentfixture.verify",
		Time:      formatSeconds(totalDuration),
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Properties: &Properties{
			Items: []Property{
				{Name: "target", Value: sanitizeXMLText(normTarget)},
				{Name: "estimated_tokens_saved", Value: fmt.Sprintf("%d", totalTokens)},
				{Name: "estimated_cost_saved_usd", Value: fmt.Sprintf("%.4f", totalCostUSD)},
			},
		},
		TestCases: make([]TestCase, 0, maxInt(1, len(cases))),
	}

	for _, c := range cases {
		normPath := sanitizeXMLText(normalizeSlashPath(c.Path))
		tc := TestCase{
			Name:      normPath,
			ClassName: "agentfixture.verify",
			File:      normPath,
			Time:      formatSeconds(c.Duration),
		}
		if c.Valid {
			tc.SystemOut = sanitizeXMLText(fmt.Sprintf(
				"cassette_id=%s interactions=%d sse_streams=%d tokens_saved=%d cost_saved_usd=%.4f",
				c.CassetteID, c.Interactions, c.SSEStreams, c.EstimatedTokensSaved, c.EstimatedCostSavedUSD,
			))
		} else {
			errType := c.ErrorType
			if errType == "" {
				errType = "VerificationError"
			}
			msg := sanitizeXMLText(c.Error)
			tc.Failure = &Failure{
				Message: msg,
				Type:    sanitizeXMLText(errType),
				Body:    fmt.Sprintf("File: %s\nReason: %s", normPath, msg),
			}
			suite.Failures++
		}
		suite.TestCases = append(suite.TestCases, tc)
	}

	suite.Tests = len(suite.TestCases)
	return TestSuites{
		Name:     "AgentFixture Verification",
		Tests:    suite.Tests,
		Failures: suite.Failures,
		Errors:   suite.Errors,
		Skipped:  suite.Skipped,
		Time:     suite.Time,
		Suites:   []TestSuite{suite},
	}
}

// BuildReplaySuites constructs a standard JUnit XML report from a runner.TestReport and its Cassette.
func BuildReplaySuites(cas *cassette.Cassette, casPath string, rep *runner.TestReport) TestSuites {
	normPath := sanitizeXMLText(normalizeSlashPath(casPath))
	casID := "unknown"
	if rep != nil && rep.CassetteID != "" {
		casID = rep.CassetteID
	} else if cas != nil && cas.Metadata.CassetteID != "" {
		casID = cas.Metadata.CassetteID
	}

	var totalDuration time.Duration
	var tokensSaved int64
	var costSaved float64
	if rep != nil {
		totalDuration = rep.Duration
		tokensSaved = rep.TokensSaved
		costSaved = rep.CostSavedUSD
	}

	suite := TestSuite{
		Name:      "agentfixture.replay." + sanitizeXMLText(casID),
		Time:      formatSeconds(totalDuration),
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Properties: &Properties{
			Items: []Property{
				{Name: "cassette_id", Value: sanitizeXMLText(casID)},
				{Name: "cassette_path", Value: normPath},
				{Name: "estimated_tokens_saved", Value: fmt.Sprintf("%d", tokensSaved)},
				{Name: "estimated_cost_saved_usd", Value: fmt.Sprintf("%.4f", costSaved)},
			},
		},
	}

	unmatchedSet := make(map[string]bool)
	driftByID := make(map[string][]runner.DriftInfo)
	if rep != nil {
		for _, id := range rep.UnmatchedExpected {
			unmatchedSet[id] = true
		}
		for _, d := range rep.DriftDetails {
			driftByID[d.InteractionID] = append(driftByID[d.InteractionID], d)
		}
	}

	var interactions []cassette.Interaction
	if cas != nil {
		interactions = cas.Interactions
	}

	var perCaseDur time.Duration
	if len(interactions) > 0 && totalDuration > 0 {
		perCaseDur = totalDuration / time.Duration(len(interactions))
	}

	strictFailed := rep != nil && !rep.Passed && len(rep.UnmatchedExpected) > 0 && rep.CommandExitCode == 0 && len(rep.UnexpectedCalls) == 0

	for _, inter := range interactions {
		desc := describeInteraction(&inter)
		tc := TestCase{
			Name:      sanitizeXMLText(fmt.Sprintf("%s (%s)", inter.ID, desc)),
			ClassName: "agentfixture.replay." + sanitizeXMLText(casID),
			File:      normPath,
			Time:      formatSeconds(perCaseDur),
		}

		if unmatchedSet[inter.ID] {
			if strictFailed || (rep != nil && !rep.Passed) {
				msg := fmt.Sprintf("expected cassette interaction %q (%s) was not called during replay", inter.ID, desc)
				tc.Failure = &Failure{
					Message: sanitizeXMLText(msg),
					Type:    "UnmatchedInteraction",
					Body:    sanitizeXMLText(fmt.Sprintf("Cassette: %s\nInteraction: %s (#%d)\nTarget: %s", normPath, inter.ID, inter.Sequence, desc)),
				}
				suite.Failures++
			} else {
				tc.Skipped = &Skipped{
					Message: "interaction not invoked during non-strict replay",
				}
				suite.Skipped++
			}
		} else if drifts := driftByID[inter.ID]; len(drifts) > 0 {
			var lines []string
			for _, d := range drifts {
				lines = append(lines, fmt.Sprintf("semantic_drift similarity=%.2f target=%s reason=%s", d.Similarity, d.ToolOrPath, d.Reason))
			}
			tc.SystemOut = sanitizeXMLText(strings.Join(lines, "\n"))
		}

		suite.TestCases = append(suite.TestCases, tc)
	}

	if rep != nil {
		for i, u := range rep.UnexpectedCalls {
			target := strings.TrimSpace(u.Method + " " + u.Endpoint)
			if u.ToolName != "" {
				target = "tool:" + u.ToolName
			}
			if target == "" {
				target = "unknown_request"
			}
			msg := fmt.Sprintf("unexpected agent call #%d not matched in cassette: %s", i+1, target)
			tc := TestCase{
				Name:      sanitizeXMLText(fmt.Sprintf("unexpected_call_%d (%s)", i+1, target)),
				ClassName: "agentfixture.replay." + sanitizeXMLText(casID),
				File:      normPath,
				Time:      "0.000000",
				Failure: &Failure{
					Message: sanitizeXMLText(msg),
					Type:    "UnexpectedCall",
					Body:    sanitizeXMLText(fmt.Sprintf("Cassette: %s\nUnexpected Call: %s\nBody: %s", normPath, target, truncateText(u.RawBody, 1024))),
				},
			}
			suite.Failures++
			suite.TestCases = append(suite.TestCases, tc)
		}

		if rep.CommandExitCode != 0 || len(suite.TestCases) == 0 || (!rep.Passed && suite.Failures == 0) {
			tc := TestCase{
				Name:      "command_execution",
				ClassName: "agentfixture.replay." + sanitizeXMLText(casID),
				File:      normPath,
				Time:      formatSeconds(totalDuration),
			}
			if rep.CommandExitCode != 0 {
				msg := fmt.Sprintf("target command exited with non-zero status code %d", rep.CommandExitCode)
				tc.Failure = &Failure{
					Message: sanitizeXMLText(msg),
					Type:    "CommandExitError",
					Body:    sanitizeXMLText(truncateText(rep.CommandOutput, 4096)),
				}
				suite.Failures++
			} else if !rep.Passed {
				tc.Failure = &Failure{
					Message: "replay regression check failed",
					Type:    "ReplayRegression",
					Body:    sanitizeXMLText(rep.FormatSummary()),
				}
				suite.Failures++
			}
			suite.TestCases = append(suite.TestCases, tc)
		}
	}

	suite.Tests = len(suite.TestCases)
	return TestSuites{
		Name:     "AgentFixture Replay",
		Tests:    suite.Tests,
		Failures: suite.Failures,
		Errors:   suite.Errors,
		Skipped:  suite.Skipped,
		Time:     suite.Time,
		Suites:   []TestSuite{suite},
	}
}

// BuildMCPReplaySuites constructs a JUnit XML report for MCP stdio replay sessions.
func BuildMCPReplaySuites(cas *cassette.Cassette, casPath string, duration time.Duration, replayErr error) TestSuites {
	normPath := sanitizeXMLText(normalizeSlashPath(casPath))
	casID := "mcp_stdio"
	if cas != nil && cas.Metadata.CassetteID != "" {
		casID = cas.Metadata.CassetteID
	}

	tc := TestCase{
		Name:      "mcp_stdio_session",
		ClassName: "agentfixture.replay.mcp." + sanitizeXMLText(casID),
		File:      normPath,
		Time:      formatSeconds(duration),
	}
	failures := 0
	if replayErr != nil {
		msg := sanitizeXMLText(replayErr.Error())
		tc.Failure = &Failure{
			Message: msg,
			Type:    "MCPReplayError",
			Body:    msg,
		}
		failures = 1
	}

	suite := TestSuite{
		Name:      "agentfixture.replay.mcp." + sanitizeXMLText(casID),
		Tests:     1,
		Failures:  failures,
		Time:      formatSeconds(duration),
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		TestCases: []TestCase{tc},
	}
	return TestSuites{
		Name:     "AgentFixture MCP Replay",
		Tests:    1,
		Failures: failures,
		Time:     suite.Time,
		Suites:   []TestSuite{suite},
	}
}

// Marshal encodes TestSuites into UTF-8 JUnit XML bytes with a standard XML declaration header.
func Marshal(suites TestSuites) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	enc.Indent("", "  ")
	if err := enc.Encode(suites); err != nil {
		return nil, fmt.Errorf("failed to marshal junit xml: %w", err)
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// WriteFile marshals TestSuites as JUnit XML and writes it atomically to path, creating parent directories as needed.
func WriteFile(path string, suites TestSuites) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("junit output path cannot be empty")
	}
	data, err := Marshal(suites)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create junit report directory %s: %w", dir, err)
		}
	}
	return os.WriteFile(path, data, 0644)
}

func describeInteraction(inter *cassette.Interaction) string {
	if inter == nil {
		return "unknown"
	}
	if inter.Request.ToolName != "" {
		return "tool:" + inter.Request.ToolName
	}
	target := strings.TrimSpace(inter.Request.Method + " " + inter.Request.Endpoint)
	if target != "" {
		return target
	}
	return string(inter.Type)
}

func formatSeconds(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%.6f", d.Seconds())
}

func truncateText(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "...[truncated]"
}

// sanitizeXMLText strips illegal XML 1.0 control characters (< 0x20 except \t, \n, \r, plus 0xFFFE/0xFFFF)
// and invalid UTF-8 sequences so XML parsers in CI never reject reports containing terminal escape sequences.
func sanitizeXMLText(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, w := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && w == 1 {
			i++
			continue
		}
		if isValidXML10Rune(r) {
			b.WriteRune(r)
		}
		i += w
	}
	return b.String()
}

func isValidXML10Rune(r rune) bool {
	return r == 0x09 || r == 0x0A || r == 0x0D ||
		(r >= 0x20 && r <= 0xD7FF) ||
		(r >= 0xE000 && r <= 0xFFFD) ||
		(r >= 0x10000 && r <= 0x10FFFF)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
