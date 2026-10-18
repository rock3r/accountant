# Accountant: multi-subscription rollover for Claude (design)

Status: design only. No code exists yet. A separate implementer builds from this file.
Date: 2026-10-17. Target reader: the implementing engineer or model.
Review: six adversarial rounds by `kimi-code/k3-256k` through Pioneer on 2026-10-17.
Final reviewer verdict: SATISFIED (no blocker or high-severity findings remain).

## Goal

One person owns several Claude subscriptions. Today, when one subscription hits its
5-hour or weekly usage limit, work stops until that person switches accounts by hand.
Accountant removes that stop. It holds one long-lived token per subscription, runs one
local proxy, and moves work to the next subscription when the current one runs out.
Claude Code and third-party (3P) Anthropic-format apps use the proxy in version 1 (v1).
Claude Desktop joins in a later, gated phase. The design keeps the core provider-neutral
so OpenAI Codex subscriptions can be added later without a rewrite.

Decisions the owner already made:

- Start as a local proof of concept (PoC) on one Mac. Do not design into a corner: a
  later phase may serve every machine on the home local area network (LAN).
- Rollover policy: priority order. Use account 1 until it runs out, then account 2.
- Claude Desktop is a later phase (phase 5 in Steps), behind a feasibility spike.
  It is not a v1 requirement.
- Claude credentials are long-lived tokens minted with `claude setup-token`. No
  credential file swapping, no browser login automation.

## Success criteria

V1 is done when all of these hold on the owner's Mac:

1. Two or more Claude subscriptions are registered, each as a `claude setup-token`
   long-lived token in the vault. No token sits in a plain-text config file.
2. Claude Code runs a normal interactive session through the proxy
   (`ANTHROPIC_BASE_URL` on loopback) with tools, subagents, streaming, and compaction
   working, per spike S2's checklist.
3. Each Claude Code session is pinned to one account. Two parallel sessions can sit
   on two different accounts at the same time, for example after one of them fails
   over (two brand-new sessions both start on account 1; that is the policy).
4. When the pinned account returns a quota-exhausted response before any response
   bytes reach the client, the same request succeeds on the next account with no
   action by the user, and the session re-pins to that account.
5. When every account is exhausted, the client sees a true upstream quota error
   unchanged (from the zero-eligible probe below), so Claude Code shows its normal
   limit message with the right reset time.
6. `accountant status` shows, per account: 5-hour and weekly use, reset times,
   cooldown state, and how many sessions are pinned.
7. A 3P app that speaks the Anthropic Messages format works against the same proxy
   with a client key; clients that are not Claude Code use a `claude_code_compat`
   key so their requests pass the system-prompt gate (S1 follow-up).
8. The fake-upstream test suite in the validation plan passes. Request bodies pass
   through byte-identical in every test.

## Facts this design rests on

Each fact says how it was checked. "Official doc" means an Anthropic page read on
2026-10-17; URLs are in Sources. "Local check" means a command run on the owner's Mac.

- `claude setup-token` mints a one-year OAuth token for a subscription, prints it once,
  and saves it nowhere. The user sets it as `CLAUDE_CODE_OAUTH_TOKEN`. It can only make
  model requests: no Remote Control, no claude.ai connectors. (Official doc:
  authentication; local check: `claude setup-token --help` on Claude Code 2.1.302.)
- Credential precedence in Claude Code puts `ANTHROPIC_AUTH_TOKEN` and
  `ANTHROPIC_API_KEY` above `CLAUDE_CODE_OAUTH_TOKEN`, and that token above the stored
  `/login`. A set `CLAUDE_CODE_OAUTH_TOKEN` stays fixed for the whole session, so
  per-session rollover cannot rely on changing it mid-session. Rollover must happen in
  the proxy. (Official doc: authentication, env-vars.)
- Claude Code supports `ANTHROPIC_BASE_URL` proxies officially. On a non-Anthropic
  host, Remote Control is disabled and MCP tool search defaults off; set
  `ENABLE_TOOL_SEARCH=true` when the proxy forwards `tool_reference` blocks.
  (Official doc: env-vars.)
