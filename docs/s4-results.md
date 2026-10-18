# Spike S4 — Codex subscriptions: upstream facts

Date: 2026-10-24. Runner: implementing agent, on the owner's Mac.
Sources: codex-cli 0.163.0 installed locally (`/opt/homebrew/bin/codex`),
the Codex source tree at `~/src/codex` (eb15245d82), the owner's
`~/.codex/auth.json` (shape only), and live requests against
`https://chatgpt.com/backend-api/codex` with the owner's own token
(one GET /models, one minimal POST /responses).

## Verified facts

1. **Credential shape.** `~/.codex/auth.json` (per-profile home via
   `CODEX_HOME`) holds `{auth_mode: "chatgpt", last_refresh, tokens:
   {id_token, access_token, refresh_token, account_id}}`. Access tokens are
   JWTs; the observed token lives ~10 days (`exp - iat` = 864000 s), so a
   proxy holding these credentials *must* refresh them.
2. **Request auth.** Model requests authenticate with
   `Authorization: Bearer <access_token>` plus `ChatGPT-Account-ID:
   <account_id>` (codex-rs `model-provider/src/bearer_auth_provider.rs`).
3. **Upstream.** The built-in ChatGPT provider posts the Responses API to
   `https://chatgpt.com/backend-api/codex` (`CHATGPT_CODEX_BASE_URL`,
   codex-rs `model-provider-info/src/lib.rs`): `POST /responses` (SSE),
   `GET /models` — which *requires* a `client_version` query parameter
   (400 `invalid_request_error` without it; verified live).
4. **Validation call.** `GET /codex/models?client_version=0.163.0` with the
   account headers: 200 with the real token, 401 with a bogus one
   (verified live). No quota cost.
5. **Quota headers.** Every `/responses` answer carries the `x-codex-*`
   family (live-verified 2026-10-18: `GET /models` carries **no** quota
   headers, so validation cannot refresh quota state). Live samples, one
   per plan (owner accounts):

   ```
   # Pro (credits plan) — no 5h window at all
   x-codex-active-limit: premium
   x-codex-plan-type: pro
   x-codex-primary-used-percent: 17
   x-codex-primary-window-minutes: 10080
   x-codex-primary-reset-at: 1792874442
   x-codex-secondary-*: all zeros/empty (inactive)
   x-codex-credits-balance: 58337.5384910000
   x-codex-credits-has-credits: True
   x-codex-credits-unlimited: False

   # Plus — primary is the 5h window here
   x-codex-plan-type: plus
   x-codex-primary-used-percent: 0
   x-codex-primary-window-minutes: 300
   x-codex-secondary-used-percent: 0
   x-codex-secondary-window-minutes: 10080
   x-codex-credits-balance: 0
   x-codex-credits-has-credits: False
   ```

   - Values are **percent** (0–100), not fractions; the provider divides by
     100 before handing the router a snapshot.
   - **Window position is not the window length**: on Pro the *primary*
     window is the weekly one (10080 min); on Plus the *primary* is the
     5h window (300 min) with weekly in *secondary*. The provider maps
     windows to the router's 5h/7d slots by `*-window-minutes`
     (≤ 720 min → 5h slot), falling back to primary→5h / secondary→7d.
     Pro plans report no 5h window at all (owner-verified); their quota
     story is the weekly percent plus credits.
   - A window that is all zeros/empty is **inactive** and must not produce
     a sample (else a phantom 0% window would reset real state).
   - **Credits family** (`x-codex-credits-balance`, `-has-credits`,
     `-unlimited`, plus `x-codex-plan-type`) is display-only data;
     accountant records it in state and shows it in `accountant status`.
     A balance only counts when `has-credits` is true (or absent): a
     non-credit plan reports balance 0, which must not render as
     "0 credits".
   - `x-codex-active-limit` names the active limit family; per-family
     headers `x-<limit>-primary-*` can supplement the default family
     (codex-rs `codex-api/src/rate_limits.rs`, `parse_all_rate_limits`).
   - Reset times are **Unix epoch seconds**.
6. **Exhaustion signal.** Plan exhaustion is HTTP 429 with body
   `{"error": {"type": "usage_limit_reached", "resets_at": <epoch>,
   "plan_type": ...}}` and the rate-limit headers still present (codex-rs
   `codex-api/src/api_bridge.rs`). `usage_not_included` (plan has no Codex
   access) is also a 429. Per-minute rate limits are 429 too, so
   accountant's "any 429 is quota-exhausted, cooldown refined by headers /
   retry-after" rule applies unchanged; the Codex CLI itself sets
   `retry_429: false`.
7. **Refresh flow** (source-verified only — running it would rotate the
   owner's live CLI tokens): `POST https://auth.openai.com/oauth/token`,
   JSON `{client_id: "app_EMoamEEZ73f0CkXaXp7hrann", grant_type:
   "refresh_token", refresh_token}` → `{id_token?, access_token?,
   refresh_token?}` (codex-rs `login/src/auth/manager.rs`). **The refresh
   token rotates**: a returned `refresh_token` must replace the stored one
   before any further refresh. Permanent failures: HTTP 401, HTTP 400
   `invalid_grant`, or codes `refresh_token_expired` / `_reused` /
   `_invalidated`; anything else is transient (`classify_refresh_token_failure`).
8. **Single writer.** Because refresh tokens rotate, exactly one party may
   refresh a login. If accountant refreshes, the Codex CLI copy of the same
   auth.json goes stale. `accountant account add codex` warns and tells the
   user to keep any direct CLI login in a separate `CODEX_HOME`.
9. **Client config.** The built-in `openai` provider cannot be overridden
   (`merge_configured_model_providers` keeps built-ins). Pointing the CLI
   at accountant uses a *custom* provider in `~/.codex/config.toml`:

   ```toml
   model_provider = "accountant"

   [model_providers.accountant]
   base_url = "http://127.0.0.1:8787/codex"
   wire_api = "responses"
   env_key = "ACCOUNTANT_API_KEY"   # the accountant client key
   ```

   Custom providers default `requires_openai_auth = false` (env_key Bearer
   auth — exactly accountant's client-key pattern) and
   `supports_websockets = false`, which keeps Codex on the HTTP transport
   the proxy can carry. Note `chatgpt_base_url` is *not* the knob for model
   traffic — it drives auth/connectors/plugins paths only.
10. **Session pinning.** The CLI sends a stable `session-id` header per
    session (plus `thread-id` / `x-client-request-id`; codex-rs
    `codex-api/src/requests/headers.rs`). The provider reads `session-id`,
    falling back to `x-client-request-id`.

## Consequences taken into the implementation

- `internal/provider/codex` implements `Provider`, `Refresher`, and the new
  `CredentialExpirer` seam (JWT `exp` drives proactive refresh in the proxy,
  single-flight per account, vault written before use).
- Credential JSON in the vault: `{access_token, refresh_token, account_id}`.
- Served paths under `/codex/`: `POST /responses`, `GET /models` (forwarded;
  everything else 404s).
- Cloudflare's edge sits in front of chatgpt.com. Pass-through forwards
  `Cookie`/`Set-Cookie` untouched, so the CLI's own cookie handling keeps
  working; a 403 edge block classifies Transient (not auth-broken), same as
  the Anthropic rule.

## Not verified (deliberately)

- Live token refresh (would rotate the owner's CLI login; flow is
  source-verified and covered by fake-server tests).
- A real 429 from exhaustion (no exhausted Codex account at spike time);
  the 429 shape above is from codex-rs source.
- WebSocket transport (`prefer_websockets` models): out of scope; the
  custom-provider setup above keeps Codex on HTTP SSE.
