package anthropic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"accountant/internal/provider"
)

func TestRewriteRequestSwapsCredentials(t *testing.T) {
	p := New("")
	req, _ := http.NewRequest(http.MethodPost, "http://x/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("x-api-key", "another-client-key")
	if err := p.RewriteRequest(req, []byte("tok-account")); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer tok-account" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := req.Header.Get("x-api-key"); got != "" {
		t.Fatalf("x-api-key must be deleted, got %q", got)
	}
}

func TestRewriteRequestBetaMerge(t *testing.T) {
	p := New("")
	cases := []struct {
		name string
		in   []string // existing anthropic-beta header lines
		want string   // expected single resulting header value
	}{
		{"absent", nil, "oauth-2025-04-20"},
		{"empty then merge", []string{"context-1m-2025-08-07"}, "context-1m-2025-08-07, oauth-2025-04-20"},
		{"already present", []string{"oauth-2025-04-20,foo"}, "oauth-2025-04-20,foo"},
		{"present case-insensitive", []string{"OAUTH-2025-04-20"}, "OAUTH-2025-04-20"},
		{"present on second line", []string{"a,b", "c,oauth-2025-04-20"}, "a,b"},
		{"substring is not a match", []string{"oauth-2025-04-20-extended"}, "oauth-2025-04-20-extended, oauth-2025-04-20"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, "http://x/v1/messages", nil)
			for _, line := range tc.in {
				req.Header.Add("anthropic-beta", line)
			}
			if err := p.RewriteRequest(req, []byte("tok")); err != nil {
				t.Fatal(err)
			}
			if got := req.Header.Get("anthropic-beta"); got != tc.want {
				t.Fatalf("anthropic-beta = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSessionKey(t *testing.T) {
	p := New("")
	req, _ := http.NewRequest(http.MethodPost, "http://x/v1/messages", nil)
	if k := p.SessionKey(req); k != "" {
		t.Fatalf("want empty, got %q", k)
	}
	req.Header.Set("x-claude-code-session-id", "sess-1")
	if k := p.SessionKey(req); k != "sess-1" {
		t.Fatalf("got %q", k)
	}
}

func TestQuotaFromResponseEpochSeconds(t *testing.T) {
	// Shape captured in S1.
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-status", "rejected")
	h.Set("anthropic-ratelimit-unified-5h-status", "allowed")
	h.Set("anthropic-ratelimit-unified-5h-utilization", "0.0")
	h.Set("anthropic-ratelimit-unified-5h-reset", "1792260600")
	h.Set("anthropic-ratelimit-unified-7d-status", "rejected")
	h.Set("anthropic-ratelimit-unified-7d-utilization", "1.0")
	h.Set("anthropic-ratelimit-unified-7d-reset", "1792584000")
	resp := &http.Response{Header: h}

	p := New("")
	snap, ok := p.QuotaFromResponse(resp)
	if !ok {
		t.Fatal("expected snapshot")
	}
	if snap.Status != "rejected" {
		t.Fatalf("status %q", snap.Status)
	}
	if snap.SevenDay.Utilization != 1.0 || !snap.SevenDay.HasUtilization {
		t.Fatalf("7d util %+v", snap.SevenDay)
	}
	if got := snap.SevenDay.ResetAt.Unix(); got != 1792584000 {
		t.Fatalf("7d reset %d", got)
	}
	if snap.FiveHour.Status != "allowed" {
		t.Fatalf("5h status %q", snap.FiveHour.Status)
	}
}

func TestQuotaFromResponseRFC3339(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-5h-utilization", "0.5")
	h.Set("anthropic-ratelimit-unified-5h-reset", "2026-10-17T20:10:00Z")
	resp := &http.Response{Header: h}
	p := New("")
	snap, ok := p.QuotaFromResponse(resp)
	if !ok {
		t.Fatal("expected snapshot")
	}
	want := time.Date(2026, 10, 17, 20, 10, 0, 0, time.UTC)
	if !snap.FiveHour.ResetAt.Equal(want) {
		t.Fatalf("reset %v", snap.FiveHour.ResetAt)
	}
}

func TestQuotaFromResponseAbsent(t *testing.T) {
	p := New("")
	if _, ok := p.QuotaFromResponse(&http.Response{Header: http.Header{}}); ok {
		t.Fatal("no unified headers: want ok=false")
	}
}

//nolint:bodyclose // responses are synthetic structs, not from HTTP calls
func TestClassify(t *testing.T) {
	p := New("")
	mk := func(code int) *http.Response {
		return &http.Response{StatusCode: code, Header: http.Header{}, Body: http.NoBody}
	}
	cases := []struct {
		name   string
		resp   *http.Response
		prefix []byte
		want   provider.Verdict
	}{
		{"200", mk(200), nil, provider.VerdictOK},
		{"429", mk(429), nil, provider.VerdictQuotaExhausted},
		{"401", mk(401), nil, provider.VerdictAuthBroken},
		{"403 authentication_error", mk(403), []byte(`{"type":"error","error":{"type":"authentication_error","message":"bad token"}}`), provider.VerdictAuthBroken},
		{"403 permission_error", mk(403), []byte(`{"type":"error","error":{"type":"permission_error","message":"scope"}}`), provider.VerdictAuthBroken},
		{"403 edge block", mk(403), []byte(`<html><body>Cloudflare block</body></html>`), provider.VerdictTransient},
		{"403 other anthropic error", mk(403), []byte(`{"type":"error","error":{"type":"forbidden","message":"x"}}`), provider.VerdictTransient},
		{"500", mk(500), nil, provider.VerdictTransient},
		{"529", mk(529), nil, provider.VerdictTransient},
		{"400", mk(400), nil, provider.VerdictTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.Classify(tc.resp, tc.prefix); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	var sawPath, sawAuth, sawBeta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawAuth = r.Header.Get("Authorization")
		sawBeta = r.Header.Get("anthropic-beta")
		if r.Header.Get("Authorization") == "Bearer good" {
			_, _ = w.Write([]byte(`{"input_tokens":10}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	p := New(srv.URL)
	if err := p.Validate(context.Background(), []byte("good")); err != nil {
		t.Fatalf("good token: %v", err)
	}
	if sawPath != "/v1/messages/count_tokens" {
		t.Fatalf("validate path %q", sawPath)
	}
	if sawAuth != "Bearer good" {
		t.Fatalf("auth %q", sawAuth)
	}
	if sawBeta != "oauth-2025-04-20" {
		t.Fatalf("beta %q", sawBeta)
	}
	if err := p.Validate(context.Background(), []byte("bad")); err == nil {
		t.Fatal("bad token must fail")
	}
}
