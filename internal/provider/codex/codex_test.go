package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"accountant/internal/provider"
)

func fakeJWT(exp time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, _ := json.Marshal(map[string]any{"exp": exp.Unix()})
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".fakesig"
}

func credJSON(t *testing.T, c Credential) []byte {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseCredentialDirectAndAuthJSON(t *testing.T) {
	direct := `{"access_token":"at","refresh_token":"rt","account_id":"acc"}`
	c, err := ParseCredential([]byte(direct))
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessToken != "at" || c.RefreshToken != "rt" || c.AccountID != "acc" {
		t.Fatalf("got %+v", c)
	}

	authJSON := `{"OPENAI_API_KEY":null,"auth_mode":"chatgpt","tokens":{"id_token":"id","access_token":"at2","refresh_token":"rt2","account_id":"acc2"}}`
	c, err = ParseCredential([]byte(authJSON))
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessToken != "at2" || c.RefreshToken != "rt2" || c.AccountID != "acc2" {
		t.Fatalf("got %+v", c)
	}

	for _, bad := range []string{
		`{"access_token":"at"}`,            // missing refresh/account
		`{"tokens":{"access_token":"at"}}`, // nested missing fields
		`{"not":"json"`,                    // syntax
		`{"tokens":null}`,                  // no tokens
	} {
		if _, err := ParseCredential([]byte(bad)); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestImportAuthFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(good, []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"at","refresh_token":"rt","account_id":"acc"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cred, err := ImportAuthFile(good)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseCredential(cred)
	if err != nil || c.AccessToken != "at" {
		t.Fatalf("got %v %+v", err, c)
	}

	apiKey := filepath.Join(dir, "apikey.json")
	if err := os.WriteFile(apiKey, []byte(`{"OPENAI_API_KEY":"sk-x","auth_mode":"apikey"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportAuthFile(apiKey); err == nil {
		t.Error("expected apikey auth_mode to be rejected")
	}

	if _, err := ImportAuthFile(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("expected error for missing file")
	}
}

func TestCredentialExpiry(t *testing.T) {
	p := New("")
	exp := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	cred := credJSON(t, Credential{AccessToken: fakeJWT(exp), RefreshToken: "rt", AccountID: "acc"})
	got, ok := p.CredentialExpiry(cred)
	if !ok {
		t.Fatal("expected expiry")
	}
	if !got.Equal(exp) {
		t.Fatalf("got %v, want %v", got, exp)
	}

	// Non-JWT access token: no readable expiry.
	cred = credJSON(t, Credential{AccessToken: "opaque", RefreshToken: "rt", AccountID: "acc"})
	if _, ok := p.CredentialExpiry(cred); ok {
		t.Error("expected no expiry for opaque token")
	}
}

func TestRewriteRequest(t *testing.T) {
	p := New("")
	req, err := http.NewRequest(http.MethodPost, p.UpstreamBaseURL()+"/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("x-api-key", "client-key")
	req.Header.Set("ChatGPT-Account-ID", "stale-account")
	req.Header.Set("originator", "codex_cli_rs")
	cred := credJSON(t, Credential{AccessToken: "at-1", RefreshToken: "rt-1", AccountID: "acc-1"})
	if err := p.RewriteRequest(req, cred); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer at-1" {
		t.Errorf("Authorization = %q", got)
	}
	if got := req.Header.Get("ChatGPT-Account-ID"); got != "acc-1" {
		t.Errorf("ChatGPT-Account-ID = %q", got)
	}
	if got := req.Header.Get("x-api-key"); got != "" {
		t.Errorf("x-api-key not stripped: %q", got)
	}
	if got := req.Header.Get("originator"); got != "codex_cli_rs" {
		t.Errorf("unrelated header dropped: %q", got)
	}

	if err := p.RewriteRequest(req, []byte("junk")); err == nil {
		t.Error("expected error for unparseable credential")
	}
}

func TestSessionKey(t *testing.T) {
	p := New("")
	req, _ := http.NewRequest(http.MethodPost, "http://x/responses", nil)
	if got := p.SessionKey(req); got != "" {
		t.Errorf("empty headers: got %q", got)
	}
	req.Header.Set("x-client-request-id", "thread-1")
	if got := p.SessionKey(req); got != "thread-1" {
		t.Errorf("thread fallback: got %q", got)
	}
	req.Header.Set("session-id", "sess-1")
	if got := p.SessionKey(req); got != "sess-1" {
		t.Errorf("session-id wins: got %q", got)
	}
}

func headerResp(h map[string]string) *http.Response {
	hdr := http.Header{}
	for k, v := range h {
		hdr.Set(k, v)
	}
	return &http.Response{Header: hdr}
}

func TestQuotaFromResponseWeeklyInPrimary(t *testing.T) {
	// The S4 live sample: on this plan the *primary* window is the weekly
	// one (10080 minutes); secondary is inactive.
	p := New("")
	//nolint:bodyclose // synthetic response struct, not an HTTP call
	snap, ok := p.QuotaFromResponse(headerResp(map[string]string{
		"x-codex-active-limit":             "premium",
		"x-codex-plan-type":                "pro",
		"x-codex-primary-used-percent":     "59",
		"x-codex-primary-window-minutes":   "10080",
		"x-codex-primary-reset-at":         "1792649662",
		"x-codex-secondary-used-percent":   "0",
		"x-codex-secondary-window-minutes": "0",
		"x-codex-secondary-reset-at":       "",
	}))
	if !ok {
		t.Fatal("expected snapshot")
	}
	if snap.SevenDay.HasUtilization == false || snap.SevenDay.Utilization != 0.59 {
		t.Errorf("weekly util: got %+v", snap.SevenDay)
	}
	if !snap.SevenDay.HasReset || snap.SevenDay.ResetAt.Unix() != 1792649662 {
		t.Errorf("weekly reset: got %+v", snap.SevenDay)
	}
	if snap.FiveHour.HasUtilization || snap.FiveHour.HasReset {
		t.Errorf("inactive window leaked: got %+v", snap.FiveHour)
	}
}

func TestQuotaFromResponseTwoWindows(t *testing.T) {
	p := New("")
	//nolint:bodyclose // synthetic response struct, not an HTTP call
	snap, ok := p.QuotaFromResponse(headerResp(map[string]string{
		"x-codex-primary-used-percent":     "40",
		"x-codex-primary-window-minutes":   "300",
		"x-codex-primary-reset-at":         "1792600000",
		"x-codex-secondary-used-percent":   "10",
		"x-codex-secondary-window-minutes": "10080",
		"x-codex-secondary-reset-at":       "1792649662",
	}))
	if !ok {
		t.Fatal("expected snapshot")
	}
	if snap.FiveHour.Utilization != 0.40 {
		t.Errorf("5h util: got %+v", snap.FiveHour)
	}
	if snap.SevenDay.Utilization != 0.10 {
		t.Errorf("7d util: got %+v", snap.SevenDay)
	}
}

func TestQuotaFromResponseFallbackWithoutWindowMinutes(t *testing.T) {
	p := New("")
	//nolint:bodyclose // synthetic response struct, not an HTTP call
	snap, ok := p.QuotaFromResponse(headerResp(map[string]string{
		"x-codex-primary-used-percent":   "25",
		"x-codex-secondary-used-percent": "75",
	}))
	if !ok {
		t.Fatal("expected snapshot")
	}
	if snap.FiveHour.Utilization != 0.25 || snap.SevenDay.Utilization != 0.75 {
		t.Errorf("fallback mapping: got %+v", snap)
	}
}

func TestQuotaFromResponseActiveLimitFamily(t *testing.T) {
	p := New("")
	//nolint:bodyclose // synthetic response struct, not an HTTP call
	snap, ok := p.QuotaFromResponse(headerResp(map[string]string{
		"x-codex-active-limit":             "premium",
		"x-premium-primary-used-percent":   "90",
		"x-premium-primary-window-minutes": "300",
	}))
	if !ok {
		t.Fatal("expected snapshot from named family")
	}
	if snap.FiveHour.Utilization != 0.90 {
		t.Errorf("named family util: got %+v", snap)
	}
}

func TestQuotaFromResponseNone(t *testing.T) {
	p := New("")
	//nolint:bodyclose // synthetic response struct, not an HTTP call
	if _, ok := p.QuotaFromResponse(headerResp(map[string]string{"content-type": "text/event-stream"})); ok {
		t.Error("expected ok=false without quota headers")
	}
}

func TestClassify(t *testing.T) {
	p := New("")
	cases := []struct {
		status int
		body   string
		want   provider.Verdict
	}{
		{200, "", provider.VerdictOK},
		{299, "", provider.VerdictOK},
		{429, `{"error":{"type":"usage_limit_reached","resets_at":1792649662}}`, provider.VerdictQuotaExhausted},
		{429, `{"error":{"message":"per-minute"}}`, provider.VerdictQuotaExhausted},
		{401, `{"error":{"code":"invalid_api_key"}}`, provider.VerdictAuthBroken},
		{403, `{"error":{"type":"authentication_error","message":"bad token"}}`, provider.VerdictAuthBroken},
		{403, `{"error":{"code":"invalid_token"}}`, provider.VerdictAuthBroken},
		{403, `<html>cloudflare edge block</html>`, provider.VerdictTransient},
		{400, `{"error":{"type":"invalid_request_error"}}`, provider.VerdictTransient},
		{500, "", provider.VerdictTransient},
		{529, "", provider.VerdictTransient},
	}
	for _, tc := range cases {
		got := p.Classify(&http.Response{StatusCode: tc.status}, []byte(tc.body))
		if got != tc.want {
			t.Errorf("status %d body %q: got %v, want %v", tc.status, tc.body, got, tc.want)
		}
	}
}

func TestValidate(t *testing.T) {
	var gotPath, gotQuery, gotAuth, gotAccount string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		gotAccount = r.Header.Get("ChatGPT-Account-ID")
		if r.Header.Get("Authorization") == "Bearer good" {
			_, _ = w.Write([]byte(`{"models":[]}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	p := New(srv.URL)
	cred := credJSON(t, Credential{AccessToken: "good", RefreshToken: "rt", AccountID: "acc"})
	if err := p.Validate(context.Background(), cred); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/models" || !strings.Contains(gotQuery, "client_version=") {
		t.Errorf("validate call: %s?%s", gotPath, gotQuery)
	}
	if gotAuth != "Bearer good" || gotAccount != "acc" {
		t.Errorf("auth headers: %q %q", gotAuth, gotAccount)
	}

	bad := credJSON(t, Credential{AccessToken: "bad", RefreshToken: "rt", AccountID: "acc"})
	if err := p.Validate(context.Background(), bad); err == nil {
		t.Error("expected rejection for bad token")
	}
}

type refreshScript struct {
	t       *testing.T
	handler func(body string) (int, string)
	calls   int
	bodies  []string
}

func (s *refreshScript) server() *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		s.calls++
		s.bodies = append(s.bodies, string(buf))
		status, body := s.handler(string(buf))
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	s.t.Cleanup(srv.Close)
	return srv
}

func TestRefreshRotation(t *testing.T) {
	script := &refreshScript{t: t, handler: func(body string) (int, string) {
		if !strings.Contains(body, `"grant_type":"refresh_token"`) ||
			!strings.Contains(body, `"refresh_token":"rt-old"`) ||
			!strings.Contains(body, `"client_id":"app_EMoamEEZ73f0CkXaXp7hrann"`) {
			t.Errorf("unexpected refresh body: %s", body)
		}
		return 200, `{"access_token":"at-new","refresh_token":"rt-new","id_token":"id-new"}`
	}}
	p := NewForTest("", script.server().URL)
	cred := credJSON(t, Credential{AccessToken: "at-old", RefreshToken: "rt-old", AccountID: "acc"})
	out, err := p.Refresh(context.Background(), cred)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseCredential(out)
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessToken != "at-new" || c.RefreshToken != "rt-new" {
		t.Errorf("rotation not applied: %+v", c)
	}
	if c.AccountID != "acc" {
		t.Errorf("account id lost: %+v", c)
	}
}

func TestRefreshKeepsOldRefreshTokenWhenAbsent(t *testing.T) {
	script := &refreshScript{t: t, handler: func(string) (int, string) {
		return 200, `{"access_token":"at-new"}`
	}}
	p := NewForTest("", script.server().URL)
	cred := credJSON(t, Credential{AccessToken: "at-old", RefreshToken: "rt-old", AccountID: "acc"})
	out, err := p.Refresh(context.Background(), cred)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := ParseCredential(out)
	if c.RefreshToken != "rt-old" {
		t.Errorf("refresh token should survive: %+v", c)
	}
}

func TestRefreshPermanentFailures(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"401", 401, `{}`},
		{"invalid_grant", 400, `{"error":{"code":"invalid_grant"}}`},
		{"expired", 400, `{"error":{"code":"refresh_token_expired"}}`},
		{"reused", 400, `{"error":"refresh_token_reused"}`},
		{"invalidated", 400, `{"code":"refresh_token_invalidated"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := &refreshScript{t: t, handler: func(string) (int, string) { return tc.status, tc.body }}
			p := NewForTest("", script.server().URL)
			cred := credJSON(t, Credential{AccessToken: "at", RefreshToken: "rt", AccountID: "acc"})
			_, err := p.Refresh(context.Background(), cred)
			var rerr *provider.RefreshError
			if !errors.As(err, &rerr) || !rerr.Permanent {
				t.Errorf("expected permanent RefreshError, got %v", err)
			}
		})
	}
}

func TestRefreshTransientFailure(t *testing.T) {
	script := &refreshScript{t: t, handler: func(string) (int, string) { return 500, `{}` }}
	p := NewForTest("", script.server().URL)
	cred := credJSON(t, Credential{AccessToken: "at", RefreshToken: "rt", AccountID: "acc"})
	_, err := p.Refresh(context.Background(), cred)
	var rerr *provider.RefreshError
	if !errors.As(err, &rerr) || rerr.Permanent {
		t.Errorf("expected transient RefreshError, got %v", err)
	}
}