- Claude Code sends an `x-claude-code-session-id` request header on gateway traffic.
  The proxy can pin per session without reading request bodies. It also reads
  `anthropic-ratelimit-unified-*` response headers and `retry-after` to show limits
  and to time retries, so the proxy must forward both unchanged. (Official doc:
  llm-gateway-protocol.)
- The installed Claude Code binary contains the header names
  `anthropic-ratelimit-unified-5h-utilization`, `-5h-reset`, `-7d-utilization`,
  `-7d-reset`, and `-status`. Subscription quota state is therefore readable from
  normal responses, with no extra endpoint. (Local check: `strings` on the 2.1.302
  binary.)
- An undocumented quota endpoint exists: `GET https://api.anthropic.com/api/oauth/usage`
  with a Bearer token and header `anthropic-beta: oauth-2025-04-20`. The open-source
  CodexBar app calls it with full login tokens, and its code handles a `user:profile`
  scope rejection. Whether an inference-only `setup-token` may call it is unknown.
  Spike S1 answers this. The design must work with headers alone. (Inspected source:
  CodexBar `ClaudeOAuthUsageFetcher.swift`.)
- Claude Desktop does not read `ANTHROPIC_BASE_URL` or `apiKeyHelper`. It routes
  through a gateway only in its third-party inference mode (`inferenceProvider:
  gateway`, `inferenceGatewayBaseUrl`, `inferenceGatewayApiKey`), set by an
  administrator profile or locally under Developer → Configure Third-Party Inference.
  The gateway must serve `POST /v1/messages`; `GET /v1/models` is optional. That mode
  replaces native features such as Remote Control and Anthropic cloud environments in
  that app profile. (Official doc: llm-gateway-connect, Claude Desktop gateway.)
- Reference project `caam` (coding_agent_account_manager) switches accounts by
  swapping stored credential files, and warns that the Codex app-server daemon caches
  `auth.json` in memory, so a file swap does not affect a running daemon. Accountant
  takes the warning, not the mechanism. (Inspected: caam README.)
- Reference project CliGate runs a local proxy with account pools, per-account quota
  caches, and client keys, but also ships an assistant, chat channels, a dashboard,
  protocol translation, and many providers. Accountant keeps the pool, quota cache,
  and client keys, and cuts the rest (see Non-goals). (Inspected: CliGate README and
  `docs/ACCOUNTS.md` on branch `master`.)

## Approach

One Go binary, `accountant`, with four parts. Go gives one static binary, a strong
standard HTTP and Server-Sent Events (SSE) story, and easy `launchd` or `systemd`
service installs later. A Node or Electron stack (CliGate-style) is heavier than this
job needs; Rust slows the implementer for no gain here.

1. **Vault** — stores one credential per account and hands it only to the proxy.
2. **Proxy** — an Anthropic-format pass-through on loopback. It swaps the client's key
   for the chosen account's token and streams bytes both ways without re-encoding.
3. **Router** — picks an account per session, pins it, tracks quota from response
   headers, cools exhausted accounts down, and fails over when safe.
4. **CLI** — `serve`, `account add|list|remove|test`, `client add|list|remove`,
   `env`, `status`, `doctor`. Nothing else.

Accountant never translates protocols and never rewrites bodies. That single rule
keeps prompt caching, tool search, beta features, and error wording intact, and it is
the main simplification over CliGate.

### How a request flows

1. Claude Code sends `POST /v1/messages` to `http://127.0.0.1:8787` with a client key
   in `Authorization: Bearer` (from `ANTHROPIC_AUTH_TOKEN`) or `x-api-key`.
2. The proxy checks the client key — `x-api-key` if that header is present, else
   the Bearer token; if the chosen one is invalid the request gets a 401, with no
   fall-through to the other header. It reads `x-claude-code-session-id` and asks
   the router for that session's account.
3. The provider's `RewriteRequest` deletes the inbound `Authorization` and
   `x-api-key` headers without exception, sets `Authorization: Bearer <account
   token>`, and merges `oauth-2025-04-20` into `anthropic-beta`: parse that header
   as comma-separated tokens across all its lines, match case-insensitively, and
   append the token only if it is absent as a whole token. Never drop client
   values. The proxy then forwards method, path, query, remaining headers, and the
   body byte-for-byte to `https://api.anthropic.com`. No other auth header is added
   or removed.
