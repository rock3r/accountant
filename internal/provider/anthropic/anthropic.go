// Package anthropic is the Anthropic provider: setup-token auth, unified
// rate-limit headers, and the cheapest credential check (count_tokens).
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"accountant/internal/provider"
)

const (
	// DefaultBaseURL is the production upstream.
	DefaultBaseURL = "https://api.anthropic.com"
	oauthBeta      = "oauth-2025-04-20"
	// validateModel is the cheapest current model; S1 verified count_tokens
	// accepts it and needs no system prompt.
	validateModel = "claude-haiku-5-5"
)

// Provider implements provider.Provider for Anthropic.
type Provider struct {
	baseURL string
	client  *http.Client
}

// New returns a Provider targeting baseURL (production default or a fake
// httptest server in tests).
func New(baseURL string) *Provider {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Provider{baseURL: strings.TrimRight(baseURL, "/"), client: &http.Client{}}
}

// ID implements provider.Provider.
func (p *Provider) ID() string { return "anthropic" }

// UpstreamBaseURL implements provider.Provider.
func (p *Provider) UpstreamBaseURL() string { return p.baseURL }

// RewriteRequest swaps the inbound credential headers for the account
// token and merges oauth-2025-04-20 into anthropic-beta. It deletes the
// inbound Authorization and x-api-key headers without exception, never
// drops client beta values, and appends the oauth token only if absent as
// a whole token (case-insensitive) across all anthropic-beta lines.
func (p *Provider) RewriteRequest(req *http.Request, cred []byte) error {
	req.Header.Del("Authorization")
	req.Header.Del("x-api-key")
	req.Header.Set("Authorization", "Bearer "+string(cred))

	found := false
	for _, line := range req.Header.Values("anthropic-beta") {
		for _, tok := range strings.Split(line, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), oauthBeta) {
				found = true
				break
			}
		}
	}
	if !found {
		lines := req.Header.Values("anthropic-beta")
		if len(lines) == 0 {
			req.Header.Set("anthropic-beta", oauthBeta)
		} else {
			// Append to the last line; never drop client header lines.
			lines[len(lines)-1] = lines[len(lines)-1] + ", " + oauthBeta
			req.Header[http.CanonicalHeaderKey("anthropic-beta")] = lines
		}
	}
	return nil
}

// SessionKey implements provider.Provider.
func (p *Provider) SessionKey(req *http.Request) string {
	return req.Header.Get("x-claude-code-session-id")
}

const (
	hPrefix   = "anthropic-ratelimit-unified-"
	hStatus   = hPrefix + "status"
	h5hUtil   = hPrefix + "5h-utilization"
	h5hReset  = hPrefix + "5h-reset"
	h5hStatus = hPrefix + "5h-status"
	h7dUtil   = hPrefix + "7d-utilization"
	h7dReset  = hPrefix + "7d-reset"
	h7dStatus = hPrefix + "7d-status"
)

// QuotaFromResponse reads the unified rate-limit headers. ok is true when
// at least one unified header was present.
func (p *Provider) QuotaFromResponse(resp *http.Response) (provider.QuotaSnapshot, bool) {
	var s provider.QuotaSnapshot
	ok := false
	s.Status = resp.Header.Get(hStatus)
	if s.Status != "" {
		ok = true
	}
	if w, wok := parseWindow(resp.Header, h5hUtil, h5hReset, h5hStatus); wok {
		s.FiveHour = w
		ok = true
	}
	if w, wok := parseWindow(resp.Header, h7dUtil, h7dReset, h7dStatus); wok {
		s.SevenDay = w
		ok = true
	}
	return s, ok
}

func parseWindow(h http.Header, utilKey, resetKey, statusKey string) (provider.WindowSample, bool) {
	var w provider.WindowSample
	ok := false
	if v := h.Get(utilKey); v != "" {
		if f, finite := provider.ParseFiniteFloat(strings.TrimSpace(v)); finite {
			w.Utilization = f
			w.HasUtilization = true
			ok = true
		}
	}
	if v := h.Get(resetKey); v != "" {
		if t, err := ParseResetTime(v); err == nil {
			w.ResetAt = t
			w.HasReset = true
			ok = true
		}
	}
	if v := h.Get(statusKey); v != "" {
		w.Status = v
		ok = true
	}
	return w, ok
}

// ParseResetTime accepts Unix epoch seconds (S1: what upstream sends) and
// RFC 3339.
func ParseResetTime(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, fmt.Errorf("empty reset time")
	}
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

// errorBody is the Anthropic error envelope.
type errorBody struct {
	Error struct {
		Type string `json:"type"`
	} `json:"error"`
}

var authTypeRE = regexp.MustCompile(`"type"\s*:\s*"(authentication_error|permission_error)"`)

// Classify implements provider.Provider.
func (p *Provider) Classify(resp *http.Response, bodyPrefix []byte) provider.Verdict {
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return provider.VerdictOK
	case resp.StatusCode == http.StatusTooManyRequests:
		return provider.VerdictQuotaExhausted
	case resp.StatusCode == http.StatusUnauthorized:
		return provider.VerdictAuthBroken
	case resp.StatusCode == http.StatusForbidden:
		// Auth-broken only when the buffered error prefix parses as an
		// Anthropic authentication or permission error; any other 403 (for
		// example a Cloudflare/WAF edge block) is Transient.
		var eb errorBody
		if err := json.Unmarshal(bodyPrefix, &eb); err == nil &&
			(eb.Error.Type == "authentication_error" || eb.Error.Type == "permission_error") {
			return provider.VerdictAuthBroken
		}
		if authTypeRE.Match(bodyPrefix) {
			return provider.VerdictAuthBroken
		}
		return provider.VerdictTransient
	default:
		// 400, 404, 413, 5xx, 529, anything new: Transient by default.
		return provider.VerdictTransient
	}
}

// Validate implements provider.Provider using the cheapest call verified in
// S1: POST /v1/messages/count_tokens (free, no system-prompt gate).
func (p *Provider) Validate(ctx context.Context, cred []byte) error {
	body := []byte(`{"model":"` + validateModel + `","messages":[{"role":"user","content":"hi"}]}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/messages/count_tokens", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	if err := p.RewriteRequest(req, cred); err != nil {
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
