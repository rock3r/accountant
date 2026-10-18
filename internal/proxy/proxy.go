// Package proxy is the Anthropic-format pass-through on loopback. It swaps
// the client's key for the chosen account's token and streams bytes both
// ways without re-encoding. Bodies pass through byte-identical, except for
// clients flagged claude_code_compat whose request body is reshaped by the
// provider's RequestShaper. It never logs credentials or bodies.
package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"accountant/internal/config"
	"accountant/internal/provider"
	"accountant/internal/refresh"
	"accountant/internal/router"
	"accountant/internal/vault"
)

const (
	// errorPrefixLimit is the bounded non-2xx prefix read before anything
	// is sent to the client; it feeds Classify and keeps the failover
	// window open.
	errorPrefixLimit = 64 * 1024
	// errorPrefixDeadline bounds how long the failover decision stays open
	// on a non-2xx response.
	errorPrefixDeadline = 30 * time.Second
	// failoverDrainLimit bounds how much of a failed response body is
	// drained before replaying, so upstream connections stay reusable.
	failoverDrainLimit = 1 << 20
)

// Proxy serves the client-facing HTTP API.
type Proxy struct {
	cfgPath   string
	stateFile string
	router    *router.Router
	vault     vault.Vault
	providers map[string]provider.Provider
	log       *slog.Logger

	mu           sync.Mutex
	httpClient   *http.Client
	clientTimout int // seconds; rebuild transport when config changes

	// refreshMu guards refreshes: in-flight credential refreshes,
	// single-flight per vault ref.
	refreshMu sync.Mutex
	refreshes map[string]*refreshCall

	// prefixDeadline overrides errorPrefixDeadline when > 0 (tests).
	prefixDeadline time.Duration
}

// New creates a Proxy.
func New(cfgPath, stateFile string, r *router.Router, v vault.Vault, providers map[string]provider.Provider, log *slog.Logger) *Proxy {
	return &Proxy{cfgPath: cfgPath, stateFile: stateFile, router: r, vault: v, providers: providers, log: log, refreshes: map[string]*refreshCall{}}
}

// hopByHop are stripped in both directions; headers named by the Connection
// header are stripped too.
var hopByHop = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"TE", "Trailer", "Transfer-Encoding", "Upgrade",
}

func stripHopByHop(h http.Header) {
	for _, k := range h["Connection"] {
		for _, tok := range strings.Split(k, ",") {
			h.Del(strings.TrimSpace(tok))
		}
	}
	for _, k := range hopByHop {
		h.Del(k)
	}
}

// Handler returns the HTTP handler with the full routing table.
func (p *Proxy) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/plain")
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/", p.route)
	return mux
}

// route dispatches exactly the served endpoints; everything else is a 404.
func (p *Proxy) route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	providerID := ""
	if rest, ok := strings.CutPrefix(path, "/anthropic/"); ok {
		providerID = "anthropic"
		path = "/" + rest
	} else if rest, ok := strings.CutPrefix(path, "/codex/"); ok {
		providerID = "codex"
		path = "/" + rest
	}
	var served bool
	switch providerID {
	case "codex":
		served = path == "/responses" && r.Method == http.MethodPost ||
			path == "/models" && r.Method == http.MethodGet
	default:
		served = path == "/v1/messages" && r.Method == http.MethodPost ||
			path == "/v1/messages/count_tokens" && r.Method == http.MethodPost ||
			path == "/v1/models" && r.Method == http.MethodGet
	}
	if !served {
		writeAPIError(w, http.StatusNotFound, "not_found_error", "unknown path")
		return
	}
	if providerID == "" {
		cfg, err := config.Load(p.cfgPath)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "api_error", "invalid configuration")
			return
		}
		providerID = cfg.DefaultProvider
	}
	p.serve(w, r, providerID, path)
}