4. Response headers and body stream back unchanged, flushed chunk by chunk. The router
   passes the response to the provider's `QuotaFromResponse` and records the returned
   snapshot against the account.
5. On a quota-exhausted or auth-broken response with zero body bytes sent to the
   client so far, the router marks the account accordingly, re-pins the session, and
   replays the same request bytes on the next eligible account. If bytes already
   flowed, the response completes as-is and only the next request moves. One bound
   covers every case: within one client request, each account is tried at most once;
   quota and auth-broken verdicts advance to the next eligible account; transient
   verdicts never advance.

### Accounts and the vault

- An account is `{id, provider, label, priority, enabled, credential_ref}`. The config
  file holds this metadata only. It never holds a token.
- The vault sits behind one interface (`Get`, `Put`, `Delete` by ref). V1 ships one
  real backend: the macOS Keychain (one generic-password item per account, service
  `accountant`). Tests use an in-memory backend. An encrypted file backend for hosts
  without a Keychain belongs to the LAN phase; building it now would add a second
  backend to test for no v1 gain. The interface is what keeps that phase possible.
- `accountant account add claude --label work` prints exact steps: log in as that
  subscription under a scratch `CLAUDE_CONFIG_DIR`, run `claude setup-token`, then
  paste the token. The command reads the token from standard input with echo off,
  never from a flag. It then validates the token (spike S1 picks the cheapest call
  that works; default: `POST /v1/messages/count_tokens`, else a one-token
  `POST /v1/messages` on the cheapest model) and stores it.
- Tokens last one year. `accountant status` warns from 30 days before the recorded
  mint date plus one year. There is no refresh flow; the fix is a new `setup-token`.

### Routing rules (exact)

- **Session key**, in order: `x-claude-code-session-id` header; else the client key
  ID (every routed request carries a valid client key, so this always resolves). A
  request without the session header deliberately follows its client-key pin, even
  mid-session; spike S2 records which paths Claude Code sends the header on, so this
  stays a measured choice. Pin records are `{provider, session_key, account_id,
  last_seen}` in the state file, keyed by `(provider, session_key)`, so a daemon
  restart keeps them and a future provider cannot collide with these pins. A pin
  expires after 24 hours without a request.
- **Selection** considers only enabled accounts of the request's provider. Eligible
  means not cooling, not auth-broken, and last known 5-hour and weekly utilization
  both below 100%. Unknown utilization counts as eligible, and a utilization sample
  counts as unknown once its own window's `reset_at` is in the past — otherwise a
  recorded 100% would wedge the account forever after its reset, since an account
  with no traffic never gets a fresh sample. A new session takes the eligible
  account with the lowest priority number.
- **Every request re-checks its pin.** If the pinned account is no longer eligible,
  run selection again and re-pin (and log) before any upstream call. Otherwise use
  the pin. Never send a request to an account already known to be out — with the
  single zero-eligible probe below as the only exception. A recovered account
  attracts only new sessions: existing pins never migrate on recovery, because a
  move costs a prompt-cache rebuild and would flap.
- **Quota exhausted** means any HTTP 429 on a served endpoint. Unified headers only
  refine the cooldown: cool until the `*-reset` time of the window that reads
  exhausted — the later of the two when both do; if no window reads exhausted (a
  per-minute limit or an unrecorded
  header shape), treat the reset headers as absent and continue down this chain —
  until now plus `retry-after` (parsed as delta-seconds or HTTP-date), else
  5 minutes. A 2xx response whose headers read 100% also marks the account
  ineligible for later requests, without disturbing the current one.
- **Auth broken**: HTTP 401 always marks the account auth-broken. HTTP 403 does so
  only when the buffered error prefix parses as an Anthropic authentication or
  permission error (`error.type` of `authentication_error` or `permission_error`);
  any other 403 — for example a Cloudflare or web application firewall (WAF) edge
  block, which is not a credential failure — is Transient. An auth-broken account
  leaves rotation until `accountant account test` passes again.
