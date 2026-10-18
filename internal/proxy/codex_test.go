package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"accountant/internal/config"
	"accountant/internal/provider"
	"accountant/internal/provider/codex"
	"accountant/internal/router"
	"accountant/internal/state"
	"accountant/internal/vault"
)

// codexCred builds a vault credential with a plain (non-JWT) access token,
// so no proactive refresh triggers.
func codexCred(access, accountID string) []byte {
	b, _ := json.Marshal(codex.Credential{AccessToken: access, RefreshToken: "rt-" + access, AccountID: accountID})
	return b
}

// codexJWTCred builds a vault credential whose access token expires at exp.
func codexJWTCred(t *testing.T, accessID string, exp time.Time, accountID string) []byte {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, _ := json.Marshal(map[string]any{"exp": exp.Unix()})
	jwt := header + "." + base64.RawURLEncoding.EncodeToString(payload) + "." + accessID
	b, _ := json.Marshal(codex.Credential{AccessToken: jwt, RefreshToken: "rt-" + accessID, AccountID: accountID})
	return b
}

func codexAcctCfg(id string, priority int) config.Account {
	en := true
	return config.Account{
		ID: id, Provider: "codex", Label: id, Priority: priority,
		Enabled: &en, CredentialRef: id,
	}
}

// codexRig wires a proxy with the codex provider pointed at the shared
// fake upstream, plus an optional fake refresh endpoint.
type codexRig struct {
	t           *testing.T
	rig         *rig
	refreshHits atomic.Int32
}

func newCodexRig(t *testing.T, accounts []config.Account, creds map[string][]byte, refreshFn func(body string) (int, string)) *codexRig {
	t.Helper()
	cr := &codexRig{t: t}
	var refreshURL string
	if refreshFn != nil {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cr.refreshHits.Add(1)
			body, _ := io.ReadAll(r.Body)
			status, respBody := refreshFn(string(body))
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, respBody)
		}))
		t.Cleanup(srv.Close)
		refreshURL = srv.URL
	}

	dir := t.TempDir()
	r := &rig{t: t, clock: newFakeClock(testStart), vault: vault.NewMemory(), clientKey: "test-client-key-1"}
	r.up = newUpstream(t)
	r.store = state.NewStore(dir + "/state.json")
	r.cfgPath = dir + "/config.toml"
	cfg := &config.Config{
		Accounts: accounts,
		Clients:  []config.Client{{ID: "c1", KeySHA256: HashKey(r.clientKey)}},
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(r.cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	for _, a := range accounts {
		cred, ok := creds[a.CredentialRef]
		if !ok {
			cred = codexCred("tok-"+a.ID, "acct-"+a.ID)
		}
		if err := r.vault.Put(a.CredentialRef, cred); err != nil {
			t.Fatal(err)
		}
	}
	r.logBuf = &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(r.logBuf, nil))
	rt := router.New(r.cfgPath, r.store, r.clock, log)
	r.proxy = New(r.cfgPath, r.store.File(), rt, r.vault, map[string]provider.Provider{
		"codex": codex.NewForTest(r.up.srv.URL, refreshURL),
	}, log)
	r.srv = httptest.NewServer(r.proxy.Handler())
	t.Cleanup(r.srv.Close)
	cr.rig = r
	return cr
}

// codexHeaders sets the codex session header and client key.
func (r *codexRig) post(t *testing.T, path, session string, body []byte) (int, []byte, http.Header) {
	t.Helper()
	h := map[string]string{}
	if session != "" {
		h["session-id"] = session
	}
	resp := r.rig.doRequest(t, http.MethodPost, path, body, h)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b, resp.Header
}

func TestCodexResponsesMountedAndHeadersRewritten(t *testing.T) {
	accts := []config.Account{codexAcctCfg("cx-a", 10)}
	cr := newCodexRig(t, accts, nil, nil)
	cr.rig.up.set("tok-cx-a", ok200(`{"id":"resp_1"}`, map[string]string{
		"x-codex-primary-used-percent":   "12",
		"x-codex-primary-window-minutes": "300",
	}))

	body := []byte(`{"model":"gpt-test","stream":true}`)
	status, got, _ := cr.post(t, "/codex/responses", "sess-1", body)
	if status != 200 {
		t.Fatalf("status %d body %s", status, got)
	}
	reqs := cr.rig.up.requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream got %d requests", len(reqs))
	}
	up := reqs[0]
	if up.Path != "/responses" {
		t.Errorf("upstream path %q", up.Path)
	}
	if got := up.Headers.Get("Authorization"); got != "Bearer tok-cx-a" {
		t.Errorf("Authorization %q", got)
	}
	if got := up.Headers.Get("ChatGPT-Account-ID"); got != "acct-cx-a" {
		t.Errorf("ChatGPT-Account-ID %q", got)
	}
	if got := up.Headers.Get("x-api-key"); got != "" {
		t.Errorf("client key leaked upstream: %q", got)
	}
	if !bytes.Equal(up.Body, body) {
		t.Errorf("body not byte-identical: %q", up.Body)
	}
	if got := up.Headers.Get("session-id"); got != "sess-1" {
		t.Errorf("session header not forwarded: %q", got)
	}

	// Quota headers landed in state (percent normalized to fraction).
	st := cr.rig.state(t)
	as := st.Accounts["cx-a"]
	if as == nil || !as.FiveHour.HasSample {
		t.Fatalf("no quota sample recorded: %+v", as)
	}
	if as.FiveHour.Utilization != 0.12 {
		t.Errorf("utilization %v, want 0.12", as.FiveHour.Utilization)
	}
}

