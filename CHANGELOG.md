# Changelog

All notable changes to **AgentFixture** will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.2.1] - 2026-11-14

### Fixed
- **Atomic File Writes for JUnit XML & Cassette Serialization (`pkg/junit/junit.go`, `pkg/cassette/serializer.go`)**: Fixed `junit.WriteFile` and `cassette.Save` (which previously called `os.WriteFile` directly, risking truncated files on crash or concurrent CI reads) to write to `os.CreateTemp` in the target directory, `Sync()`, `Close()`, and atomically `os.Rename` with Windows destination-overwrite fallback and guaranteed cleanup.
- **Google Gemini & Gateway URL Query Secret Redaction + Canonical Replay Matching (`pkg/cassette/redact.go`, `pkg/matcher/matcher.go`, `pkg/matcher/fuzzy.go`, `pkg/proxy/http_proxy.go`)**: Fixed `?key=...`, `?token=...`, and `?auth_token=...` URL query parameter redaction and `CheckForUnredactedSecrets` URL query scanning; canonicalized sensitive query parameters (`key`, `api_key`, `apikey`, `access_token`, `auth_token`, `token`, `client_secret`, `password`, `secret`, `x-goog-api-key`) and `[REDACTED*]` placeholders in `matcher.NormalizeEndpoint` and `FuzzyMatcher` so cassettes recorded with live Gemini `?key=AIzaSy...` match deterministically in offline CI when replayed with non-`AIza` dummy keys (e.g. `GEMINI_API_KEY=af-offline-replay-key`) or header-only authentication.
- **Google Gemini, Anthropic, & OpenAI Prompt Extraction + `FuzzySignature` Recording (`pkg/matcher/fuzzy.go`, `pkg/proxy/http_proxy.go`, `cmd/agentfixture/main.go`)**: Added `matcher.ExtractPromptAndModel` supporting Google Gemini (`systemInstruction` + `contents[].parts[]` text, `functionCall`, `functionResponse`, and `/models/<model>` URL extraction), Anthropic (`system` blocks + `messages[]`), and OpenAI (`messages[]`, `input`, `prompt`); populated `Matcher.FuzzySignature` (`NormalizedPrompt`, `Model`) during HTTP/SSE recording and `agentfixture redact`; honored `FuzzySignature` on signature-only cassettes; and weighted conversation prompt similarity in `FuzzyMatcher` so `generationConfig`/`temperature` tweaks match cleanly while preventing false-positive matches between unrelated prompts sharing large `tools` schemas.
- **JSON `RawBody`, Nested Slice Secret Scrubbing & Replay Candidate Redaction (`pkg/cassette/redact.go`, `pkg/proxy/http_proxy.go`, `pkg/proxy/stdio_proxy.go`)**: Added 64-bit-safe (`UseNumber`) JSON `RawBody` and `SSEEvents[].Data` sensitive-field scrubbing (`password`, `client_secret`, `api_key`, `private_key`, etc.) when `Body` is `nil`, extended `CheckForUnredactedSecrets` recursively into `[]interface{}` slices and `FuzzySignature.NormalizedArgs`, redacted incoming `ModeReplay` HTTP and MCP stdio candidates prior to `StrictHash` comparison so redacted request fields match CI dummy secrets, and scrubbed upstream `*url.Error` messages in `ModeRecord` 502 responses.
- **MCP Stdio Framing, 64-Bit JSON-RPC IDs & `isError: true` Tool Results (`pkg/proxy/stdio_proxy.go`)**: Fixed `readStdioFrame` to accept arbitrary MIME header ordering (e.g. `Content-Type` preceding `Content-Length`) and return `io.ErrUnexpectedEOF` on truncated `Content-Length` bodies instead of silent `io.EOF`; decoded `JSONRPCRequest`/`JSONRPCResponse` with `json.Decoder.UseNumber()` to preserve 64-bit integer IDs (`> 2^53`); preserved full MCP `Response.Body` when `"isError": true` or `"structuredContent"` is present instead of collapsing to plain `ToolResult` text; and aligned synthesized MCP server version with `cassette.GeneratorVersion`.
- **Gemini SSE Token Preservation & CRLF Wire Injection Guard (`pkg/cassette/sse.go`)**: Prevented trailing `0` `candidatesTokenCount` / `thoughtsTokenCount` Gemini SSE metadata chunks from resetting accumulated non-zero output token counts in `mergeGeminiUsage`, and normalized `\r\n` inside `EncodeSSEEvent` `Data` and `Comment` lines so embedded CRLFs cannot forge SSE frame boundaries.
- **TracePort v1.2 `Tags`, Duplicate Frame Index Collision & Clock-Skew Handling (`pkg/importer/traceport.go`)**: Preserved and scrubbed top-level `TracePortSession.Tags` during import, deduplicated interaction IDs when `.frames.jsonl` contains repeated or zero `frame.Index` values so `cassette.Validate()` always succeeds, clamped negative NTP clock-skew latencies to `0`, and preserved `git_snapshot` metadata on chat turns.
- **CLI Trailing Flag Permutation, `diff` Structured Body/Error Drift & Installer Hardening (`cmd/agentfixture/main.go`, `install.sh`, `install.ps1`)**: Added `permuteFlagsFirst` so flags placed after positional arguments (e.g. `agentfixture verify fixtures/ -json -junit out.xml`, `diff a.yaml b.yaml -exit-code`) work across all subcommands; updated `agentfixture diff` to detect `Response.Error` and structured `Response.Body` drift; prevented `install.sh` from hanging on non-TTY `sudo` in rootless containers; and preserved existing TLS protocols via `-bor Tls12` in `install.ps1`.