- **Transient errors** (HTTP 5xx, 529 overloaded, network errors): pass through, no
  cooldown, no failover. Claude Code already retries these, and a proxy retry could
  double-spend quota on an ambiguous result. Any status not otherwise classified
  (400, 404, 413 from upstream, anything new) is Transient by default.
- **No enabled accounts** (none configured, or all disabled): return a local 503
  with an Anthropic-shaped error body (`error.type` `overloaded_error`) without
  contacting upstream. `accountant doctor` flags this state as an error.
- **Zero eligible accounts** (Selection yields no eligible account before this
  request has contacted upstream at all — whether accounts are out through
  cooldown, fresh 100% samples, or both): probe once. Forward the request to the
  enabled, non-auth-broken account with the earliest recovery time, where recovery
  time is `cooling_until` if set and in the future, else the earliest future
  `reset_at` among its fresh exhausted windows, else now; ties go to the lowest
  priority number. If every enabled account is auth-broken, use the enabled account
  with the lowest priority number. This probe is the one exemption from the
  never-send rule — cooldown data can be stale, and the client deserves an
  authoritative answer. Pass the probe response through unchanged. A probe
  response is classified and mutates account state exactly like any other response
  (a 429 sets cooldown from its headers, a 401 or qualifying 403 sets auth-broken,
  samples refresh). The only special probe behaviors are: no further failover
  within this request, and a 2xx clears cooldown and pins the session. This is
  distinct from running out
  mid-request: if attempts in this request already failed over one or more accounts
  and none remain, return the last failure response unchanged; no probe runs.
- **Mid-stream errors** (including in-stream SSE `error` events after a 200) always
  pass through unclassified: no cooldown, no re-pin. Bytes already sent cannot be
  taken back, and guessing at stream content would break the pass-through rule.
- **Concurrency**: one mutex guards account selection, re-pinning, quota updates,
  and state persistence. Re-pin is compare-and-swap on the session's current
  account, so two concurrent requests of one session that both fail over converge
  on the same next account instead of splitting. Credential refresh (future
  providers) is single-flight per account, with the new credential written to the
  vault before any request uses it.
- **Mid-session re-pin cost**: prompt cache lives upstream per account, so the first
  request on the new account reprocesses the full context and charges it there. This
  is accepted: it happens only when the alternative is a hard stop. Log every re-pin.

### Quota state

Response headers are the source of truth. Per account the state file keeps
`five_hour: {utilization, reset_at}`, `seven_day: {utilization, reset_at}`,
`status`, `cooling_until`, `auth_broken`, `updated_at`. The reset-time parser
accepts both Unix epoch seconds and RFC 3339 (spike S1 records which one upstream
sends) and records the local receipt time with each sample. `cooling_until` is
clamped: a reset in the past means immediately eligible, and no cooldown runs
longer than 8 days, so a misparsed value or a skewed clock cannot strand an
account. Utilization is normalized to a fraction before any comparison, using the
`utilization_scale` config field that spike S1 sets for every value: `percent`
divides all values by 100; `fraction` (the default) uses values as-is, except that
any value above 1 is still divided by 100, as a guard against an upstream scale
change. With no S1 result recorded yet, the default reads an exact `1` as
exhausted: the safe direction. Eligibility is always "fraction below 1.0". A
successful (2xx) response from an account clears its `cooling_until` at once;
`auth_broken` is never cleared by traffic, only by `accountant account test`. If
spike S1 shows that
`setup-token` tokens may call `GET /api/oauth/usage`, add an optional poller (default
off, at most once per 5 minutes per account, with its own 429 backoff — that endpoint
rate-limits too). Correctness must never depend on the poller.

### Client setup for v1

- Claude Code: set `ANTHROPIC_BASE_URL=http://127.0.0.1:8787`,
  `ANTHROPIC_AUTH_TOKEN=<client key>`, `ENABLE_TOOL_SEARCH=true`. Document both the
  shell-export form and the `~/.claude/settings.json` `env` block. `accountant env`
  prints the exports. Documented losses in this mode: no Remote Control, no
  claude.ai connectors, no `/login` account display. Local MCP servers still work.
