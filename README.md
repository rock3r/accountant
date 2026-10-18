# accountant

[![ci](https://github.com/rock3r/accountant/actions/workflows/ci.yml/badge.svg)](https://github.com/rock3r/accountant/actions/workflows/ci.yml)

Multi-subscription rollover for Claude and Codex. One local proxy holds one
credential per subscription and moves work to the next subscription when
the current one runs out — no manual account switching.

Status: v1, local proof of concept on one Mac. See `DESIGN.md` for the
binding design and `docs/s1-results.md` for the verified upstream facts.

## Disclaimer

accountant is experimental software, provided as-is, purely for
experimentation, and used entirely at your own risk. Check the terms of
service of your model providers before routing traffic through it. You are
solely responsible for anything you do with it.

## How it works

- One Go binary. A loopback proxy on `127.0.0.1:8787` speaks the Anthropic
  Messages format and passes requests through **byte-identical** — it only
  swaps your client key for the current account's `claude setup-token` token.
- Each Claude Code session is pinned to one account (via the
  `x-claude-code-session-id` header). When the pinned account answers
  quota-exhausted before any response bytes flow, the same request bytes are
  replayed on the next account and the session re-pins. Transient errors
  (5xx, network) never fail over.
- Quota state comes from `anthropic-ratelimit-unified-*` response headers —
  no usage-endpoint dependency (setup-tokens lack the scope for it; see S1).
- Tokens live in the macOS Keychain (service `accountant`). Config holds
  metadata only — never a token. Client keys are stored as SHA-256 hashes.

## Install

```sh
go build ./cmd/accountant
# put the binary on your PATH, e.g.:
cp accountant /opt/homebrew/bin/
```

## Setup

```sh
# 1. Register each subscription (token read from stdin with echo off,
#    validated against the API, stored in the Keychain):
accountant account add claude --label work --priority 10
accountant account add claude --label personal --priority 20

# 2. Create a client key (shown once — store it now):
accountant client add macbook

# 3. Start the service (launchd user agent, restarts on login):
accountant service install

# 4. Check everything:
accountant doctor
accountant status
```

## Connect Claude Code

```sh
export ANTHROPIC_BASE_URL=http://127.0.0.1:8787
export ANTHROPIC_AUTH_TOKEN=<client key from step 2>
export ENABLE_TOOL_SEARCH=true
```

`accountant env` prints the same block. The equivalent `env` block in
`~/.claude/settings.json` works too.

**Known losses behind the proxy** (Anthropic's gateway rules, not ours):
no Remote Control, no claude.ai connectors, no `/login` account display.
Local MCP servers still work. A native `/login` session outside the proxy
(without these env vars) stays available as an escape hatch.

**Other apps (Pi, scripts, …)**: any app that speaks the Anthropic Messages
format can use the same base URL with a client key as `x-api-key` or
`Authorization: Bearer`. Anthropic gates subscription-token inference on
requests carrying the Claude Code system prompt (S1 finding), so clients
that are not Claude Code need a compat key:

```sh
accountant client add pi --claude-code-compat
```

For requests from a compat client, accountant (a) prepends Claude Code's
exact first system block to the request body (idempotently — an
already-shaped body is forwarded byte-identical) and (b) sanitizes the
client's own system-prompt text: standalone "pi"/"Pi" becomes
"agent"/"Agent" and standalone ".md" becomes "markdown", because upstream
otherwise classifies the request as third-party and rejects it (HTTP 400,
"Third-party apps now draw from your extra usage"; S1 follow-up). Words
like "pipeline" and paths like "README.md" are untouched. Note the model
will see the sanitized wording. Headers and everything else pass through
untouched. For Pi, add this to `~/.pi/agent/models.json`:

```json
{
  "providers": {
    "accountant": {
      "baseUrl": "http://127.0.0.1:8787",
      "api": "anthropic-messages",
      "apiKey": "<client key>",
      "models": [
        { "id": "claude-opus-4-6", "name": "Claude Opus 4.6 (accountant)", "reasoning": true,
          "input": ["text", "image"], "contextWindow": 200000, "maxTokens": 64000 },
        { "id": "claude-sonnet-4-6", "name": "Claude Sonnet 4.6 (accountant)", "reasoning": true,
          "input": ["text", "image"], "contextWindow": 200000, "maxTokens": 64000 },
        { "id": "claude-haiku-5-5", "name": "Claude Haiku 5.5 (accountant)", "reasoning": true,
          "input": ["text", "image"], "contextWindow": 200000, "maxTokens": 64000 }
      ]
    }
  }
}
```

Then `/model` and pick `accountant/…`. Keeping it a separate provider
leaves Pi's built-in Anthropic provider untouched as a fallback. The
`!security …` form reads the key from the login Keychain at request time;
a literal key or `$ENV_VAR` work too.

For a **live model catalog** (every model the subscriptions can use,
straight from the proxied `GET /v1/models` and `GET /codex/models`, instead
of the static list above), copy `integrations/pi/accountant.ts` to
`~/.pi/agent/extensions/` and drop the static `accountant` block from
`models.json`. The extension registers one `accountant` provider listing
every model the proxy can serve — Claude models on the Anthropic Messages
API, Codex models with per-model overrides onto the OpenAI Responses API
under `/codex` — fetches both catalogs at startup (a daemon that only
serves one side keeps the other side's cached data), caches them under
`~/.pi/agent/accountant-models-cache.json` for when the daemon is down, and
refreshes them when the model picker reloads. Codex models carry their live
context window and a thinking-level map clamped to the efforts each model
advertises; `compat` flags keep Pi from sending request fields the ChatGPT
backend rejects (`max_output_tokens`, prompt-cache retention).

The extension's last resort is an embedded static catalog, refreshed at
release time against a running daemon:

```sh
node integrations/update-catalog.mjs            # re-embed + write integrations/catalog.json
node integrations/update-catalog.mjs --check    # fail if the committed catalog is stale
```

`integrations/catalog.json` holds the same raw upstream payloads for client
integrations that cannot do live discovery (Amp, others).

## Connect Codex (ChatGPT subscriptions)

Codex subscriptions join the same pool behind `http://127.0.0.1:8787/codex`.
Facts and caveats are verified in `docs/s4-results.md` (spike S4).

```sh
# 1. Log the subscription into a dedicated Codex CLI profile, then import
#    its OAuth tokens (reads auth.json; never modifies it):
CODEX_HOME="$(mktemp -d)/codex-work" codex login
accountant account add codex --label work --priority 10 \
  --from "$CODEX_HOME/auth.json"

# 2. Point Codex CLI at the proxy — in ~/.codex/config.toml:
#
#    model_provider = "accountant"
#
#    [model_providers.accountant]
#    base_url = "http://127.0.0.1:8787/codex"
#    wire_api = "responses"
#    env_key = "ACCOUNTANT_API_KEY"

export ACCOUNTANT_API_KEY=<client key>
```

Differences from Claude accounts:

- Codex credentials are OAuth tokens that **expire (~10-day access tokens)
  and rotate on refresh**. The proxy refreshes them itself, single-flight
  per account, and writes the rotated tokens back to the vault before use.
  That makes accountant the sole refresher for imported logins: keep any
  direct, non-proxied Codex CLI login in a separate `CODEX_HOME`, or its
  refresh token goes stale after accountant's first refresh.
- Quota comes from `x-codex-*` response headers (percent scale, normalized
  internally). Note upstream may report the weekly window as "primary" —
  windows are mapped by their actual length, not their position.
- Only model traffic goes through the proxy. Login, connectors, and other
  ChatGPT app features keep talking to OpenAI directly from the CLI.
- Model discovery is live: the CLI fetches `GET /codex/models` through the
  proxy (5-minute `models_cache.json` TTL), so the picker reflects the plan
  of whichever account currently serves the request. Nothing is hardcoded.
- A permanently rejected refresh (expired/revoked/reused token) marks the
  account auth-broken; re-import with `accountant account add codex` and
  clear with `accountant account test ID`.

## Commands

```
accountant serve                 run the proxy in the foreground
accountant account add|list|remove|test
                                 add claude prompts for a setup-token;
                                 add codex imports a Codex CLI auth.json
accountant client add|list|update|remove
                                 update flips --claude-code-compat without
                                 rotating the key
accountant env                   print the client setup blocks
accountant status                per-account 5h/7d use, resets, cooldowns, pins
accountant doctor                permissions, token tests, upstream reachability
accountant service install|uninstall   launchd user service
```

## Files

- `~/.config/accountant/config.toml` — accounts (metadata only) and client
  key hashes. Mode `0600`, directory `0700`.
- `~/.local/state/accountant/state.json` — quota snapshots, session pins,
  cooldowns. Mode `0600`, guarded by `state.lock` (flock) shared by the
  daemon and all CLI commands.
- `~/Library/LaunchAgents/com.accountant.agent.plist` — the service.

Accounts added or edited while the daemon runs take effect immediately;
every routing decision re-reads config and state under the lock.

## Recovery

- An account that returns 401 leaves rotation until
  `accountant account test ID` passes again. (Codex: `account test` also
  refreshes expired OAuth tokens.)
- When every account is exhausted, you see the true upstream 429 unchanged,
  and Claude Code shows its normal limit message with the right reset time.
- Claude setup-tokens last one year; `accountant status` warns 30 days
  before expiry. The fix is a fresh `claude setup-token` and
  `accountant account add`. Codex OAuth tokens never expire while the
  proxy refreshes them; a permanently rejected refresh means re-import.

## Development

```sh
gofmt -l .
go build ./...
go vet ./...
go test ./... -race
golangci-lint run
```

The test suite runs against a scripted fake upstream with a fake clock and
covers the full validation plan in `DESIGN.md`: byte-identical pass-through,
beta-header merge, pin stickiness across restarts, pre-byte failover, the
cooldown chain, zero-eligible probes, and the safety limits. A test also
scans logs to prove no token or session ID ever appears in them.

## License

Apache License 2.0. Copyright 2026 Sebastiano Poggi. See `LICENSE`.
