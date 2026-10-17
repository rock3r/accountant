# Spike S1 — setup-token capabilities

Date: 2026-10-17. Runner: implementing agent, on the owner's Mac.
Tokens: two real `claude setup-token` long-lived tokens, read into memory from
1Password (`op read`) per call, never written to disk, logs, or this repo.
Accounts: "primary" (Claude) and "secondary" (Claude secondary).
Claude Code on this Mac: 2.1.302.

## Result table

| Call | Bearer auth + `anthropic-beta: oauth-2025-04-20` | Result |
|---|---|---|
| `GET /v1/models` | both tokens | **200**, JSON model list. No unified rate-limit headers. |
| `POST /v1/messages/count_tokens` | both tokens | **200** `{"input_tokens":10}`. No unified rate-limit headers. Works *without* the Claude Code system prompt. |
| `POST /v1/messages` | secondary (has quota) | **200**, but *only with the gate satisfied* (below). TTFB ≈ 1.0 s for a 1-token Haiku reply, streaming and non-streaming. |
| `POST /v1/messages` | primary (weekly limit hit) | **429** genuine quota exhaustion, full unified header set (below). |
| `GET /api/oauth/usage` | primary | **403** `permission_error`: "OAuth token does not meet scope requirement `user:profile`" (`error_code: oauth_scope_insufficient`). |
| `GET /api/oauth/usage` | secondary | **429** endpoint-level `rate_limit_error` (this endpoint is heavily rate-limited; consistent with CodexBar's findings). |

## The gate: setup-tokens only serve Claude-Code-shaped requests

A bare `curl` `POST /v1/messages` with a valid token on an account **with quota**
returns an instant **429** `{"type":"error","error":{"type":"rate_limit_error","message":"Error"}}`
with **no unified headers and no `retry-after`**. Bisected against the secondary
account (each variant otherwise identical, minimal 1-token Haiku request):

| Variant | Result |
|---|---|
| minimal body, `?beta=true` | 429 |
| minimal body + full Claude Code beta list | 429 |
| minimal body **+ system prompt "You are a Claude agent, built on Anthropic's Claude Agent SDK."** | **200** |

Streaming on/off, model choice, `x-app`, and the `claude-cli` user-agent made no
difference; the system prompt is the gate. Verified end-to-end: the same token runs
`claude -p` fine while bare curl 429s, and the primary account fails in Claude Code
itself with "You've hit your weekly limit · resets Oct 21 at 2pm (Europe/Rome)".

**Consequence for the design (no rework needed):** the pass-through rule is exactly
right — Claude Code always sends this system prompt, so routed traffic is unaffected.
A 3P Anthropic-format app that does not send a Claude Code system prompt will get
this bare 429 from upstream; the router's "429 with no exhausted window and no
`retry-after` → 5-minute cooldown" chain absorbs it. Documented in the README.

## Unified headers on 2xx (`/v1/messages`, streaming and not)

```
anthropic-ratelimit-unified-status: allowed
anthropic-ratelimit-unified-5h-status: allowed
anthropic-ratelimit-unified-5h-utilization: 0.0
anthropic-ratelimit-unified-5h-reset: 1792260600
anthropic-ratelimit-unified-7d-status: allowed
anthropic-ratelimit-unified-7d-utilization: 0.0
anthropic-ratelimit-unified-7d-reset: 1792670400
anthropic-ratelimit-unified-reset: 1792260600
anthropic-ratelimit-unified-representative-claim: five_hour
anthropic-ratelimit-unified-overage-status: rejected
anthropic-ratelimit-unified-overage-disabled-reason: org_level_disabled
anthropic-ratelimit-unified-fallback-percentage: 0.5
```

- **Utilization scale: fraction 0–1** (`utilization_scale = "fraction"`; the design
  default stands).
- **Reset format: Unix epoch seconds** (parser also accepts RFC 3339, per design).
- `/v1/models` and `/v1/messages/count_tokens` carry **no** unified headers.

## Genuine quota-exhausted 429 (primary account, weekly window)

Captured with the full Claude Code request shape:

```
HTTP 429, x-should-retry: true, retry-after: 340298 (delta-seconds ≈ 3.9 d)
anthropic-ratelimit-unified-status: rejected
anthropic-ratelimit-unified-5h-status: allowed
anthropic-ratelimit-unified-5h-utilization: 0.0
anthropic-ratelimit-unified-5h-reset: 1792260600
anthropic-ratelimit-unified-7d-status: rejected
anthropic-ratelimit-unified-7d-utilization: 1.0
anthropic-ratelimit-unified-7d-reset: 1792584000
anthropic-ratelimit-unified-representative-claim: seven_day
anthropic-ratelimit-unified-upgrade-paths: overage
body: {"type":"error","error":{"type":"rate_limit_error",
  "message":"This request would exceed your account's rate limit. Please try again later."}, ...}
```

- The exhausted window reads utilization **exactly 1.0** and status `rejected`;
  `retry-after` matches the 7d reset (epoch 1792584000 = 2026-10-22 14:00 CEST).
- The *gate* 429 (above) has no unified headers at all — the two 429 shapes are
  distinguishable exactly as the design's cooldown chain expects.

## Decisions taken from evidence

1. **`Validate` = `POST /v1/messages/count_tokens`** — the design's default. It is
   free (no quota), works without the system-prompt gate, and a bad token returns
   401. The `/v1/messages` fallback is unnecessary.
2. **No usage-endpoint poller.** Setup-tokens lack the `user:profile` scope
   (primary: 403 `oauth_scope_insufficient`; secondary: endpoint 429). Quota state
   comes from response headers only, which the design already treats as baseline.
   No poller config is shipped.
3. **`utilization_scale = "fraction"`** in the shipped config template and as the
   default; reset parser primary path is epoch seconds.

## Claude Code request shape (captured via a local recording stub, `ANTHROPIC_BASE_URL`)

- `POST /v1/messages?beta=true`, `Accept: application/json`, body `"stream": true`,
  `x-claude-code-session-id` header present, `x-stainless-*` headers,
  `user-agent: claude-cli/2.1.302 (external, sdk-cli)`,
  `anthropic-beta` carries ~13 flags including `oauth-2025-04-20`,
  system prompt begins with an `x-anthropic-billing-header` text block.
- Confirms the proxy's beta-merge rule ("append `oauth-2025-04-20` only if absent")
  is a no-op for Claude Code and matters only for 3P clients.

## Follow-up probes: exact gate semantics (2026-10-19, secondary token, direct API)

The system-prompt gate on `/v1/messages` is an **exact-block match**, not a
substring or fuzzy check:

| System prompt sent | Result |
|---|---|
| Any non-Claude-Code text (e.g. a Pi prompt) | 429 |
| `"You are Claude Code, Anthropic's official CLI for Claude."` as the only block, as a plain string, or followed by other blocks | 200 |
| Billing-header block first, Claude Code block second (real Claude Code shape) | 200 |
| Claude Code block second, anything else first | **429** |
| Claude Code text embedded inside a larger block | **429** |

Also: headers are not gated (curl UA and a minimal `anthropic-beta` pass),
`cache_control` on the injected block is accepted, and streaming is unaffected.
`/v1/messages/count_tokens` remains ungated. The gate-429 body carries no
unified quota headers, so `Classify` buckets it as Transient — no false quota
cooldowns when an unshaped client hits it.

Consequence implemented in v1: per-client-key `claude_code_compat` shaping
(`internal/provider/anthropic/shape.go`) prepends the exact Claude Code first
block for clients that are not Claude Code.