// TestCodexModelsForwarded covers the live model-discovery path: the CLI
// calls GET {base}/models?client_version=... and the proxy must forward
// path and query unchanged with the account credential swapped in.
func TestCodexModelsForwarded(t *testing.T) {
	accts := []config.Account{codexAcctCfg("cx-a", 10)}
	cr := newCodexRig(t, accts, nil, nil)
	catalog := `{"models":[{"slug":"gpt-test","supported_in_api":true,"visibility":"list"}]}`
	cr.rig.up.set("tok-cx-a", ok200(catalog, map[string]string{
		"x-codex-primary-used-percent":   "59",
		"x-codex-primary-window-minutes": "10080",
		"x-codex-primary-reset-at":       itoa(testStart.Add(72 * time.Hour).Unix()),
	}))

	resp := cr.rig.doRequest(t, http.MethodGet, "/codex/models?client_version=0.163.0", nil, map[string]string{})
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if string(body) != catalog {
		t.Errorf("catalog not byte-identical: %s", body)
	}
	reqs := cr.rig.up.requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream got %d requests", len(reqs))
	}
	if reqs[0].Path != "/models" || reqs[0].Query != "client_version=0.163.0" {
		t.Errorf("upstream got %s?%s", reqs[0].Path, reqs[0].Query)
	}
	if got := reqs[0].Headers.Get("ChatGPT-Account-ID"); got != "acct-cx-a" {
		t.Errorf("ChatGPT-Account-ID %q", got)
	}

	// Weekly window in the primary position lands in the 7d slot.
	st := cr.rig.state(t)
	as := st.Accounts["cx-a"]
	if as == nil || !as.SevenDay.HasSample || as.SevenDay.Utilization != 0.59 {
		t.Errorf("weekly sample not recorded: %+v", as)
	}
	if as != nil && as.FiveHour.HasSample {
		t.Errorf("5h slot should be empty: %+v", as.FiveHour)
	}
}

