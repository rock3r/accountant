package proxy

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"accountant/internal/config"
	"accountant/internal/provider"
	"accountant/internal/provider/anthropic"
	"accountant/internal/router"
	"accountant/internal/state"
	"accountant/internal/vault"
)

// fakeClock is the test clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

var testStart = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

// recordedRequest captures exactly what the upstream received.
type recordedRequest struct {
	Method  string
	Path    string
	Query   string
	Headers http.Header
	Body    []byte
}

// upstream is a scripted fake Anthropic API: behavior is dispatched per
// Bearer token, exactly as the real upstream behaves per account.
type upstream struct {
	srv   *httptest.Server
	mu    sync.Mutex
	behav map[string]func(w http.ResponseWriter, r *http.Request)
	reqs  []recordedRequest
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{behav: map[string]func(w http.ResponseWriter, r *http.Request){}}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.reqs = append(u.reqs, recordedRequest{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
			Headers: r.Header.Clone(), Body: body,
		})
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		fn := u.behav[token]
		u.mu.Unlock()
		if fn != nil {
			fn(w, r)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_default","type":"message"}`))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstream) set(token string, fn func(w http.ResponseWriter, r *http.Request)) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.behav[token] = fn
}

func (u *upstream) requests() []recordedRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]recordedRequest(nil), u.reqs...)
}

func (u *upstream) requestsFor(token string) []recordedRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []recordedRequest
	for _, r := range u.reqs {
		if strings.TrimPrefix(r.Headers.Get("Authorization"), "Bearer ") == token {
			out = append(out, r)
		}
	}
	return out
}

func (u *upstream) reset() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.reqs = nil
}

// rig wires config, vault, router (fake clock), and proxy together.
type rig struct {
	t         *testing.T
	cfgPath   string
	store     *state.Store
	clock     *fakeClock
	vault     *vault.Memory
	up        *upstream
	srv       *httptest.Server
	logBuf    *bytes.Buffer
	proxy     *Proxy
	clientKey string
}

func acctCfg(id string, priority int) config.Account {
	en := true
	return config.Account{
		ID: id, Provider: "anthropic", Label: id, Priority: priority,
		Enabled: &en, CredentialRef: id,
	}
}

func newRig(t *testing.T, accounts []config.Account) *rig {
	t.Helper()
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
		if err := r.vault.Put(a.CredentialRef, []byte("tok-"+a.ID)); err != nil {
			t.Fatal(err)
		}
	}
	r.logBuf = &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(r.logBuf, nil))
	rt := router.New(r.cfgPath, r.store, r.clock, log)
	r.proxy = New(r.cfgPath, rt, r.vault, map[string]provider.Provider{
		"anthropic": anthropic.New(r.up.srv.URL),
	}, log)
	r.srv = httptest.NewServer(r.proxy.Handler())
	t.Cleanup(r.srv.Close)
	return r
}

// doRequest performs one client request through the proxy.
func (r *rig) doRequest(t *testing.T, method, path string, body []byte, headers map[string]string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, r.srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := headers["x-api-key"]; !ok {
		if _, ok := headers["Authorization"]; !ok {
			req.Header.Set("x-api-key", r.clientKey)
		}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (r *rig) post(t *testing.T, path, session string, body []byte) (int, []byte, http.Header) {
	t.Helper()
	h := map[string]string{}
	if session != "" {
		h["x-claude-code-session-id"] = session
	}
	resp := r.doRequest(t, http.MethodPost, path, body, h)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b, resp.Header
}

func (r *rig) state(t *testing.T) *state.State {
	t.Helper()
	st, err := state.Load(r.store.File())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func ok200(body string, headers map[string]string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

func statusWith(status int, body string, headers map[string]string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func unifiedHeaders(util5h, util7d, reset5h, reset7d string) map[string]string {
	return map[string]string{
		"anthropic-ratelimit-unified-5h-utilization": util5h,
		"anthropic-ratelimit-unified-5h-reset":       reset5h,
		"anthropic-ratelimit-unified-7d-utilization": util7d,
		"anthropic-ratelimit-unified-7d-reset":       reset7d,
		"anthropic-ratelimit-unified-status":         "allowed",
	}
}
