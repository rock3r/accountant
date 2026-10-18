package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"accountant/internal/config"
	"accountant/internal/router"
	"accountant/internal/state"
)

func hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// --- Pass-through -------------------------------------------------------

func TestPassThroughByteIdentical(t *testing.T) {
	r := newRig(t, nil)
	r.up.set("tok-a1", ok200("resp-body-\x00-binary-\xff-ok", map[string]string{
		"anthropic-ratelimit-unified-5h-utilization": "0.42",
		"anthropic-ratelimit-unified-5h-reset":       "1792260600",
		"anthropic-ratelimit-unified-status":         "allowed",
	}))
	rigAddAccount(t, r, "a1", 10)

	reqBody := []byte(`{"model":"claude-haiku-5-5","stream":true,"x":"` + strings.Repeat("é", 500) + `"}`)
	code, respBody, respHeaders := r.post(t, "/v1/messages", "sess-1", reqBody)
	if code != 200 {
		t.Fatalf("status %d body %s", code, respBody)
	}

	ups := r.up.requestsFor("tok-a1")
	if len(ups) != 1 {
		t.Fatalf("upstream requests: %d", len(ups))
	}
	if hash(ups[0].Body) != hash(reqBody) {
		t.Fatal("request body not byte-identical upstream")
	}
	if hash(respBody) != hash([]byte("resp-body-\x00-binary-\xff-ok")) {
		t.Fatal("response body not byte-identical to client")
	}
	// Unified response headers forwarded unchanged.
	if got := respHeaders.Get("anthropic-ratelimit-unified-5h-utilization"); got != "0.42" {
		t.Fatalf("unified header: %q", got)
	}
	if got := respHeaders.Get("anthropic-ratelimit-unified-5h-reset"); got != "1792260600" {
		t.Fatalf("unified reset header: %q", got)
	}
}

func TestCredentialHeadersReplacedAndBetaMerged(t *testing.T) {
	r := newRig(t, nil)
	r.up.set("tok-a1", ok200("ok", nil))
	rigAddAccount(t, r, "a1", 10)

	resp := r.doRequest(t, http.MethodPost, "/v1/messages", []byte(`{"m":1}`), map[string]string{
		"Authorization":  "Bearer " + r.clientKey,
		"x-api-key":      "",
		"anthropic-beta": "context-1m-2025-08-07",
	})
	_ = resp.Body.Close()
	ups := r.up.requestsFor("tok-a1")
	if len(ups) != 1 {
		t.Fatalf("upstream requests: %d", len(ups))
	}
	got := ups[0].Headers
	if auth := got.Get("Authorization"); auth != "Bearer tok-a1" {
		t.Fatalf("upstream auth = %q", auth)
	}
	if beta := got.Get("anthropic-beta"); beta != "context-1m-2025-08-07, oauth-2025-04-20" {
		t.Fatalf("beta = %q", beta)
	}

	// oauth already present: never duplicated, client values never dropped.
	r.up.reset()
	resp = r.doRequest(t, http.MethodPost, "/v1/messages", []byte(`{"m":1}`), map[string]string{
		"x-api-key":      r.clientKey,
		"anthropic-beta": "oauth-2025-04-20,claude-code-20250219",
	})
	_ = resp.Body.Close()
	ups = r.up.requestsFor("tok-a1")
	if beta := ups[0].Headers.Get("anthropic-beta"); beta != "oauth-2025-04-20,claude-code-20250219" {
		t.Fatalf("beta second request = %q", beta)
	}
	// The client key must never appear in any upstream header.
	for _, hdr := range ups[0].Headers {
		for _, v := range hdr {
			if strings.Contains(v, r.clientKey) {
				t.Fatal("client key leaked upstream")
			}
		}
	}
}

func TestHopByHopStripped(t *testing.T) {
	r := newRig(t, nil)
	r.up.set("tok-a1", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Connection", "x-hop")
		w.Header().Set("x-hop", "yes")
		w.Header().Set("x-real", "kept")
		_, _ = io.WriteString(w, "ok")
	})
	rigAddAccount(t, r, "a1", 10)

	req, _ := http.NewRequest(http.MethodPost, r.srv.URL+"/v1/messages", strings.NewReader("b"))
	req.Header.Set("x-api-key", r.clientKey)
	req.Header.Set("Connection", "x-secret-hop")
	req.Header.Set("x-secret-hop", "1")
	req.Header.Set("Keep-Alive", "timeout=5")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	ups := r.up.requestsFor("tok-a1")
	if ups[0].Headers.Get("x-secret-hop") != "" || ups[0].Headers.Get("Keep-Alive") != "" {
		t.Fatalf("hop-by-hop leaked upstream: %v", ups[0].Headers)
	}
	if resp.Header.Get("x-hop") != "" || resp.Header.Get("Connection") != "" {
		t.Fatalf("hop-by-hop leaked to client: %v", resp.Header)
	}
	if resp.Header.Get("x-real") != "kept" {
		t.Fatal("normal response header dropped")
	}
}

