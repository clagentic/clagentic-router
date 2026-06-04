# Security Policy

## Reporting a Vulnerability

Report security vulnerabilities using GitHub's private vulnerability reporting:

**[Report a vulnerability](https://github.com/clagentic/clagentic-router/security/advisories/new)**

Do not open a public issue. We will acknowledge your report within **3 business days**
and work with you to assess and address the issue before any public disclosure.

## Scope

The following are in scope for security reports:

- The `clagentic-router` daemon process and its HTTP API
- Bearer token authentication and authorization enforcement
- Webhook HMAC signing and delivery
- Configuration parsing — specifically any path where malformed config could
  allow privilege escalation, credential leakage, or denial of service
- SQLite state store — any path where stored data could be manipulated to
  affect routing or auth decisions

The following are **out of scope**:

- Vulnerabilities in the upstream LLM providers (Anthropic, OpenAI, etc.)
- The `claude`, `codex`, or `gemini` CLI binaries and their OAuth sessions
- Issues requiring physical access to the host

## What to Include in a Report

Please provide:

1. **Version** — output of `clagentic-router version`
2. **Reproduction steps** — minimal config and request sequence that triggers the issue
3. **Impact** — what an attacker can achieve (e.g., unauthenticated access, token
   exfiltration, SSRF, DoS)
4. **Suggested fix** (optional but appreciated)

## Response Timeline

| Stage | Target |
|---|---|
| Acknowledgement | 3 business days |
| Initial assessment | 7 business days |
| Fix or workaround | Dependent on severity; critical issues prioritised |
| Public disclosure | Coordinated with reporter after fix is available |