// TestCodexPercentScaleConfigNoDoubleNormalize: with utilization_scale =
// "percent" configured, a Codex window that reads 100% must still block the
// account. Codex samples are normalized by the provider, so the router must
// not apply the configured scale to them again.
func TestCodexPercentScaleConfigNoDoubleNormalize(t *testing.T) {
	accts := []config.Account{codexAcctCfg("cx-a", 10), codexAcctCfg("cx-b", 20)}
	cr := newCodexRig(t, accts, nil, nil)
	cfg, err := config.Load(cr.rig.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.UtilizationScale = "percent"
	if err := config.Save(cr.rig.cfgPath, cfg); err != nil {
		t.Fatal(err)
	}

	resetAt := testStart.Add(3 * time.Hour).Unix()
	cr.rig.up.set("tok-cx-a", ok200(`{"id":"ra"}`, map[string]string{
		"x-codex-primary-used-percent":   "100",
		"x-codex-primary-window-minutes": "300",
		"x-codex-primary-reset-at":       itoa(resetAt),
	}))
	cr.rig.up.set("tok-cx-b", ok200(`{"id":"rb"}`, nil))

	// A 2xx whose headers read 100% marks the account ineligible for later
	// requests (without disturbing this one).
	status, _, _ := cr.post(t, "/codex/responses", "sess-1", []byte(`{}`))
	if status != 200 {
		t.Fatalf("status %d", status)
	}
	st := cr.rig.state(t)
	if router.Eligible(st.Accounts["cx-a"], testStart, "percent") {
		t.Error("cx-a at 100% must be ineligible under utilization_scale=percent")
	}

	// The session's next request re-selects and lands on cx-b.
	status, got, _ := cr.post(t, "/codex/responses", "sess-1", []byte(`{}`))
	if status != 200 || string(got) != `{"id":"rb"}` {
		t.Errorf("expected failover to cx-b, got %d %s", status, got)
	}
}

func TestCodexUnknownPath404(t *testing.T) {
	cr := newCodexRig(t, []config.Account{codexAcctCfg("cx-a", 10)}, nil, nil)
	status, _, _ := cr.post(t, "/codex/chat", "s", []byte(`{}`))
	if status != http.StatusNotFound {
		t.Fatalf("status %d, want 404", status)
	}
}

func TestCodexQuota429Failover(t *testing.T) {
	accts := []config.Account{codexAcctCfg("cx-a", 10), codexAcctCfg("cx-b", 20)}
	cr := newCodexRig(t, accts, nil, nil)
	resetAt := testStart.Add(3 * time.Hour).Unix()
	cr.rig.up.set("tok-cx-a", statusWith(429, `{"error":{"type":"usage_limit_reached"}}`, map[string]string{
		"x-codex-primary-used-percent":   "100",
		"x-codex-primary-window-minutes": "300",
		"x-codex-primary-reset-at":       itoa(resetAt),
	}))
	cr.rig.up.set("tok-cx-b", ok200(`{"id":"resp_b"}`, nil))

	status, got, _ := cr.post(t, "/codex/responses", "sess-1", []byte(`{"model":"m"}`))
	if status != 200 {
		t.Fatalf("status %d body %s", status, got)
	}
	if string(got) != `{"id":"resp_b"}` {
		t.Errorf("body from wrong account: %s", got)
	}

	st := cr.rig.state(t)
	asA := st.Accounts["cx-a"]
	if asA == nil || !asA.CoolingUntil.Equal(time.Unix(resetAt, 0).UTC()) {
		t.Errorf("cx-a should cool until reset: %+v", asA)
	}
	if len(cr.rig.up.requestsFor("tok-cx-a")) != 1 || len(cr.rig.up.requestsFor("tok-cx-b")) != 1 {
		t.Errorf("expected one request per account")
	}

	// Next request of the same session goes straight to cx-b.
	cr.rig.up.reset()
	status, _, _ = cr.post(t, "/codex/responses", "sess-1", []byte(`{"model":"m"}`))
	if status != 200 {
		t.Fatalf("status %d", status)
	}
	if n := len(cr.rig.up.requestsFor("tok-cx-a")); n != 0 {
		t.Errorf("cooling account got %d more requests", n)
	}
}

func TestCodexAllExhaustedProbePassesThrough(t *testing.T) {
	accts := []config.Account{codexAcctCfg("cx-a", 10)}
	cr := newCodexRig(t, accts, nil, nil)
	body429 := `{"error":{"type":"usage_limit_reached","resets_at":1792699999}}`
	cr.rig.up.set("tok-cx-a", statusWith(429, body429, map[string]string{
		"x-codex-primary-used-percent":   "100",
		"x-codex-primary-window-minutes": "300",
		"x-codex-primary-reset-at":       itoa(testStart.Add(3 * time.Hour).Unix()),
	}))

	// First request: 429 fails over, no account left → passed through.
	status, got, _ := cr.post(t, "/codex/responses", "sess-1", []byte(`{}`))
	if status != 429 {
		t.Fatalf("status %d, want 429", status)
	}
	if string(got) != body429 {
		t.Errorf("429 body not passed through unchanged: %s", got)
	}

	// Second request: zero eligible → probe once, response unchanged.
	status, got, _ = cr.post(t, "/codex/responses", "sess-2", []byte(`{}`))
	if status != 429 || string(got) != body429 {
		t.Errorf("probe: status %d body %s", status, got)
	}
	if n := len(cr.rig.up.requestsFor("tok-cx-a")); n != 2 {
		t.Errorf("probe should add exactly one upstream request, total %d", n)
	}
}

func TestCodexRefreshBeforeRequest(t *testing.T) {
	accts := []config.Account{codexAcctCfg("cx-a", 10)}
	expired := codexJWTCred(t, "old", testStart.Add(-time.Hour), "acct-cx-a")
	var sawRefresh atomic.Int32
	cr := newCodexRig(t, accts, map[string][]byte{"cx-a": expired}, func(body string) (int, string) {
		sawRefresh.Add(1)
		if !strings.Contains(body, `"refresh_token":"rt-old"`) {
			t.Errorf("refresh body missing old refresh token: %s", body)
		}
		// Rotated tokens; the new access token is a JWT valid for 10 days.
		header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
		payload, _ := json.Marshal(map[string]any{"exp": testStart.Add(240 * time.Hour).Unix()})
		jwt := header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".new"
		return 200, `{"access_token":"` + jwt + `","refresh_token":"rt-new"}`
	})
	cr.rig.up.set("tok-cx-a", statusWith(401, `{"error":{"code":"invalid_api_key"}}`, nil))

	status, _, _ := cr.post(t, "/codex/responses", "sess-1", []byte(`{}`))
	if status != 200 {
		t.Fatalf("status %d (old token must never reach upstream)", status)
	}
	if got := sawRefresh.Load(); got != 1 {
		t.Errorf("refresh calls: %d, want 1", got)
	}
	reqs := cr.rig.up.requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream requests: %d", len(reqs))
	}
	if got := reqs[0].Headers.Get("Authorization"); !strings.HasSuffix(got, ".new") {
		t.Errorf("upstream did not see refreshed token: %q", got)
	}

	// The vault now holds the rotated credential.
	stored, err := cr.rig.vault.Get("cx-a")
	if err != nil {
		t.Fatal(err)
	}
	c, err := codex.ParseCredential(stored)
	if err != nil {
		t.Fatal(err)
	}
	if c.RefreshToken != "rt-new" || !strings.HasSuffix(c.AccessToken, ".new") {
		t.Errorf("vault not updated: %+v", c)
	}
}