func TestAnthropicMountAndModels(t *testing.T) {
	r := newRig(t, nil)
	r.up.set("tok-a1", ok200(`{"data":[]}`, nil))
	rigAddAccount(t, r, "a1", 10)

	code, _, _ := r.post(t, "/anthropic/v1/messages", "s1", []byte(`{"m":1}`))
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	ups := r.up.requestsFor("tok-a1")
	if ups[0].Path != "/v1/messages" {
		t.Fatalf("mount prefix not stripped: %s", ups[0].Path)
	}
	resp := r.doRequest(t, http.MethodGet, "/v1/models", nil, nil)
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("models: %d", resp.StatusCode)
	}
}

func TestQueryStringForwarded(t *testing.T) {
	r := newRig(t, nil)
	r.up.set("tok-a1", ok200("ok", nil))
	rigAddAccount(t, r, "a1", 10)
	code, _, _ := r.post(t, "/v1/messages?beta=true", "s1", []byte(`{"m":1}`))
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if got := r.up.requestsFor("tok-a1")[0].Query; got != "beta=true" {
		t.Fatalf("query = %q", got)
	}
}

// --- Limits and safety --------------------------------------------------

func TestBodyOverLimit413(t *testing.T) {
	r := newRig(t, nil)
	rigAddAccount(t, r, "a1", 10)
	big := make([]byte, (32<<20)+1)
	code, body, _ := r.post(t, "/v1/messages", "s1", big)
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d", code)
	}
	if !strings.Contains(string(body), "request_too_large") {
		t.Fatalf("body %s", body)
	}
	if len(r.up.requests()) != 0 {
		t.Fatal("upstream must not be contacted")
	}
}

func TestBadClientKey401(t *testing.T) {
	r := newRig(t, nil)
	rigAddAccount(t, r, "a1", 10)

	resp := r.doRequest(t, http.MethodPost, "/v1/messages", []byte(`{"m":1}`), map[string]string{
		"x-api-key": "wrong-key",
	})
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}

	// Invalid x-api-key with a valid Bearer: no fall-through.
	resp = r.doRequest(t, http.MethodPost, "/v1/messages", []byte(`{"m":1}`), map[string]string{
		"x-api-key":     "wrong-key",
		"Authorization": "Bearer " + r.clientKey,
	})
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("fall-through must not happen: status %d", resp.StatusCode)
	}
	if len(r.up.requests()) != 0 {
		t.Fatal("upstream must not be contacted")
	}
}

func TestUnknownPath404(t *testing.T) {
	r := newRig(t, nil)
	resp := r.doRequest(t, http.MethodGet, "/nope", nil, nil)
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d", resp.StatusCode)
	}
	// GET on a POST-only path is also a 404.
	resp = r.doRequest(t, http.MethodGet, "/v1/messages", nil, nil)
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestNoEnabledAccounts503(t *testing.T) {
	r := newRig(t, nil)
	code, body, _ := r.post(t, "/v1/messages", "s1", []byte(`{"m":1}`))
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", code)
	}
	if !strings.Contains(string(body), "overloaded_error") {
		t.Fatalf("body %s", body)
	}
	if len(r.up.requests()) != 0 {
		t.Fatal("upstream must not be contacted")
	}
}

func TestHealthz(t *testing.T) {
	r := newRig(t, nil)
	resp := r.doRequest(t, http.MethodGet, "/healthz", nil, map[string]string{"x-api-key": "wrong"})
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "ok\n" {
		t.Fatalf("healthz: %d %q", resp.StatusCode, b)
	}
}

func TestTokenNeverInLogs(t *testing.T) {
	r := newRig(t, nil)
	r.up.set("tok-a1", statusWith(429, `{"type":"error","error":{"type":"rate_limit_error","message":"x"}}`, nil))
	r.up.set("tok-a2", ok200("ok", nil))
	rigAddAccount(t, r, "a1", 10)
	rigAddAccount(t, r, "a2", 20)

	code, _, _ := r.post(t, "/v1/messages", "sess-secret", []byte(`{"m":1}`))
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	logs := r.logBuf.String()
	for _, secret := range []string{"tok-a1", "tok-a2", r.clientKey, "sess-secret"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("log contains secret %q:\n%s", secret, logs)
		}
	}
}

// --- Routing ------------------------------------------------------------

func TestNewSessionsTakePriorityAccount(t *testing.T) {
	r := newRig(t, nil)
	r.up.set("tok-a1", ok200("from-a1", nil))
	r.up.set("tok-a2", ok200("from-a2", nil))
	rigAddAccount(t, r, "a1", 10)
	rigAddAccount(t, r, "a2", 20)

	// Policy: two brand-new sessions both start on account 1.
	for _, s := range []string{"s1", "s2"} {
		code, body, _ := r.post(t, "/v1/messages", s, []byte(`{"m":1}`))
		if code != 200 || string(body) != "from-a1" {
			t.Fatalf("%s: %d %s", s, code, body)
		}
	}
}