## [1.2.0] - 2026-11-14

### Added
- **First-Class Google Gemini API SSE Streaming & Token/Cost Accounting (`pkg/cassette/sse.go`, `pkg/cassette/types.go`, `pkg/proxy`)**: Extended `ReconstructStreamSummary` to natively detect and reconstruct Google Gemini `:streamGenerateContent?alt=sse` streams (`candidates`, `modelVersion`, `responseId`, `thought: true` reasoning blocks, `text` deltas, `functionCall` tool invocations, `thoughtSignature` signatures, `finishReason`, and `usageMetadata`). Extended `ExtractUsageFromBody`, `ExtractResponseUsage`, and `ComputeMetrics()` so both streaming and non-streaming Gemini responses (`promptTokenCount`, `candidatesTokenCount` + `thoughtsTokenCount`, `cachedContentTokenCount`, `totalTokenCount`) accurately populate `EstimatedTokensSaved` and `EstimatedCostSavedUSD`, with full `agentfixture diff` semantic SSE support and offline Gemini SDK environment defaults in `runner.Run`.
- **Directory-Wide Batch Verification (`cmd/agentfixture`)**: Upgraded `agentfixture verify [flags] <file-or-dir>` to recursively discover `.yaml`, `.yml`, and `.json` cassettes in deterministic sorted order, validate schema compliance and scan for unredacted secrets across every cassette, report per-cassette status plus aggregate interaction/SSE/token/USD totals, emit machine-readable JSON reports via `-json`, and exit non-zero if any cassette fails.
- **Native CI/CD JUnit XML Reporting (`pkg/junit`, `cmd/agentfixture`)**: Added `pkg/junit` (`encoding/xml` with XML 1.0 control-character sanitization) and `-junit <report.xml>` flags to both `agentfixture verify` and `agentfixture replay`, emitting standard `<testsuites>`, `<testsuite>`, `<testcase>`, `<failure>`, and `<skipped>` reports for native test annotations in GitHub Actions, GitLab CI, and CircleCI.
- **Universal One-Line Installers (`install.sh`, `install.ps1`)**: Added POSIX/bash (`install.sh` for Linux & macOS `amd64`/`arm64`) and PowerShell (`install.ps1` for Windows `amd64`/`arm64`) installers with mandatory SHA-256 verification against GitHub Release `checksums.txt` prior to extraction.

## [1.1.0] - 2026-11-10

