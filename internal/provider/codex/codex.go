package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"accountant/internal/provider"
)

// defaultClientVersion is sent as the client_version query when the client
// did not send one; the backend rejects /models without it (S4).
const defaultClientVersion = "0.163.0"

// Provider implements provider.Provider (plus provider.Refresher and
// provider.CredentialExpirer) for ChatGPT Codex subscriptions.
type Provider struct {
	baseURL    string
	refreshURL string
	client     *http.Client
}

// New returns a Provider targeting baseURL (production default or a fake
// httptest server in tests).
func New(baseURL string) *Provider {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Provider{
		baseURL:    strings.TrimRight(baseURL, "/"),
		refreshURL: defaultRefreshURL,
		client:     &http.Client{},
	}
}

// NewForTest additionally overrides the OAuth refresh endpoint.
func NewForTest(baseURL, refreshURL string) *Provider {
	p := New(baseURL)
	if refreshURL != "" {
		p.refreshURL = refreshURL
	}
	return p
}

// ID implements provider.Provider.
func (p *Provider) ID() string { return "codex" }

// UpstreamBaseURL implements provider.Provider.
func (p *Provider) UpstreamBaseURL() string { return p.baseURL }

// RewriteRequest swaps the inbound credential headers for the account's:
// Authorization: Bearer <access token> plus ChatGPT-Account-ID (S4). The
// inbound Authorization, x-api-key, and ChatGPT-Account-ID headers are
// deleted without exception; every other header passes through. Clients
// that are not the Codex CLI get the CLI's defaults filled in: the
// originator header and the client_version query (required on /models).
func (p *Provider) RewriteRequest(req *http.Request, credBytes []byte) error {
	cred, err := ParseCredential(credBytes)
	if err != nil {
		return err
	}
	req.Header.Del("Authorization")
	req.Header.Del("x-api-key")
	req.Header.Del("ChatGPT-Account-ID")
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	req.Header.Set("ChatGPT-Account-ID", cred.AccountID)
	if req.Header.Get("originator") == "" {
		req.Header.Set("originator", "codex_cli_rs")
	}
	if req.URL.Query().Get("client_version") == "" {
		q := req.URL.Query()
		q.Set("client_version", defaultClientVersion)
		req.URL.RawQuery = q.Encode()
	}
	return nil
}

// SessionKey implements provider.Provider: the Codex CLI sends a stable
// session-id header per session, with x-client-request-id (the thread ID)
// as fallback (S4).
func (p *Provider) SessionKey(req *http.Request) string {
	if v := req.Header.Get("session-id"); v != "" {
		return v
	}
	return req.Header.Get("x-client-request-id")
}

// fiveHourWindowMinutes bounds which quota window maps to the router's
// five-hour slot. Observed Codex windows are 300 minutes (5h) and
// 10080 minutes (weekly); either window can arrive in the "primary"
// position depending on plan (S4), so the window length, not the position,
// decides the slot.
const fiveHourWindowMinutes = 720

// QuotaFromResponse implements provider.Provider: reads the x-codex-*
// rate-limit header family. When x-codex-active-limit names another
// family and that family carries data, the named family wins (this is how
// the Codex CLI itself picks; S4). Used-percent values are normalized to
// fractions; ok is true when at least one quota header was present.
func (p *Provider) QuotaFromResponse(resp *http.Response) (provider.QuotaSnapshot, bool) {
	h := resp.Header
	prefix := "x-codex"
	if limit := strings.ToLower(strings.TrimSpace(h.Get("x-codex-active-limit"))); limit != "" {
		alt := "x-" + strings.ReplaceAll(limit, "_", "-")
		if _, ok := parseCodexWindow(h, alt+"-primary"); ok {
			prefix = alt
		} else if _, ok := parseCodexWindow(h, alt+"-secondary"); ok {
			prefix = alt
		}
	}

	primary, pOK := parseCodexWindow(h, prefix+"-primary")
	secondary, sOK := parseCodexWindow(h, prefix+"-secondary")
	credits := parseCodexCredits(h)
	if !pOK && !sOK && credits == nil {
		return provider.QuotaSnapshot{}, false
	}

	var s provider.QuotaSnapshot
	fiveHourTaken := false
	place := func(w provider.WindowSample, minutes int64, fallbackFiveHour bool) {
		fiveHour := fallbackFiveHour
		if minutes > 0 {
			fiveHour = minutes <= fiveHourWindowMinutes
		}
		if fiveHour {
			s.FiveHour = w
			fiveHourTaken = true
		} else {
			s.SevenDay = w
		}
	}
	if pOK {
		place(primary, windowMinutes(h, prefix+"-primary-window-minutes"), true)
	}
	if sOK {
		// Fallback for a secondary window with no recorded length is the
		// slot the primary window did not take.
		place(secondary, windowMinutes(h, prefix+"-secondary-window-minutes"), !fiveHourTaken)
	}
	s.Credits = credits
	return s, true
}