func TestParallelSessionsOnDifferentAccounts(t *testing.T) {
	r := newRig(t, nil)
	r.up.set("tok-a1", ok200("from-a1", nil))
	r.up.set("tok-a2", ok200("from-a2", nil))
	rigAddAccount(t, r, "a1", 10)
	rigAddAccount(t, r, "a2", 20)

	r.post(t, "/v1/messages", "s1", []byte(`{"m":1}`)) // pins a1
	rigCool(t, r, "a1", r.clock.Now().Add(time.Hour))  // pre-cool a1

	code, body, _ := r.post(t, "/v1/messages", "s2", []byte(`{"m":1}`))
	if code != 200 || string(body) != "from-a2" {
		t.Fatalf("s2: %d %s", code, body)
	}
	st := r.state(t)
	pins := map[string]string{}
	for _, p := range st.Pins {
		pins[p.SessionKey] = p.AccountID
	}
	if pins["s1"] != "a1" || pins["s2"] != "a2" {
		t.Fatalf("pins: %v", pins)
	}
}

func TestPinSurvivesDaemonRestart(t *testing.T) {
	r := newRig(t, nil)
	r.up.set("tok-a1", ok200("from-a1", nil))
	r.up.set("tok-a2", ok200("from-a2", nil))
	rigAddAccount(t, r, "a1", 10)
	rigAddAccount(t, r, "a2", 20)

	r.post(t, "/v1/messages", "s1", []byte(`{"m":1}`))
	rigCool(t, r, "a1", r.clock.Now().Add(time.Hour))
	code, _, _ := r.post(t, "/v1/messages", "s1", []byte(`{"m":1}`))
	if code != 200 {
		t.Fatalf("status %d", code)
	}

	// "Restart": new router+proxy over the same state and config.
	log := slog.New(slog.NewTextHandler(r.logBuf, nil))
	rt := router.New(r.cfgPath, r.store, r.clock, log)
	r.proxy = New(r.cfgPath, r.store.File(), rt, r.vault, r.proxy.providers, log)
	r.srv.Close()
	r.srv = httptest.NewServer(r.proxy.Handler())
	t.Cleanup(r.srv.Close)

	code, body, _ := r.post(t, "/v1/messages", "s1", []byte(`{"m":1}`))
	if code != 200 || string(body) != "from-a2" {
		t.Fatalf("after restart: %d %s (want a2)", code, body)
	}
}

func TestStaleSampleClearsAfterReset(t *testing.T) {
	r := newRig(t, nil)
	r.up.set("tok-a1", ok200("from-a1", unifiedHeaders("1.0", "0.0",
		strconv.FormatInt(r.clock.Now().Add(time.Hour).Unix(), 10),
		strconv.FormatInt(r.clock.Now().Add(24*time.Hour).Unix(), 10))))
	r.up.set("tok-a2", ok200("from-a2", nil))
	rigAddAccount(t, r, "a1", 10)
	rigAddAccount(t, r, "a2", 20)

	// a1's 2xx reads 100% on the 5h window: later requests avoid it.
	code, body, _ := r.post(t, "/v1/messages", "s1", []byte(`{"m":1}`))
	if code != 200 || string(body) != "from-a1" {
		t.Fatalf("s1: %d %s", code, body)
	}
	code, body, _ = r.post(t, "/v1/messages", "s2", []byte(`{"m":1}`))
	if code != 200 || string(body) != "from-a2" {
		t.Fatalf("s2: %d %s", code, body)
	}
	// Past the reset, the stale sample no longer blocks; a new session
	// takes the priority account again with no probe involved.
	r.up.reset()
	r.clock.Advance(2 * time.Hour)
	code, body, _ = r.post(t, "/v1/messages", "s3", []byte(`{"m":1}`))
	if code != 200 || string(body) != "from-a1" {
		t.Fatalf("s3: %d %s", code, body)
	}
	if got := len(r.up.requests()); got != 1 {
		t.Fatalf("s3 must go straight to a1 without a probe, upstream calls: %d", got)
	}
}

// helpers shared with failover tests

func rigAddAccount(t *testing.T, r *rig, id string, priority int) {
	t.Helper()
	cfg, err := config.Load(r.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Accounts = append(cfg.Accounts, acctCfg(id, priority))
	if err := config.Save(r.cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	if err := r.vault.Put(id, []byte("tok-"+id)); err != nil {
		t.Fatal(err)
	}
}

func rigCool(t *testing.T, r *rig, id string, until time.Time) {
	t.Helper()
	_ = r.store.Locked(func(st *state.State) (bool, error) {
		st.Account(id).CoolingUntil = until
		return true, nil
	})
}
