# Spike S2 — Claude Code through the pass-through

Date: 2026-10-17. Runner: implementing agent, on the owner's Mac.
Setup: real daemon (`accountant serve`, then launchd `com.accountant.agent`),
accounts `primary` (priority 10, genuinely weekly-limited until Oct 21) and
`secondary` (priority 20, has quota), client key `macbook`.
Claude Code 2.1.302, `ANTHROPIC_BASE_URL=http://127.0.0.1:8787`,
`ANTHROPIC_AUTH_TOKEN=<client key>`.

## Checklist

| Check | Result | Evidence |
|---|---|---|
| Normal interactive session | **PASS** | pty-driven `claude` TUI loads, answers prompts, `/exit` clean |
| Long tool-using session | **PASS** | `claude -p` created and read back a file (Write + Read tools) |
| Subagent | **PASS** | Task-tool subagent computed and returned 17×23=391 |
| MCP tool search, `ENABLE_TOOL_SEARCH=true` | **PASS (partial)** | session works with the flag; no deferred MCP tools were loaded in the scratch dir, so `tool_reference` forwarding was not exercised with real traffic |
| `/status` / `/usage` display | **PASS with documented loss** | TUI and panels render; header shows "API Usage Billing" instead of subscription account info (expected: no `/login` display behind a gateway) |
| Forced 429 | **PASS — real, not simulated** | primary is genuinely weekly-limited; see below |
| Compaction | **PASS** | manual `/compact` completed: "Compacted (ctrl+o to see full summary)" |
| Session-header paths | **PASS** | every observed API call was `POST /v1/messages?beta=true` and always carried `x-claude-code-session-id`; no other API paths observed in a tool-using run |
| Time to first byte | **recorded** | Haiku minimal ≈ 1.0 s; Sonnet thinking (10k budget) 3.6 s; Opus thinking (24k budget) 0.6 s TTFB with a 114 s total stream. Default `response_header_timeout_seconds = 120` keeps >30× headroom; no change |

## The real rollover (highest-risk validation step)

First request of a fresh `claude -p` session:

1. Routed to `primary` (priority 10).
2. Upstream answered a genuine 429 (weekly limit) with the full unified
   header set (`7d-utilization: 1.0`, `7d-status: rejected`,
   `7d-reset: 1792584000`, `retry-after: 340298`).
3. The proxy cooled `primary` until the 7d reset (2026-10-21 14:00 CEST —
   exactly what Claude Code itself prints), replayed the same request bytes
   on `secondary`, got a 200, re-pinned the session.
4. `accountant status` showed: primary `100.0% used, cooling until
   2026-10-21 14:00:00`; secondary `eligible, 1 session pinned`.

The user-visible result was a normal answer with no action taken.

## Streaming

SSE streams pass through chunked and flushed per read; a 114 s Opus
thinking stream completed without truncation (server write timeouts are
disabled per design).

## Logs

Daemon log contains only: `session pinned`, `request served`,
`session re-pinned after failover`, `account cooling`, `failing over`,
with hashed session IDs. Zero occurrences of `sk-ant` or `ak_` strings
(grep-verified), matching the automated log-scan test.

## Documented feature losses (observed)

- claude.ai connectors disabled (Claude Code warns at startup).
- "API Usage Billing" header and no `/login` account display.
- Remote Control unavailable (not exercised; Anthropic disables it on
  non-Anthropic hosts per the gateway docs).

## launchd

`accountant service install` wrote
`~/Library/LaunchAgents/com.accountant.agent.plist` (0600) and bootstrapped
the agent. Verified: serves traffic, and `KeepAlive` restarts the daemon
within seconds after a kill.

## Follow-ups for the owner

- `tool_reference` forwarding with `ENABLE_TOOL_SEARCH=true` is
  code-verified (byte pass-through) but deserves one real session with
  deferred MCP tools loaded in your normal project setup.
- A natural 5-hour-window exhaustion rollover will exercise the same path
  as the weekly one; no further action needed.