// parseCodexCredits reads the x-codex-credits-* family and plan type.
// Display-only data. A balance only counts when has-credits is true (or
// absent): a non-credit plan reports balance 0, which must not render as
// "0 credits". The plan label is kept either way.
func parseCodexCredits(h http.Header) *provider.CreditSample {
	cs := &provider.CreditSample{Plan: strings.ToLower(strings.TrimSpace(h.Get("x-codex-plan-type")))}
	hasCredits := true
	if v := strings.TrimSpace(h.Get("x-codex-credits-has-credits")); v != "" {
		hasCredits = strings.EqualFold(v, "true")
	}
	if b := strings.TrimSpace(h.Get("x-codex-credits-balance")); b != "" && hasCredits {
		if f, ok := provider.ParseFiniteFloat(b); ok {
			cs.Balance = f
			cs.HasBalance = true
		}
	}
	cs.Unlimited = strings.EqualFold(strings.TrimSpace(h.Get("x-codex-credits-unlimited")), "true")
	if !cs.HasBalance && !cs.Unlimited && cs.Plan == "" {
		return nil
	}
	return cs
}

// parseCodexWindow reads one window's headers. A window whose values are
// all zero/empty is inactive upstream (S4) and reports ok = false.
func parseCodexWindow(h http.Header, prefix string) (provider.WindowSample, bool) {
	var w provider.WindowSample
	util := strings.TrimSpace(h.Get(prefix + "-used-percent"))
	if util != "" {
		if f, ok := provider.ParseFiniteFloat(util); ok {
			w.Utilization = f / 100 // percent upstream; fraction internally
			w.HasUtilization = true
			w.Normalized = true
		}
	}
	if v := strings.TrimSpace(h.Get(prefix + "-reset-at")); v != "" {
		if t, err := parseResetTime(v); err == nil {
			w.ResetAt = t
			w.HasReset = true
		}
	}
	inactive := (!w.HasUtilization || w.Utilization == 0) && !w.HasReset &&
		windowMinutes(h, prefix+"-window-minutes") == 0
	return w, !inactive
}

func windowMinutes(h http.Header, key string) int64 {
	v := strings.TrimSpace(h.Get(key))
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// parseResetTime accepts Unix epoch seconds (S4: what upstream sends) and
// RFC 3339.
func parseResetTime(v string) (time.Time, error) {
	if secs, ok := provider.ParseFiniteFloat(v); ok {
		whole := int64(secs)
		frac := secs - float64(whole)
		return time.Unix(whole, int64(frac*1e9)).UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("unparseable reset time %q", v)
}

// authCodeRE matches Codex auth/permission error codes in a 403 body.
var authCodeRE = regexp.MustCompile(`"(?:type|code)"\s*:\s*"(authentication_error|permission_error|invalid_api_key|invalid_token)"`)

// Classify implements provider.Provider. Any 429 is quota-exhausted: the
// backend sends usage_limit_reached for plan exhaustion, and the rarer
// per-minute 429 carries a retry-after the router's cooldown chain honors
// (S4). A 401 is always auth-broken; a 403 only when the error prefix
// carries an auth-shaped code (anything else, e.g. a Cloudflare edge
// block, is Transient).
func (p *Provider) Classify(resp *http.Response, bodyPrefix []byte) provider.Verdict {
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return provider.VerdictOK
	case resp.StatusCode == http.StatusTooManyRequests:
		return provider.VerdictQuotaExhausted
	case resp.StatusCode == http.StatusUnauthorized:
		return provider.VerdictAuthBroken
	case resp.StatusCode == http.StatusForbidden:
		if authCodeRE.Match(bodyPrefix) {
			return provider.VerdictAuthBroken
		}
		return provider.VerdictTransient
	default:
		return provider.VerdictTransient
	}
}

// Validate implements provider.Provider with the cheapest verified call:
// GET /models?client_version=... (S4: 200 with a good token, 401 with a
// bad one; no quota cost).
func (p *Provider) Validate(ctx context.Context, credBytes []byte) error {
	if _, err := ParseCredential(credBytes); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.baseURL+"/models?client_version="+defaultClientVersion, nil)
	if err != nil {
		return err
	}
	if err := p.RewriteRequest(req, credBytes); err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("validate: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("validate: credential rejected (HTTP %d)", resp.StatusCode)
	default:
		return fmt.Errorf("validate: unexpected HTTP %d", resp.StatusCode)
	}
}

// CredentialExpiry implements provider.CredentialExpirer: the access
// token's JWT exp claim (S4: ~10-day access tokens).
func (p *Provider) CredentialExpiry(credBytes []byte) (time.Time, bool) {
	cred, err := ParseCredential(credBytes)
	if err != nil {
		return time.Time{}, false
	}
	return accessTokenExpiry(cred.AccessToken)
}

// Refresh implements provider.Refresher: exchanges the stored refresh
// token at auth.openai.com and returns the updated credential JSON.
func (p *Provider) Refresh(ctx context.Context, credBytes []byte) ([]byte, error) {
	cred, err := ParseCredential(credBytes)
	if err != nil {
		return nil, err
	}
	cred, err = p.refresh(ctx, cred)
	if err != nil {
		return nil, err
	}
	return json.Marshal(cred)
}

var (
	_ provider.Provider          = (*Provider)(nil)
	_ provider.Refresher         = (*Provider)(nil)
	_ provider.CredentialExpirer = (*Provider)(nil)
)