func (p *Proxy) serve(w http.ResponseWriter, r *http.Request, providerID, path string) {
	start := time.Now()
	prov, ok := p.providers[providerID]
	if !ok {
		writeAPIError(w, http.StatusBadGateway, "api_error", "provider not available")
		return
	}
	cfg, err := config.Load(p.cfgPath)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "api_error", "invalid configuration")
		return
	}

	// Client key check: x-api-key if present, else the Bearer token; an
	// invalid chosen key is a 401 with no fall-through to the other header.
	client, ok := checkClientKey(r, cfg)
	if !ok {
		writeAPIError(w, http.StatusUnauthorized, "authentication_error", "invalid client key")
		return
	}

	// Buffer the request body (bounded) so a pre-byte failover can replay
	// it; over the bound, fail with 413 before contacting upstream.
	limit := cfg.RequestBodyLimitMiB << 20
	var body []byte
	if r.Body != nil && (r.Method == http.MethodPost || r.Method == http.MethodPut) {
		if r.ContentLength > limit {
			writeAPIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body over the configured limit")
			return
		}
		body, err = io.ReadAll(io.LimitReader(r.Body, limit+1))
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "could not read request body")
			return
		}
		if int64(len(body)) > limit {
			writeAPIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body over the configured limit")
			return
		}
	}

	// Clients flagged claude_code_compat get their body reshaped once,
	// before the failover loop, so every attempt sends the same bytes.
	if client.ClaudeCodeCompat && len(body) > 0 {
		if shaper, ok := prov.(provider.RequestShaper); ok {
			shaped, err := shaper.ShapeRequest(body)
			if err != nil {
				p.log.Info("request shaping failed, passing through", "client", client.ID, "error", err.Error())
			} else {
				body = shaped
			}
		}
	}

	sessionKey := prov.SessionKey(r)
	if sessionKey == "" {
		sessionKey = "client:" + client.ID
	}
	logAttrs := []any{"session", router.HashSession(sessionKey), "path", path}

	dec, err := p.router.Acquire(providerID, sessionKey)
	if err != nil {
		p.log.Error("routing failed", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "api_error", "routing failed")
		return
	}
	if dec.NoneEnabled {
		// No enabled accounts: local 503 without contacting upstream.
		writeAPIError(w, http.StatusServiceUnavailable, "overloaded_error", "no enabled accounts")
		return
	}

	tried := map[string]bool{}
	for {
		acct := dec.Account
		tried[acct.ID] = true
		cred, err := p.vault.Get(acct.CredentialRef)
		if err != nil {
			p.log.Error("credential unavailable", "account", acct.ID)
			writeAPIError(w, http.StatusInternalServerError, "api_error", "credential unavailable")
			return
		}

		// Providers with expiring credentials (Codex OAuth) refresh
		// proactively, before any upstream call. A failed refresh advances
		// to the next account without contacting upstream; a permanent
		// failure also takes the account out of rotation.
		cred, err = p.refreshIfNeeded(r.Context(), prov, acct, cred)
		if err != nil {
			var rerr *provider.RefreshError
			permanent := errors.As(err, &rerr) && rerr.Permanent
			if permanent {
				if merr := p.router.MarkAuthBroken(acct.ID); merr != nil {
					p.log.Error("marking auth-broken failed", "error", merr)
				}
			}
			p.log.Warn("credential refresh failed", append(logAttrs, "account", acct.ID,
				"permanent", permanent, "error", err.Error())...)
			if !dec.Probe {
				next, ok, ferr := p.router.Failover(providerID, sessionKey, acct.ID, tried)
				if ferr != nil {
					p.log.Error("failover failed", "error", ferr)
				}
				if ok {
					p.log.Info("failing over", append(logAttrs, "from", acct.ID, "to", next.ID,
						"verdict", "refresh_failed")...)
					dec.Account = next
					continue
				}
			}
			writeAPIError(w, http.StatusBadGateway, "api_error", "account credential refresh failed")
			return
		}

		upReq, err := p.buildUpstreamRequest(r, prov, path, body)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "api_error", "could not build upstream request")
			return
		}
		if err := prov.RewriteRequest(upReq, cred); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "api_error", "could not prepare upstream request")
			return
		}

		resp, err := p.upstreamClient(cfg).Do(upReq)
		if err != nil {
			if errors.Is(err, context.Canceled) || r.Context().Err() != nil {
				return // client went away
			}
			// Response-header timeout or network error: Transient, never
			// advances. Local 504 so the client retries.
			p.log.Info("upstream request failed", append(logAttrs, "account", acct.ID, "error", err.Error())...)
			writeAPIError(w, http.StatusGatewayTimeout, "api_error", "upstream did not respond in time")
			return
		}

		snap, hasSnap := prov.QuotaFromResponse(resp)

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if err := p.router.Record(acct.ID, resp, snap, hasSnap, provider.VerdictOK); err != nil {
				p.log.Error("state record failed", "error", err)
			}
			if dec.Probe {
				if err := p.router.PinSession(providerID, sessionKey, acct.ID); err != nil {
					p.log.Error("pin failed", "error", err)
				}
			}
			p.log.Info("request served", append(logAttrs, "account", acct.ID, "status", resp.StatusCode,
				"duration_ms", time.Since(start).Milliseconds(), "probe", dec.Probe)...)
			writeResponseHead(w, resp)
			streamCopy(w, resp.Body)
			_ = resp.Body.Close()
			return
		}

		// Non-2xx: read a bounded prefix before sending anything, so the
		// failover window stays open.
		prefix, timedOut, err := readPrefix(r.Context(), resp.Body, errorPrefixLimit, p.prefixDeadlineOrDefault())
		if err != nil {
			_ = resp.Body.Close()
			return // client went away
		}
		verdict := provider.VerdictTransient // deadline passed: Transient
		if !timedOut {
			verdict = prov.Classify(resp, prefix)
		}
		if err := p.router.Record(acct.ID, resp, snap, hasSnap, verdict); err != nil {
			p.log.Error("state record failed", "error", err)
		}

		if (verdict == provider.VerdictQuotaExhausted || verdict == provider.VerdictAuthBroken) && !dec.Probe {
			next, ok, err := p.router.Failover(providerID, sessionKey, acct.ID, tried)
			if err != nil {
				p.log.Error("failover failed", "error", err)
			}
			if ok {
				drainClose(resp.Body, failoverDrainLimit)
				p.log.Info("failing over", append(logAttrs, "from", acct.ID, "to", next.ID,
					"verdict", verdict.String())...)
				dec.Account = next
				continue
			}
		}

		// Pass-through: buffered prefix verbatim first, then the remainder.
		p.log.Info("response passed through", append(logAttrs, "account", acct.ID, "status", resp.StatusCode,
			"verdict", verdict.String(), "probe", dec.Probe, "duration_ms", time.Since(start).Milliseconds())...)
		writeResponseHead(w, resp)
		if len(prefix) > 0 {
			if _, err := w.Write(prefix); err != nil {
				_ = resp.Body.Close()
				return
			}
			flush(w)
		}
		streamCopy(w, resp.Body)
		_ = resp.Body.Close()
		return
	}
}

