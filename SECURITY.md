# Security Policy

## Supported Versions

| Version | Supported |
| ------- | --------- |
| 1.1.x   | Yes       |
| 1.0.x   | Yes       |
| < 1.0   | No        |

## Security Controls in AgentFixture

- **Pre-Hash Secret Redaction**: Credentials, Bearer tokens, PEM private keys, cookies, URL basic auth, and provider API keys (`sk-ant-*`, `sk-proj-*`, `sk-*`, `AIza*`, `ghp_*`, `github_pat_*`, `AKIA*`, `ASIA*`, `xox*-*`, `hf_*`) are scrubbed before computing `StrictHash` SHA-256 digests so cassettes never act as secret hash oracles.
- **Zero Outbound Network Replay**: Replay proxies block `HTTP CONNECT` tunnels (`403 Forbidden`) and `proxy.NewOfflineHTTPClient` rejects any TCP/UDP dial outside loopback (`127.0.0.1`, `::1`, `localhost`).
- **Masked Secret Diagnostics**: `agentfixture verify` masks matched credential suffixes so CI logs never print leaked secret bytes.

## Reporting a Vulnerability

If you discover a security vulnerability in AgentFixture:
1. Do NOT open a public issue.
2. Email `security@aloneangel.dev` or use GitHub Security Advisories.
3. Include reproduction steps and details.