- 3P Anthropic-format apps: same base URL; client key as `x-api-key` or Bearer.
  Clients that are not Claude Code must be created with
  `accountant client add ID --claude-code-compat` so the proxy prepends Claude
  Code's first system block to their requests and sanitizes their system-prompt
  text (the gate and the third-party classifier from S1). The flag is
  per client key and can be flipped with `accountant client update` without
  rotating the key. Compat shaping is idempotent: a body that already carries
  the Claude Code system block is forwarded byte-identical.
- Client keys: `accountant` generates one per client or machine, stores only a
  SHA-256 hash, and checks in constant time. Keys exist even on loopback, so the LAN
  phase needs no new auth design.

### Proxy contract (exact)

- Serve: `POST /v1/messages`, `POST /v1/messages/count_tokens`, `GET /v1/models`
  (forward upstream), `GET /healthz` (local, no auth). Reject other paths with 404.
  Mount the same handlers under `/anthropic/` as well. The Codex provider owns
  `/codex/`: `POST /codex/responses` and `GET /codex/models` (S4); other `/codex/`
  paths 404.
- Forward unchanged: body bytes, `anthropic-version`, all `x-claude-code-*` headers,
  and `Accept-Encoding`. The core proxy also forwards `anthropic-beta` untouched;
  the one merge in flow step 3 happens inside the provider's `RewriteRequest` (it
  is an auth-related adjustment there, and only there). Strip hop-by-hop headers.
  Change only the credential headers, exactly as flow step 3 says. Never parse or
  re-serialize the JSON body — with one exception: for clients flagged
  `claude_code_compat`, the provider's `RequestShaper` prepends Claude Code's
  first system block to the body (idempotent; already-shaped bodies stay
  byte-identical). Set `DisableCompression: true`
  on the upstream `http.Transport`, so Go never adds its own `gzip` handling or
  transparently decodes a response and mutates its bytes.
- Streaming: no full-body buffering on the response path for 2xx responses; flush
  per read. For a non-2xx response, first read a bounded prefix (64 KiB, 30-second
  deadline) without sending anything to the client; that prefix feeds `Classify`
  and keeps the failover window open, since error bodies are small. If the deadline
  passes, treat the response as Transient, and clear that read deadline before
  writing anything to the client. On any pass-through verdict (Transient,
  or a final QuotaExhausted or AuthBroken with no account left to try), write the
  buffered prefix to the client verbatim first, then stream the remainder — the
  client must receive the complete, unmodified error body. On failover, drain the
  failed response body (bounded at 1 MiB) and close it before replaying, so
  upstream connections stay reusable.
- Timeouts, in Go terms: `http.Server` `ReadTimeout` and `WriteTimeout` stay zero
  (disabled) — a write timeout would kill long SSE streams mid-generation. A short
  `ReadHeaderTimeout` (30 seconds) is required. The upstream `http.Transport` sets
  `ResponseHeaderTimeout` from the `response_header_timeout_seconds` config field
  (default 120, set from time-to-first-byte evidence in spike S2): generous enough
  for long-thinking first tokens, finite enough that an upstream which accepts the
  connection and never
  sends headers cannot hang a session forever. On that timeout, abort the upstream
  body, classify as Transient, return a local 504 with an Anthropic-shaped error
  body (`error.type` `api_error`), and never advance to another account — the
  client retries, as with any Transient. Stream liveness otherwise comes from the
  request context: a client disconnect cancels the upstream call and any in-flight
  failover attempt at once.
- Buffer the request body (bounded, default 32 MiB) only so a pre-byte failover can
  replay it; over the bound, fail the request with 413 before contacting upstream.
- Logs carry metadata only: time, hashed session ID, account ID, status, duration,
  re-pin events. Never log credentials or bodies. (The model name lives only in the
  body, so v1 logs do not carry it.)

### The provider seam (Codex implemented; see docs/s4-results.md)

