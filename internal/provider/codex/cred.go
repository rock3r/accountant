// Package codex is the Codex (ChatGPT subscription) provider: OAuth
// tokens imported from a Codex CLI login, x-codex-* rate-limit headers,
// and refresh-token rotation against auth.openai.com.
// Facts verified in docs/s4-results.md.
package codex

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"accountant/internal/provider"
)

const (
	// DefaultBaseURL is the production ChatGPT Codex backend
	// (codex-rs CHATGPT_CODEX_BASE_URL).
	DefaultBaseURL = "https://chatgpt.com/backend-api/codex"
	// defaultRefreshURL is the OAuth token endpoint the Codex CLI itself
	// uses (codex-rs login crate, REFRESH_TOKEN_URL).
	defaultRefreshURL = "https://auth.openai.com/oauth/token"
	// clientID is the first-party Codex CLI OAuth client ID; token refresh
	// only works with it (codex-rs login crate, CLIENT_ID).
	clientID = "app_EMoamEEZ73f0CkXaXp7hrann"
)

// Credential is the vault-stored form of one Codex subscription login: the
// OAuth token triple from a Codex CLI auth.json. The refresh token rotates
// on every refresh; the stored credential always carries the latest one.
type Credential struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`
}

// ParseCredential validates the vault-stored credential JSON. It also
// accepts a raw Codex CLI auth.json document (tokens are nested under
// "tokens" there).
func ParseCredential(b []byte) (Credential, error) {
	var c Credential
	if err := json.Unmarshal(b, &c); err == nil && c.AccessToken != "" {
		if c.RefreshToken == "" || c.AccountID == "" {
			return Credential{}, fmt.Errorf("credential missing refresh_token or account_id")
		}
		return c, nil
	}
	var authJSON struct {
		Tokens *Credential `json:"tokens"`
	}
	if err := json.Unmarshal(b, &authJSON); err != nil {
		return Credential{}, fmt.Errorf("unparseable credential: %w", err)
	}
	if authJSON.Tokens == nil || authJSON.Tokens.AccessToken == "" {
		return Credential{}, fmt.Errorf("no access token in credential")
	}
	c = *authJSON.Tokens
	if c.RefreshToken == "" || c.AccountID == "" {
		return Credential{}, fmt.Errorf("credential missing refresh_token or account_id")
	}
	return c, nil
}

// ImportAuthFile reads a Codex CLI auth.json and returns the vault-stored
// credential JSON for it. The source file is only read, never modified;
// accountant owns refresh from import on (the CLI's copy goes stale after
// the first rotation).
func ImportAuthFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var doc struct {
		AuthMode string      `json:"auth_mode"`
		Tokens   *Credential `json:"tokens"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if doc.AuthMode != "" && doc.AuthMode != "chatgpt" {
		return nil, fmt.Errorf("%s: auth_mode is %q, not \"chatgpt\" (an API-key login has no subscription quota to roll over)", path, doc.AuthMode)
	}
	if doc.Tokens == nil {
		return nil, fmt.Errorf("%s: no tokens block (log in with `codex login` first)", path)
	}
	if doc.Tokens.AccessToken == "" || doc.Tokens.RefreshToken == "" || doc.Tokens.AccountID == "" {
		return nil, fmt.Errorf("%s: tokens block is incomplete (need access_token, refresh_token, account_id)", path)
	}
	return json.Marshal(*doc.Tokens)
}

// accessTokenExpiry reads the exp claim from the access token JWT without
// verifying the signature (the upstream verifies it; we only schedule).
func accessTokenExpiry(accessToken string) (time.Time, bool) {
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

// refreshRequest is the Codex CLI's token-refresh body (JSON, not form).
type refreshRequest struct {
	ClientID     string `json:"client_id"`
	GrantType    string `json:"grant_type"`
	RefreshToken string `json:"refresh_token"`
}

// refreshResponse carries rotated tokens; every field is optional upstream
// and a returned refresh_token MUST replace the stored one.
type refreshResponse struct {
	IDToken      *string `json:"id_token"`
	AccessToken  *string `json:"access_token"`
	RefreshToken *string `json:"refresh_token"`
}

// refresh exchanges the stored refresh token for a new access token (and,
// upstream permitting, a rotated refresh token) at auth.openai.com.
// Permanent failures — HTTP 401, HTTP 400 invalid_grant, or the codes
// refresh_token_expired / refresh_token_reused / refresh_token_invalidated
// (codex-rs classify_refresh_token_failure) — come back as
// *provider.RefreshError with Permanent set.
func (p *Provider) refresh(ctx context.Context, cred Credential) (Credential, error) {
	body, err := json.Marshal(refreshRequest{
		ClientID:     clientID,
		GrantType:    "refresh_token",
		RefreshToken: cred.RefreshToken,
	})
	if err != nil {
		return Credential{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.refreshURL, bytes.NewReader(body))
	if err != nil {
		return Credential{}, err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return Credential{}, &provider.RefreshError{Message: "refresh: " + err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return Credential{}, &provider.RefreshError{Message: "refresh: " + err.Error()}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var rr refreshResponse
		if err := json.Unmarshal(respBody, &rr); err != nil {
			return Credential{}, &provider.RefreshError{Message: "refresh: unparseable token response"}
		}
		if rr.AccessToken == nil || *rr.AccessToken == "" {
			// Without a new access token the old (near-expiry) one would
			// be persisted as a "successful" refresh; refuse instead.
			return Credential{}, &provider.RefreshError{Message: "refresh: token response has no access token"}
		}
		cred.AccessToken = *rr.AccessToken
		if rr.RefreshToken != nil && *rr.RefreshToken != "" {
			cred.RefreshToken = *rr.RefreshToken
		}
		return cred, nil
	}
	if permanentRefreshFailure(resp.StatusCode, respBody) {
		return Credential{}, &provider.RefreshError{
			Permanent: true,
			Message:   fmt.Sprintf("refresh token rejected (HTTP %d); re-import with `accountant account add codex`", resp.StatusCode),
		}
	}
	return Credential{}, &provider.RefreshError{
		Message: fmt.Sprintf("refresh: unexpected HTTP %d", resp.StatusCode),
	}
}

// permanentRefreshFailure mirrors the Codex CLI's classification: 401, or
// 400 invalid_grant, or an explicit expired/reused/invalidated code.
func permanentRefreshFailure(status int, body []byte) bool {
	if status == http.StatusUnauthorized {
		return true
	}
	code := refreshErrorCode(body)
	if status == http.StatusBadRequest && strings.EqualFold(code, "invalid_grant") {
		return true
	}
	switch strings.ToLower(code) {
	case "refresh_token_expired", "refresh_token_reused", "refresh_token_invalidated":
		return true
	}
	return false
}

// refreshErrorCode extracts the machine-readable error code, accepting both
// {"error": {"code": ...}} and {"error": "code"} / {"code": ...}.
func refreshErrorCode(body []byte) string {
	var v struct {
		Error json.RawMessage `json:"error"`
		Code  string          `json:"code"`
	}
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	if v.Code != "" {
		return v.Code
	}
	var s string
	if json.Unmarshal(v.Error, &s) == nil {
		return s
	}
	var obj struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(v.Error, &obj) == nil {
		return obj.Code
	}
	return ""
}
