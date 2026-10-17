// Package provider defines the seam between the router/proxy core and a
// concrete upstream provider. The core must not know Anthropic details.
package provider

import (
	"context"
	"net/http"
	"time"
)

// WindowSample is one quota window parsed from response headers.
type WindowSample struct {
	Utilization    float64
	HasUtilization bool
	ResetAt        time.Time
	HasReset       bool
	Status         string // e.g. "allowed", "rejected"
}

// QuotaSnapshot is the per-response quota read. Response headers are the
// source of truth.
type QuotaSnapshot struct {
	FiveHour WindowSample
	SevenDay WindowSample
	Status   string // unified status
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
// Claude setup-tokens do not; Codex OAuth will.
type Refresher interface {
	Refresh(ctx context.Context, cred []byte) (newCred []byte, err error)
}