The router, vault, quota state, session pins, and client keys must not know provider
details. They depend on this provider interface only:

```go
type Provider interface {
    ID() string // "anthropic"
    // Constructor-injected per instance: the production default for a
    // provider, a fake httptest server in tests.
    UpstreamBaseURL() string
    // Inject the account credential; adjust only auth-related headers.
    // For Anthropic that includes the anthropic-beta oauth merge.
    RewriteRequest(req *http.Request, cred []byte) error
    // Extract the session key, or "" when the client sends none.
    SessionKey(req *http.Request) string
    // Read quota windows from response headers.
    QuotaFromResponse(resp *http.Response) (QuotaSnapshot, bool)
    // Bucket a finished response: OK, QuotaExhausted{ResetAt},
    // AuthBroken, Transient. bodyPrefix is the bounded non-2xx prefix the
    // proxy read before sending anything (empty for 2xx), for providers
    // that signal exhaustion in the body.
    Classify(resp *http.Response, bodyPrefix []byte) Verdict
    // Cheapest credential check available.
    Validate(ctx context.Context, cred []byte) error
}

// Only for providers whose credentials expire. Claude setup-tokens do not
// implement this; Codex OAuth does (spike S4).
type Refresher interface {
    Refresh(ctx context.Context, cred []byte) (newCred []byte, err error)
}

// Added with the Codex provider: proactive expiry. The proxy refreshes
// single-flight per account when the expiry is near (30 min), writes the
// new credential to the vault before any request uses it, and treats a
// permanent refresh failure as auth-broken without contacting upstream.
type CredentialExpirer interface {
    CredentialExpiry(cred []byte) (time.Time, bool)
}
```

Codex facts are verified in `docs/s4-results.md` (spike S4): OAuth token
triple imported from a Codex CLI `auth.json`, `Authorization: Bearer` +
`ChatGPT-Account-ID` request auth, `https://chatgpt.com/backend-api/codex`
upstream, `x-codex-*` percent-scale quota headers mapped by window length,
429 `usage_limit_reached` exhaustion, rotating refresh tokens against
`auth.openai.com/oauth/token`, and the custom-`model_providers` client
setup (`chatgpt_base_url` does not carry model traffic). The Codex CLI's
app-server auth cache (the caam warning) is irrelevant here: the CLI holds
no subscription tokens at all in the proxied setup.

### Files and config

XDG-style paths on every OS, so nothing macOS-specific leaks into the core:
`~/.config/accountant/config.toml` and `~/.local/state/accountant/state.json`
(directory `0700`, files `0600`). Every write to either file is atomic (temp file
plus rename, then set `0600`). One lock file (`state.lock`, taken with `flock`)
guards every read-modify-write of state and config, by the daemon and by CLI
commands alike, so `account add`, `account test` clearing `auth_broken`, and the
daemon's own quota updates cannot interleave. The daemon stats both files before
each routing decision and reloads if the modification time changed, so CLI edits
take effect without a restart. Config example:

```toml
listen = "127.0.0.1:8787"        # LAN phase changes this; nothing else hardcodes it
default_provider = "anthropic"
request_body_limit_mib = 32
response_header_timeout_seconds = 120  # default set from spike S2 first-byte times
utilization_scale = "fraction"   # set from spike S1; resolves the exact value 1

[[accounts]]
id = "claude-work"
provider = "anthropic"
label = "Work Max"
priority = 10                    # lower runs first
minted_at = "2026-10-17"         # drives the expiry warning

[[clients]]
id = "macbook"
key_sha256 = "..."               # secret shown once at creation
```

### LAN-ready, without building the LAN phase now

Bind address, paths, and ports come from config. Client keys exist from day one. The
vault and state sit behind interfaces. No macOS call lives outside the vault and
service adapters. The later LAN phase then only adds: a non-loopback bind behind
Transport Layer Security (TLS) or a tailnet, key rotation, and one shared daemon.
Do not build any of that in v1.

## Non-goals

