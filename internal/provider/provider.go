// Package provider defines the seam between the router/proxy core and a
// concrete upstream provider. The core must not know Anthropic details.
package provider

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"time"
)

// ParseFiniteFloat parses a header float, rejecting NaN and infinities.
// Those values are accepted by strconv.ParseFloat but rejected by
// json.Marshal, so letting one through would fail the entire state-file
// update (losing quota and cooldown data recorded in the same write).
func ParseFiniteFloat(s string) (float64, bool) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// WindowSample is one quota window parsed from response headers.
type WindowSample struct {
	Utilization    float64
	HasUtilization bool
	ResetAt        time.Time
	HasReset       bool
	Status         string // e.g. "allowed", "rejected"
	// Normalized marks Utilization as already a fraction (provider
	// converted its native scale); the router must not apply the
	// configured utilization_scale to it again.
	Normalized bool
}

// QuotaSnapshot is the per-response quota read. Response headers are the
// source of truth.
type QuotaSnapshot struct {
	FiveHour WindowSample
	SevenDay WindowSample
	Status   string // unified status
	// Credits is the prepaid credit balance reading for plans metered in
	// credits instead of (or besides) rolling windows. Nil when the
	// provider reports none. Display-only: never affects eligibility.
	Credits *CreditSample
}

// CreditSample is a provider-neutral prepaid credit balance reading
// (e.g. the Codex x-codex-credits-* family).
type CreditSample struct {
	Balance    float64 // remaining balance in provider units
	HasBalance bool
	Unlimited  bool   // plan is not credit-metered
	Plan       string // plan label, e.g. "pro"; optional
}

// Verdict buckets a finished upstream response.
type Verdict int

const (
	// VerdictOK means a 2xx; clears cooldown.
	VerdictOK Verdict = iota
	// VerdictQuotaExhausted means an HTTP 429 on a served endpoint.
	VerdictQuotaExhausted
	// VerdictAuthBroken means a 401, or a 403 with an auth/permission
	// error prefix.
	VerdictAuthBroken
	// VerdictTransient covers everything else; never advances, never cools
	// down.
	VerdictTransient
)

func (v Verdict) String() string {
	switch v {
	case VerdictOK:
		return "ok"
	case VerdictQuotaExhausted:
		return "quota_exhausted"
	case VerdictAuthBroken:
		return "auth_broken"
	default:
		return "transient"
	}
}

// Provider is the provider-neutral seam. See DESIGN.md, "The provider seam".
type Provider interface {
	ID() string // "anthropic"
	// Constructor-injected per instance: the production default for a
	// provider, a fake httptest server in tests.
	UpstreamBaseURL() string
	// RewriteRequest injects the account credential and adjusts only
	// auth-related headers. For Anthropic that includes the anthropic-beta
	// oauth merge.
	RewriteRequest(req *http.Request, cred []byte) error
	// SessionKey extracts the session key, or "" when the client sends none.
	SessionKey(req *http.Request) string
	// QuotaFromResponse reads quota windows from response headers.
	QuotaFromResponse(resp *http.Response) (QuotaSnapshot, bool)
	// Classify buckets a finished response. bodyPrefix is the bounded
	// non-2xx prefix the proxy read before sending anything (empty for
	// 2xx), for providers that signal exhaustion in the body.
	Classify(resp *http.Response, bodyPrefix []byte) Verdict
	// Validate is the cheapest credential check available.
	Validate(ctx context.Context, cred []byte) error
}

// Refresher is implemented only by providers whose credentials expire.
// Claude setup-tokens do not; Codex OAuth does. The proxy calls Refresh
// single-flight per account and writes the new credential to the vault
// before any request uses it.
type Refresher interface {
	Refresh(ctx context.Context, cred []byte) (newCred []byte, err error)
}

// CredentialExpirer is implemented by providers whose credentials carry an
// expiry the proxy should respect proactively: when the expiry is near,
// the proxy refreshes before sending rather than waiting for a 401.
type CredentialExpirer interface {
	// CredentialExpiry reports when the credential stops being accepted.
	// ok is false when the credential carries no readable expiry.
	CredentialExpiry(cred []byte) (time.Time, bool)
}

// RefreshError classifies a credential refresh failure. A permanent
// failure (expired, reused, or revoked refresh token) takes the account
// out of rotation; a transient one (network, 5xx) does not.
type RefreshError struct {
	Permanent bool
	Message   string
}

func (e *RefreshError) Error() string { return e.Message }

// RequestShaper is implemented by providers that can rewrite a request
// body for clients that are not the provider's native client. For
// Anthropic, subscription credentials are only accepted when the request
// looks like it came from Claude Code, so the shaper prepends Claude
// Code's first system block. The proxy applies it only to clients
// flagged claude_code_compat; native Claude Code traffic stays
// byte-identical.
type RequestShaper interface {
	// ShapeRequest returns the body to send upstream. Bodies that need
	// no shaping must be returned byte-for-byte unchanged.
	ShapeRequest(body []byte) ([]byte, error)
}
