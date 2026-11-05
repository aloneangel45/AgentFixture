# AgentFixture

[![CI](https://github.com/aloneangel45/AgentFixture/actions/workflows/ci.yml/badge.svg)](https://github.com/aloneangel45/AgentFixture/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/aloneangel45/AgentFixture)](https://github.com/aloneangel45/AgentFixture/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](https://opensource.org/licenses/MIT)
[![Go Report Card](https://goreportcard.com/badge/github.com/aloneangel45/AgentFixture)](https://goreportcard.com/report/github.com/aloneangel45/AgentFixture)

> **Zero-dollar deterministic CI/CD regression testing cassette runner for AI agents.**

`AgentFixture` eliminates the exorbitant API costs and flaky non-determinism of testing autonomous AI coding agents (Claude Code, Cursor, Windsurf, Aider, OpenHands) in Continuous Integration. By converting [TracePort](https://github.com/aloneangel45/traceport) black-box flight recordings into deterministic testing cassettes, AgentFixture allows entire agent test suites to run **instantly in CI with $0 in LLM API bills**.

```
                      +-----------------------------+
                      |   LLM Agent / Claude 3.7    |
                      +--------------+--------------+
                                     |
                          [Tool Requests / Calls]
                                     v
                      +-----------------------------+
                      |          ToolPrune          |  <-- -91.4% Context Bloat Reduction
                      +--------------+--------------+
                                     |
                          [Filtered Tool Calls]
                                     v
                      +-----------------------------+
                      |          TracePort          |  <-- Black-Box Flight Recording & Git Tree Snapshots
                      +--------------+--------------+
                                     |
                        [Flight Session JSON Export]
                                     v
                      +-----------------------------+
                      |        AgentFixture         |  <-- Zero-Dollar Deterministic CI/CD Replay & Regression
                      +-----------------------------+
```

---

## Why AgentFixture?

1. **Eliminate the "Broken Agent Tax"**: Running a 25-test agent suite against Claude 3.7 Sonnet or GPT-4o costs **$5 to $50 per CI run**. For an active team with 15 PR runs daily, that's **$2,250+/month** in burned tokens. With AgentFixture, CI costs are **$0.00**.
2. **Sub-Second Test Runtimes**: Agent test runs that usually take 5–15 minutes waiting on LLM generation run in **under 500 milliseconds** during cassette replay.
3. **Flakiness-Proof Determinism**: Temperature drift, LLM provider rate limits (`429 Too Many Requests`), and transient network dropouts no longer cause false-positive build failures.
4. **TracePort Native Synergy**: TracePort records actual developer terminal agent sessions; AgentFixture ingests those flight recordings into permanent regression fixtures with one command.
5. **Zero Secret Leaks**: Built-in automated secret redactor scrubs Anthropic keys (`sk-ant-*`), OpenAI keys (`sk-*`), GitHub tokens (`ghp_*`, `github_pat_*`), Bearer tokens, and sensitive headers before cassettes ever touch git.

---

## Architecture

AgentFixture operates as an intelligent VCR proxy and testing harness:

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                            AgentFixture Architecture                        │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│   ┌─────────────────────────────────────────────────────────────────────┐   │
│   │                        CLI & Go SDK Entrypoint                      │   │
│   │    (agentfixture record | replay | verify | import | diff)          │   │
│   └──────────────────────────────────┬──────────────────────────────────┘   │
│                                      │                                      │
│                 ┌────────────────────┴────────────────────┐                 │
│                 ▼                                         ▼                 │
│   ┌───────────────────────────┐             ┌───────────────────────────┐   │
│   │   Recording Proxy Engine  │             │ Deterministic Replay Eng. │   │
│   │   • HTTP Proxy (OpenAI/   │             │ • Strict SHA-256 Matcher  │   │
│   │     Anthropic API tap)    │             │ • Fuzzy Semantic Matcher  │   │
│   │   • Stdio / MCP Stream    │             │ • Dynamic Key Exclusion   │   │
│   │   • Secret Redactor (sk-*)│             │ • Actionable Diff Engine  │   │
│   └─────────────┬─────────────┘             └─────────────┬─────────────┘   │
│                 │                                         ▲                 │
│                 │          ┌───────────────────┐          │                 │
│                 └─────────►│  Cassette Store   │──────────┘                 │
│                            │  (YAML / JSON)    │                            │
│                            └─────────▲─────────┘                            │
│                                      │                                      │
│                            ┌─────────┴─────────┐                            │
│                            │TracePort Importer │                            │
│                            │(Flight Log -> Cas)│                            │
│                            └───────────────────┘                            │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## Installation

### Pre-Compiled Binaries

Download pre-compiled binaries for Windows, Linux, and macOS from the [Releases](https://github.com/aloneangel45/AgentFixture/releases) page.

```bash
# macOS / Linux (via curl)
curl -fsSL https://github.com/aloneangel45/AgentFixture/releases/latest/download/agentfixture-linux-amd64.tar.gz | tar -xz
sudo mv agentfixture /usr/local/bin/

# Windows (PowerShell)
Invoke-WebRequest -Uri https://github.com/aloneangel45/AgentFixture/releases/latest/download/agentfixture-windows-amd64.zip -OutFile agentfixture.zip
Expand-Archive agentfixture.zip -DestinationPath C:\Windows\System32
```

### Go Install

```bash
go install github.com/aloneangel45/AgentFixture/cmd/agentfixture@latest
```

---

## Quick Start & CLI Usage

### 1. Ingest a TracePort Flight Recording
Convert any session recorded with `traceport record` into an AgentFixture cassette:
```bash
agentfixture import -traceport ~/.traceport/sessions/trace_1793907746.json -out fixtures/auth_flow.yaml
```

### 2. Verify Cassette Integrity & Redaction
Ensure the cassette format conforms to `agentfixture/v1` and contains no leaked credentials:
```bash
agentfixture verify fixtures/auth_flow.yaml
```

Output:
```text
[✓] Cassette fixtures/auth_flow.yaml is VALID and SECURE
    Version:              agentfixture/v1
    ID:                   trace_1793907746
    Interactions:         8
    Estimated Tokens Saved: 24,150
    Estimated Cost Saved:   $0.1449 USD
```

### 3. Replay in CI/CD (Zero-Dollar Test Runner)
Run your agent's test suite inside GitHub Actions without configuring LLM API keys:
```bash
agentfixture replay -cassette fixtures/auth_flow.yaml -- go test -v ./...
```

Output:
```text
=================================================================
 AgentFixture Regression Report: trace_1793907746
=================================================================
 Status:              PASSED
 Exit Code:           0
 Interactions Replayed: 8 / 8 (100.0%)
 Estimated Tokens Saved: 24,150
 Estimated Cost Saved:   $0.1449 USD
 Replay Run Duration:    182ms
=================================================================
```

### 4. Record a Live Agent Session
Tap incoming agent API calls to Anthropic or OpenAI and store them in a new cassette:
```bash
agentfixture record -cassette fixtures/new_feature.yaml -upstream https://api.anthropic.com -- python run_agent.py
```

### 5. Diff Two Cassettes (Drift Analysis)
Compare prompt alterations or tool input changes between cassette versions:
```bash
agentfixture diff fixtures/auth_v1.yaml fixtures/auth_v2.yaml
```

---

## Cassette Specification (`agentfixture/v1`)

Cassettes can be stored as either **YAML** or **JSON**:

```yaml
version: "agentfixture/v1"
metadata:
  cassette_id: "cas_auth_middleware_01"
  recorded_at: "2026-11-05T20:30:00Z"
  generator: "AgentFixture v1.0.0"
  agent_name: "claude-code"
  model: "claude-3-7-sonnet-20250219"
  metrics:
    total_interactions: 2
    recorded_duration_ms: 1240
    estimated_tokens_saved: 14200
    estimated_cost_saved_usd: 0.0852
  tags:
    git_snapshot: "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
interactions:
  - id: "tp_step_1"
    sequence: 1
    type: "tool_execution"
    request:
      tool_name: "view_file"
      tool_input:
        path: "pkg/auth/auth.go"
      matcher:
        strict_hash: "sha256:7e8a9b1c3d..."
        fuzzy_signature:
          normalized_tool_name: "view_file"
          normalized_args:
            path: "pkg/auth/auth.go"
    response:
      status_code: 0
      latency_ms: 32
      tool_result: "package auth\n\nfunc ValidateToken(t string) bool { return true }\n"
```

---

## Matching Engines

| Matcher | Behavior | Use Case |
|---|---|---|
| **Strict Matcher** | Canonical SHA-256 hash comparison across sorted JSON keys, headers, and endpoints. | Production CI assertion; ensures exact reproducibility. |
| **Fuzzy Matcher** | Normalizes whitespace, ignores dynamic keys (`timestamp`, `nonce`, `request_id`), strips UUIDs and ISO timestamps, uses Levenshtein similarity. | Resilient regression tests where prompt phrasing evolves slightly. |

---

## GitHub Actions CI Integration

Add AgentFixture to your repository workflow:

```yaml
name: Agent Regression Suite

on:
  push:
    branches: [main]
  pull_request:
    branches: [main]

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.23'
      - name: Install AgentFixture
        run: go install github.com/aloneangel45/AgentFixture/cmd/agentfixture@latest
      - name: Run Zero-Dollar Agent Regression Test
        run: agentfixture replay -cassette fixtures/agent_suite.yaml -- go test -v ./...
```

---

## License

MIT License. Copyright (c) 2026 [aloneangel45](https://github.com/aloneangel45).