// buildUpstreamRequest clones the inbound request for the provider's
// upstream: method, path, query, remaining headers, and the body
// byte-for-byte.
func (p *Proxy) buildUpstreamRequest(in *http.Request, prov provider.Provider, path string, body []byte) (*http.Request, error) {
	url := prov.UpstreamBaseURL() + path
	if in.URL.RawQuery != "" {
		url += "?" + in.URL.RawQuery
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	up, err := http.NewRequestWithContext(in.Context(), in.Method, url, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		up.ContentLength = int64(len(body))
	}
	up.Header = in.Header.Clone()
	stripHopByHop(up.Header)
	up.Header.Del("Content-Length") // Go sets it from ContentLength
	return up, nil
}

// refreshMargin is how long before credential expiry the proxy refreshes.
const refreshMargin = 30 * time.Minute

// refreshTimeout bounds one OAuth refresh round-trip.
const refreshTimeout = 30 * time.Second

// refreshCall is one in-flight single-flight refresh.
type refreshCall struct {
	done chan struct{}
	cred []byte
	err  error
}

// refreshIfNeeded returns a usable credential for the account, refreshing
// single-flight when the provider reports an expiry within refreshMargin.
// Providers without CredentialExpirer/Refresher get their credential back
// unchanged. The refreshed credential is written to the vault before any
// request uses it.
func (p *Proxy) refreshIfNeeded(ctx context.Context, prov provider.Provider, acct config.Account, cred []byte) ([]byte, error) {
	exp, ok := prov.(provider.CredentialExpirer)
	if !ok {
		return cred, nil
	}
	expiry, ok := exp.CredentialExpiry(cred)
	if !ok || expiry.After(time.Now().Add(refreshMargin)) {
		return cred, nil
	}
	if _, ok := prov.(provider.Refresher); !ok {
		return cred, nil
	}
	p.log.Info("refreshing credential", "account", acct.ID, "expired", expiry.Format(time.RFC3339))
	return p.refreshSingleFlight(ctx, prov, acct.CredentialRef)
}

// refreshSingleFlight coalesces concurrent refreshes of one vault ref
// in-process. The leader runs refresh.IfNeeded, which re-reads the vault
// under the shared per-credential lock: a worker (or CLI command) that
// arrives after another refresh completed finds a fresh credential and
// makes no second exchange. This matters because Codex refresh tokens
// rotate — a duplicate exchange fails as refresh_token_reused.
func (p *Proxy) refreshSingleFlight(ctx context.Context, prov provider.Provider, ref string) ([]byte, error) {
	p.refreshMu.Lock()
	if c, ok := p.refreshes[ref]; ok {
		p.refreshMu.Unlock()
		select {
		case <-c.done:
			return c.cred, c.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c := &refreshCall{done: make(chan struct{})}
	p.refreshes[ref] = c
	p.refreshMu.Unlock()

	// The refresh outlives a client disconnect: a cancelled client request
	// must not waste a rotating refresh token round-trip.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
	cred, _, err := refresh.IfNeeded(rctx, p.vault, prov, p.stateFile, ref, refreshMargin)
	cancel()

	p.refreshMu.Lock()
	delete(p.refreshes, ref)
	p.refreshMu.Unlock()
	c.cred, c.err = cred, err
	close(c.done)
	return c.cred, c.err
}

// upstreamClient returns the shared HTTP client, rebuilding the transport
// when the configured response-header timeout changed.
func (p *Proxy) upstreamClient(cfg *config.Config) *http.Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.httpClient != nil && p.clientTimout == cfg.ResponseHeaderTimeoutSeconds {
		return p.httpClient
	}
	tr := &http.Transport{
		// Never add or transparently decode gzip: bytes pass through.
		DisableCompression:    true,
		ResponseHeaderTimeout: time.Duration(cfg.ResponseHeaderTimeoutSeconds) * time.Second,
	}
	p.httpClient = &http.Client{Transport: tr}
	p.clientTimout = cfg.ResponseHeaderTimeoutSeconds
	return p.httpClient
}

// checkClientKey validates the request's client key and returns the
// matched client. x-api-key wins when present; no fall-through on an
// invalid chosen key.
func checkClientKey(r *http.Request, cfg *config.Config) (config.Client, bool) {
	key := r.Header.Get("x-api-key")
	if key == "" {
		auth := r.Header.Get("Authorization")
		if rest, ok := strings.CutPrefix(auth, "Bearer "); ok {
			key = rest
		}
	}
	if key == "" {
		return config.Client{}, false
	}
	sum := sha256.Sum256([]byte(key))
	for _, c := range cfg.Clients {
		stored, err := hex.DecodeString(c.KeySHA256)
		if err != nil || len(stored) != len(sum) {
			continue
		}
		if subtle.ConstantTimeCompare(sum[:], stored) == 1 {
			return c, true
		}
	}
	return config.Client{}, false
}

// HashKey hashes a client key for storage.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func writeResponseHead(w http.ResponseWriter, resp *http.Response) {
	h := w.Header()
	for k := range h {
		h.Del(k)
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			h.Add(k, v)
		}
	}
	// Only hop-by-hop headers are stripped; everything else, including
	// Content-Length and Content-Encoding, is forwarded unchanged.
	stripHopByHop(h)
	w.WriteHeader(resp.StatusCode)
}