### Added
- **Anthropic Messages API & OpenAI SSE Streaming Engine (`pkg/cassette/sse.go`)**: Implemented RFC #1 with full recording, wire encoding/decoding (`bufio.Reader` with zero 64KB line limit), live proxy chunk flushing, configurable replay pacing (`instant`, `scaled`, `exact`), and `ReconstructStreamSummary` supporting `message_start`, `content_block_start`, `content_block_delta` (`text_delta`, `thinking_delta`, `signature_delta`, `input_json_delta`, `citations_delta`), `content_block_stop`, `message_delta`, `message_stop`, `ping`, `error`, and OpenAI `[DONE]` streams.
- **Zero-Outbound-Network Replay Enforcement (`pkg/proxy/offline.go`)**: Added `proxy.NewOfflineHTTPClient(proxyURL)` blocking non-loopback TCP dials (`127.0.0.1`, `::1`, `localhost`), rejected `HTTP CONNECT` tunnels with `403 Forbidden`, and injected `NO_PROXY` plus fallback offline API keys in `runner.Run`.
- **Go Testing SDK (`pkg/agenttest` & `pkg/proxy/testing.go`)**: Added `agenttest.NewTestServer(tb, cassettePath, opts...)` with `URL()`, zero-network `HTTPClient()`, `WithRecordMode`, `WithStrictExhaustive`, and automatic `tb.Cleanup` verification.
- **MCP Stdio Upgrades & Recorder (`pkg/proxy/stdio_proxy.go`)**: Added 16MB frame support, dual NDJSON and `Content-Length` framing, JSON-RPC 2.0 notification handling, automatic `ping`/`initialize`/`tools/list` synthesis when unrecorded, and `StdioRecorder` (`agentfixture record -mcp`).
- **CLI Subcommands & Flags (`cmd/agentfixture`)**: Added `agentfixture inspect [-json]`, `agentfixture redact [-out]`, `agentfixture diff -exit-code` with semantic SSE stream diffing, `-timing` / `-delay-scale` / `-max-delay` flags on `replay`, `-mcp` mode on `record`/`replay`, and `windows/arm64` release artifacts.

### Fixed
- Fixed `agentfixture record -- <cmd>` spawning an isolated replay proxy instead of routing subprocess calls through the active `ModeRecord` server.
- Fixed upstream gzip response corruption by stripping `Accept-Encoding` and `Content-Length` on outbound proxy requests and stripping `Content-Encoding` / `Content-Length` from stored headers.
- Replaced fixed 30s `http.Client.Timeout` on upstream recording requests with `Transport{ResponseHeaderTimeout: 30s}` so multi-minute LLM streams do not abort mid-response.
- Fixed upstream URL base path joining so prefixes (e.g. `/api/v1`) are preserved rather than overwritten.
- Released `Server` mutex before simulated latency sleeps, HTTP response writes, and SSE chunk streaming; cloned cassettes during `ToYAML`/`ToJSON` to prevent concurrent mutation races.
- Fixed multi-turn stateful HTTP and MCP stdio replay to prefer unconsumed cassette interactions in sequence order before falling back to idempotent retries.
- Fixed 64-bit integer precision truncation in `StrictMatcher` using `json.Decoder.UseNumber()`, canonicalized JSON `RawBody` payloads, and normalized full URLs vs relative paths with sorted query parameters.
- Fixed `FuzzyMatcher` UTF-8 rune length calculation, recursive slice filtering in `filterDynamicKeys`, structured JSON HTTP matching, and preserved bare `"id"` arguments.
- Fixed `ComputeMetrics()` to sum `InputTokens + OutputTokens` (+ prompt cache tokens) when `TotalTokens == 0` (Anthropic usage format) with accurate input/output pricing.
- Preserved `EventError` frames in `ImportTracePortSession` instead of dropping them.
- Fixed `runner.Run` timeout cancellation via `exec.CommandContext`, Windows case-insensitive environment variable deduplication, and populated `UnmatchedExpected` and `DriftDetails`.
- Fixed `-ldflags` version injection (`var Version`) and stripped duplicate leading `v` prefixes.

### Performance
- Reduced 100KB prompt `FuzzyMatcher` latency from ~15s (`O(N*M)` rune Levenshtein) to `<1ms` via zero-copy prefix/suffix stripping, a 4,096-rune exact DP cap, and zero-allocation 4-gram Sørensen-Dice hashing.

### Security
- Expanded redaction and secret detection across Google (`AIza*`), GitHub (`ghp_*`, `gho_*`, `ghu_*`, `ghs_*`, `ghr_*`, `github_pat_*`), AWS (`AKIA*`, `ASIA*`), Slack (`xox*-*`), HuggingFace (`hf_*`), PEM private keys, URL `user:pass@` credentials, `Cookie`/`Set-Cookie`, `X-Api-Key`, `x-goog-api-key`, endpoints, tags, interaction metadata, and SSE event frames.
- Redacted secrets prior to `StrictHash` computation to eliminate secret hash oracles, and masked matched credential previews in `CheckForUnredactedSecrets`.

## [1.0.0] - 2026-11-05

### Added
- Zero-dollar deterministic CI/CD regression testing cassette for AI agents.
- Cassette record/replay engine eliminating real LLM API call costs in CI.
- Mock server harness for MCP, REST, and OpenAI/Anthropic compatible APIs.
- Automated CI pipeline with multi-platform cross-compilation binaries.
- Strict timing, seed, and temperature determinism assertions.
