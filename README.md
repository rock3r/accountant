# accountant

[![ci](https://github.com/rock3r/accountant/actions/workflows/ci.yml/badge.svg)](https://github.com/rock3r/accountant/actions/workflows/ci.yml)

Multi-subscription rollover for Claude. One local proxy holds one long-lived
token per Claude subscription and moves work to the next subscription when
the current one runs out — no manual account switching.

Status: v1, local proof of concept on one Mac. See `DESIGN.md` for the
binding design and `docs/s1-results.md` for the verified upstream facts.

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

## Commands

```
accountant serve                 run the proxy in the foreground
accountant account add|list|remove|test
accountant client add|list|update|remove
                                 update flips --claude-code-compat without
                                 rotating the key
accountant env                   print the Claude Code env exports
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
  `accountant account test ID` passes again.
- When every account is exhausted, you see the true upstream 429 unchanged,
  and Claude Code shows its normal limit message with the right reset time.
- Token lasts one year; `accountant status` warns 30 days before expiry.
  The fix is a fresh `claude setup-token` and `accountant account add`.

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