- No protocol translation (no OpenAI-format front door), no model name mapping.
- No dashboard, web UI, assistant, chat channels, scheduling, or cost reporting.
- No credential file swapping, HOME forgery, or browser login automation.
- No sharing accounts between people, no multi-user support, no resale features.
- No usage-endpoint dependency, no scraping of claude.ai web pages.
- No Claude Desktop support in v1; no Codex code in v1 (interface plus spike only).

## Spikes (run before the phase they gate)

- **S1 — setup-token capabilities** (gates phase 1). With one real `setup-token`:
  call `POST /v1/messages`, `POST /v1/messages/count_tokens`, `GET /v1/models`, and
  `GET /api/oauth/usage` from `curl`, with Bearer auth and the beta header. Record
  status codes, which unified headers come back, the reset-time format, the
  utilization scale, and what a quota 429 body and header set look like. Exit: a
  table of what works. If `/v1/messages` fails outside Claude Code, stop; the design
  needs rework.
- **S2 — Claude Code through a pass-through** (gates phase 3 sign-off). Point Claude
  Code at the phase 2 proxy and check: a long tool-using session, a subagent, MCP
  tool search with `ENABLE_TOOL_SEARCH=true`, `/status` and `/usage` display, a forced
  429 (point one account at an exhausted token or a fake upstream), one compaction,
  which request paths carry the `x-claude-code-session-id` header, and observed
  time to first byte on long-thinking requests (sets the response-header timeout
  default). Exit: checklist
  with pass/fail each; failures become phase 4 fixes or documented losses.
- **S3 — Claude Desktop 3P mode** (gates phase 5). Configure third-party inference
  locally against the proxy; check Chat, Cowork, and Code sessions, model discovery,
  and what native features disappear. Exit: go/no-go for pooling Desktop.
- **S4 — Codex pass-through** (gates phase 7). Test `chatgpt_base_url` against a
  recording proxy; find the session signal and quota headers, and settle the
  single-writer refresh rule versus the CLI and its daemon. Exit: a provider sketch
  filled into the interface above.

## Steps

1. **Phase 0**: run spike S1; write results into `docs/s1-results.md`. Adjust the
   `Validate` call and the poller decision from evidence, not hope.
2. **Phase 1 — core**: module layout (`cmd/accountant`, `internal/{config,vault,
   state,router,proxy,provider/anthropic}`), config load, the Keychain vault (plus
   the in-memory vault for tests), `account add|list|remove|test`, and locked,
   atomic state and config writes.
3. **Phase 2 — proxy and router**: the proxy contract and routing rules above, the
   Anthropic provider, session pins, quota capture, failover, `serve`.
4. **Phase 3 — client integration**: `accountant env`, client key management,
   `status`, `doctor` (file permissions, token test per account, upstream reach),
   `launchd` user-service install and uninstall, README with the Claude Code setup
   and the known losses. Run spike S2 and fix what it finds.
5. **Phase 4 — hardening**: complete the validation plan, run a real week of use,
   tune only from logged evidence.
6. **Later phases, in this order**: phase 5 Desktop (after S3); phase 6 LAN serving;
   phase 7 Codex provider (after S4). Each starts only on an explicit go decision.

## Validation plan

Automated, against a scripted fake upstream (`httptest`), all with a fake clock:

- Pass-through: request and response bodies hash-identical in and out; beta-header
  merge adds once, never duplicates, never drops; credential headers replaced;
  hop-by-hop headers stripped; unified response headers forwarded unchanged.
- Routing: priority order on new sessions; pin stickiness across requests and across
  a daemon restart; parallel sessions on different accounts with account 1
  pre-cooled (the setup that makes them differ); no pin migration when account 1
  recovers; pin expiry after 24h.