func TestCodexRefreshSingleFlight(t *testing.T) {
	accts := []config.Account{codexAcctCfg("cx-a", 10)}
	expired := codexJWTCred(t, "old", testStart.Add(-time.Hour), "acct-cx-a")
	cr := newCodexRig(t, accts, map[string][]byte{"cx-a": expired}, func(body string) (int, string) {
		time.Sleep(50 * time.Millisecond) // widen the race window
		header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
		payload, _ := json.Marshal(map[string]any{"exp": testStart.Add(240 * time.Hour).Unix()})
		jwt := header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".new"
		return 200, `{"access_token":"` + jwt + `"}`
	})

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, _, _ := cr.post(t, "/codex/responses", "sess-shared", []byte(`{}`))
			if status != 200 {
				t.Errorf("status %d", status)
			}
		}()
	}
	wg.Wait()
	if got := cr.refreshHits.Load(); got != 1 {
		t.Errorf("refresh calls: %d, want 1 (single-flight)", got)
	}
}

func TestCodexRefreshPermanentFailureMarksAuthBroken(t *testing.T) {
	accts := []config.Account{codexAcctCfg("cx-a", 10), codexAcctCfg("cx-b", 20)}
	expired := codexJWTCred(t, "old", testStart.Add(-time.Hour), "acct-cx-a")
	cr := newCodexRig(t, accts, map[string][]byte{"cx-a": expired}, func(string) (int, string) {
		return 400, `{"error":{"code":"invalid_grant"}}`
	})
	cr.rig.up.set("tok-cx-b", ok200(`{"id":"resp_b"}`, nil))

	status, got, _ := cr.post(t, "/codex/responses", "sess-1", []byte(`{}`))
	if status != 200 {
		t.Fatalf("status %d body %s", status, got)
	}
	st := cr.rig.state(t)
	if as := st.Accounts["cx-a"]; as == nil || !as.AuthBroken {
		t.Errorf("cx-a should be auth-broken: %+v", as)
	}
	if n := len(cr.rig.up.requestsFor("tok-cx-a")); n != 0 {
		t.Errorf("dead credential reached upstream %d times", n)
	}
}

func TestCodexRefreshTransientFailureFailsOverWithoutMarking(t *testing.T) {
	accts := []config.Account{codexAcctCfg("cx-a", 10), codexAcctCfg("cx-b", 20)}
	expired := codexJWTCred(t, "old", testStart.Add(-time.Hour), "acct-cx-a")
	cr := newCodexRig(t, accts, map[string][]byte{"cx-a": expired}, func(string) (int, string) {
		return 500, `{}`
	})
	cr.rig.up.set("tok-cx-b", ok200(`{"id":"resp_b"}`, nil))

	status, _, _ := cr.post(t, "/codex/responses", "sess-1", []byte(`{}`))
	if status != 200 {
		t.Fatalf("status %d", status)
	}
	st := cr.rig.state(t)
	if as := st.Accounts["cx-a"]; as != nil && as.AuthBroken {
		t.Errorf("transient refresh failure must not mark auth-broken: %+v", as)
	}
}

func TestCodexNoRefreshWhenTokenFresh(t *testing.T) {
	accts := []config.Account{codexAcctCfg("cx-a", 10)}
	// Expiry is relative to the wall clock: production freshness checks
	// compare against time.Now(), not the rig's frozen clock.
	fresh := codexJWTCred(t, "fresh", time.Now().Add(240*time.Hour), "acct-cx-a")
	cr := newCodexRig(t, accts, map[string][]byte{"cx-a": fresh}, func(string) (int, string) {
		return 200, `{}`
	})
	cr.rig.up.set("", ok200(`{"id":"resp"}`, nil))

	status, _, _ := cr.post(t, "/codex/responses", "sess-1", []byte(`{}`))
	if status != 200 {
		t.Fatalf("status %d", status)
	}
	if got := cr.refreshHits.Load(); got != 0 {
		t.Errorf("refresh calls: %d, want 0 for a fresh token", got)
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