func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// prefixDeadlineOrDefault returns the test override or the design default.
func (p *Proxy) prefixDeadlineOrDefault() time.Duration {
	if p.prefixDeadline > 0 {
		return p.prefixDeadline
	}
	return errorPrefixDeadline
}

// streamCopy copies with a flush per read; no full-body buffering.
func streamCopy(w http.ResponseWriter, r io.Reader) {
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			flush(w)
		}
		if err != nil {
			return
		}
	}
}

// readPrefix reads up to limit bytes without sending anything to the
// client. When the deadline passes mid-read it awaits the pending read (so
// bytes stay ordered) and reports timedOut, which the caller classifies as
// Transient; the deadline is then effectively cleared because the caller
// proceeds to write and stream.
func readPrefix(ctx context.Context, body io.Reader, limit int, deadline time.Duration) (prefix []byte, timedOut bool, err error) {
	buf := make([]byte, 0, limit)
	chunk := make([]byte, 8192)
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	type res struct {
		n   int
		err error
	}
	for len(buf) < limit {
		max := limit - len(buf)
		if max > len(chunk) {
			max = len(chunk)
		}
		ch := make(chan res, 1)
		go func(n int) {
			nn, rerr := body.Read(chunk[:n])
			ch <- res{nn, rerr}
		}(max)
		select {
		case r := <-ch:
			buf = append(buf, chunk[:r.n]...)
			if r.err != nil {
				return buf, false, nil // EOF or read error: prefix complete
			}
		case <-timer.C:
			select {
			case r := <-ch:
				buf = append(buf, chunk[:r.n]...)
				return buf, true, nil
			case <-ctx.Done():
				return nil, true, ctx.Err()
			}
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}
	return buf, false, nil
}

// drainClose drains up to limit bytes and closes, keeping the upstream
// connection reusable.
func drainClose(body io.ReadCloser, limit int64) {
	_, _ = io.CopyN(io.Discard, body, limit)
	_ = body.Close()
}

// writeAPIError writes a local, Anthropic-shaped error body.
func writeAPIError(w http.ResponseWriter, status int, typ, msg string) {
	body, _ := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    typ,
			"message": msg,
		},
	})
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