- Failover: quota 429 before bytes replays on the next account and re-pins; a bare
  429 with no unified headers also fails over and cools for 5 minutes; a 429 whose
  headers show no exhausted window cools by its `retry-after`, not by a window
  reset; 429 after
  bytes does not replay; an in-stream SSE `error` event after a 200 passes through
  with no cooldown and no re-pin; 401 marks auth-broken and fails over; a 403 with
  an Anthropic authentication prefix does the same while an edge-block 403 passes
  through as Transient; an upstream that never sends headers yields the local 504
  with no account advance; 500 passes
  through with no cooldown and its full error body intact (prefix plus remainder,
  hash-identical to the fake upstream's body); mid-request exhaustion returns the
  last failure response unchanged; zero-eligible at arrival runs exactly one probe
  to the earliest-recovering account and passes its response through, a 2xx probe
  clears the cooldown and pins, and a 401 probe marks that account auth-broken,
  passes through unchanged, and triggers no further failover; each account is tried
  at most once per
  request; cooldown ends at the `*-reset` time, and once the fake clock passes that
  reset the stale 100% sample no longer blocks the account, so a new session takes
  the priority account again with no probe involved; two concurrent requests of one
  session that both fail over end pinned to the same account, and the state file
  stays valid under parallel load (`-race` clean).
- Limits and safety: body over the limit gets 413 with no upstream call; bad client
  key gets 401; unknown path gets 404; no enabled accounts gets the local 503 with
  no upstream call; a token string never appears in any log line (test scans logs
  for the fixture tokens).
- Commands: `go build ./...`, `go vet ./...`, `go test ./...`, then `golangci-lint
  run` with the repo config. All must pass clean.

Manual: the S2 checklist on the real Mac, plus one forced real-world rollover:
exhaust or disable account 1 mid-session and watch the session continue on account 2
while `accountant status` shows the re-pin. This manual check is the highest-risk
validation step, because it is the only one that proves the client's real headers and
the upstream's real 429 shape match this design.

## Risks and open questions

- **Setup-token scope.** The usage endpoint may reject inference-only tokens (S1).
  Impact if so: quota display relies on headers only, which the design already
  treats as the baseline. No rework needed either way.
- **Upstream drift.** Header names, the 429 shape, and beta flags can change with
  Claude Code releases. Mitigation: unknown shapes degrade to "eligible, pass
  through", never to a hard failure; `doctor` re-runs S1's calls on demand.
- **Feature losses behind a proxy** are real (Remote Control, claude.ai connectors,
  some account UI). They are listed in client setup so the owner accepts them with
  open eyes. A native `/login` session outside the proxy stays available as an escape
  hatch.
- **Cache cost on re-pin.** A moved session pays full input cost once on the new
  account. With long contexts this is a real quota chunk. Priority-order pinning makes
  it rare. There is no correct way to avoid it.
- Open questions: the exact utilization scale in unified headers (0–1 or 0–100; S1
  records it into `utilization_scale`, and the normalization rule in Quota state
  covers every other value); whether `/usage` inside Claude Code
  stays meaningful behind the proxy (S2); whether Desktop 3P mode is acceptable day
  to day (S3); whether the Codex apps expose any base-URL surface at all (S4).
- **Gate drift.** Compat clients depend on Anthropic's system-prompt gate and
  third-party classifier keeping their current shape. If either tightens,
  compat clients degrade to upstream 429s/400s (both classified Transient —
  no quota cooldown, see S1); Claude Code traffic is unaffected because it is
  shaped correctly by the client itself.

## Sources

- [Claude Code authentication](https://code.claude.com/docs/en/authentication)
- [Claude Code environment variables](https://code.claude.com/docs/en/env-vars)
- [Connect Claude Code to an LLM gateway](https://code.claude.com/docs/en/llm-gateway-connect)
- [Claude Code gateway compatibility guide](https://code.claude.com/docs/en/llm-gateway-protocol)
- [Claude Desktop 3P gateway](https://claude.com/docs/third-party/claude-desktop/gateway)
- [CliGate](https://github.com/codeking-ai/cligate) — README and `docs/ACCOUNTS.md`
- [coding_agent_account_manager](https://github.com/Dicklesworthstone/coding_agent_account_manager) — README
- [CodexBar](https://github.com/steipete/CodexBar) — `ClaudeOAuthUsageFetcher.swift`
- Local checks on 2026-10-17: Claude Code 2.1.302 help output and binary header
  strings; `codex-cli` 0.163.0 help and `~/.codex/auth.json` key names (no values);
  presence of `/Applications/Claude.app` and `~/Library/Application Support/Claude-3p/`.
