package proxy

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"accountant/internal/config"
	"accountant/internal/state"
)

// --- Failover -----------------------------------------------------------

func TestQuota429FailoverBeforeBytes(t *testing.T) {
	r := newRig(t, nil)
	reset5h := r.clock.Now().Add(2 * time.Hour)
	r.up.set("tok-a1", statusWith(429, `{"type":"error","error":{"type":"rate_limit_error","message":"limit"}}`,
		unifiedHeaders("1.0", "0.0", strconv.FormatInt(reset5h.Unix(), 10), "1792584000")))
	r.up.set("tok-a2", ok200("served-by-a2", nil))
	rigAddAccount(t, r, "a1", 10)
	rigAddAccount(t, r, "a2", 20)

	reqBody := []byte(`{"model":"claude-haiku-5-5","stream":true}`)
	code, body, _ := r.post(t, "/v1/messages", "s1", reqBody)
	if code != 200 || string(body) != "served-by-a2" {
		t.Fatalf("status %d body %q", code, body)
	}
	// The same request bytes were replayed on a2.
	a2reqs := r.up.requestsFor("tok-a2")
	if len(a2reqs) != 1 || hash(a2reqs[0].Body) != hash(reqBody) {
		t.Fatalf("a2 replay: %d requests", len(a2reqs))
	}
	// State: a1 cooling until the exhausted window's reset; session re-pinned.
	st := r.state(t)
	if got := st.Accounts["a1"].CoolingUntil; !got.Equal(reset5h) {
		t.Fatalf("a1 cooling until %v, want %v", got, reset5h)
	}
	if i := st.FindPin("anthropic", "s1"); i < 0 || st.Pins[i].AccountID != "a2" {
		t.Fatalf("pin: %+v", st.Pins)
	}
}

func TestBare429CoolsFiveMinutes(t *testing.T) {
	r := newRig(t, nil)
	r.up.set("tok-a1", statusWith(429, `{"type":"error","error":{"type":"rate_limit_error","message":"Error"}}`, nil))
	r.up.set("tok-a2", ok200("ok", nil))
	rigAddAccount(t, r, "a1", 10)
	rigAddAccount(t, r, "a2", 20)

	code, _, _ := r.post(t, "/v1/messages", "s1", []byte(`{"m":1}`))
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	st := r.state(t)
	want := r.clock.Now().Add(5 * time.Minute)
	if got := st.Accounts["a1"].CoolingUntil; !got.Equal(want) {
		t.Fatalf("a1 cooling until %v, want %v", got, want)
	}
}

func Test429WithRetryAfterCoolsByRetryAfter(t *testing.T) {
	r := newRig(t, nil)
	// Unified headers present, but no window reads exhausted.
	hdr := unifiedHeaders("0.5", "0.6", "1792260600", "1792584000")
	hdr["retry-after"] = "900"
	r.up.set("tok-a1", statusWith(429, `{"type":"error","error":{"type":"rate_limit_error","message":"x"}}`, hdr))
	r.up.set("tok-a2", ok200("ok", nil))
	rigAddAccount(t, r, "a1", 10)
	rigAddAccount(t, r, "a2", 20)

	code, _, _ := r.post(t, "/v1/messages", "s1", []byte(`{"m":1}`))
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	st := r.state(t)
	want := r.clock.Now().Add(900 * time.Second)
	if got := st.Accounts["a1"].CoolingUntil; !got.Equal(want) {
		t.Fatalf("a1 cooling until %v, want %v (retry-after, not window reset)", got, want)
	}
}

// A 429 whose body stalls past the prefix deadline is Transient: the
// response completes as-is, with no failover and no cooldown. (Design:
// "429 after bytes does not replay" — the deadline passing is the only way
// bytes start flowing before a verdict in this implementation.)
func Test429SlowBodyIsTransient(t *testing.T) {
	r := newRig(t, nil)
	r.proxy.prefixDeadline = 100 * time.Millisecond
	r.up.set("tok-a1", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(429)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(400 * time.Millisecond)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow"}}`)
	})
	r.up.set("tok-a2", ok200("must-not-be-used", nil))
	rigAddAccount(t, r, "a1", 10)
	rigAddAccount(t, r, "a2", 20)

	code, body, _ := r.post(t, "/v1/messages", "s1", []byte(`{"m":1}`))
	if code != 429 || !strings.Contains(string(body), "slow") {
		t.Fatalf("status %d body %q", code, body)
	}
	if len(r.up.requestsFor("tok-a2")) != 0 {
		t.Fatal("transient must never advance")
	}
	st := r.state(t)
	if !st.Accounts["a1"].CoolingUntil.IsZero() {
		t.Fatal("transient must not cool")
	}
}

func TestInStreamSSEErrorPassesThrough(t *testing.T) {
	r := newRig(t, nil)
	sse := "event: message_start\r\ndata: {\"type\":\"message_start\"}\r\n\r\n" +
		"event: error\r\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\r\n\r\n"
	r.up.set("tok-a1", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	})
	r.up.set("tok-a2", ok200("must-not-be-used", nil))
	rigAddAccount(t, r, "a1", 10)
	rigAddAccount(t, r, "a2", 20)

	code, body, _ := r.post(t, "/v1/messages", "s1", []byte(`{"m":1}`))
	if code != 200 || string(body) != sse {
		t.Fatalf("status %d body %q", code, body)
	}
	if len(r.up.requestsFor("tok-a2")) != 0 {
		t.Fatal("mid-stream error must not fail over")
	}
	st := r.state(t)
	if !st.Accounts["a1"].CoolingUntil.IsZero() || st.Accounts["a1"].AuthBroken {
		t.Fatal("mid-stream error must not classify")
	}
	if i := st.FindPin("anthropic", "s1"); i < 0 || st.Pins[i].AccountID != "a1" {
		t.Fatalf("pin must not move: %+v", st.Pins)
	}
}

func Test401AuthBrokenFailover(t *testing.T) {
	r := newRig(t, nil)
	r.up.set("tok-a1", statusWith(401, `{"type":"error","error":{"type":"authentication_error","message":"invalid"}}`, nil))
	r.up.set("tok-a2", ok200("served-by-a2", nil))
	rigAddAccount(t, r, "a1", 10)
	rigAddAccount(t, r, "a2", 20)

	code, body, _ := r.post(t, "/v1/messages", "s1", []byte(`{"m":1}`))
	if code != 200 || string(body) != "served-by-a2" {
		t.Fatalf("status %d body %q", code, body)
	}
	st := r.state(t)
	if !st.Accounts["a1"].AuthBroken {
		t.Fatal("a1 must be auth-broken")
	}
	// A new session must skip the auth-broken account entirely.
	r.up.reset()
	code, _, _ = r.post(t, "/v1/messages", "s2", []byte(`{"m":1}`))
	if code != 200 || len(r.up.requestsFor("tok-a1")) != 0 {
		t.Fatalf("s2 must not touch a1: %d a1-reqs", len(r.up.requestsFor("tok-a1")))
	}
}

func Test403AnthropicAuthPrefixFailsOver(t *testing.T) {
	r := newRig(t, nil)
	r.up.set("tok-a1", statusWith(403, `{"type":"error","error":{"type":"permission_error","message":"scope"}}`, nil))
	r.up.set("tok-a2", ok200("served-by-a2", nil))
	rigAddAccount(t, r, "a1", 10)
	rigAddAccount(t, r, "a2", 20)

	code, body, _ := r.post(t, "/v1/messages", "s1", []byte(`{"m":1}`))
	if code != 200 || string(body) != "served-by-a2" {
		t.Fatalf("status %d body %q", code, body)
	}
	if !r.state(t).Accounts["a1"].AuthBroken {
		t.Fatal("a1 must be auth-broken")
	}
}

func Test403EdgeBlockPassesThrough(t *testing.T) {
	r := newRig(t, nil)
	edge := `<html><head><title>Attention Required</title></head><body>Cloudflare</body></html>`
	r.up.set("tok-a1", statusWith(403, edge, map[string]string{"content-type": "text/html"}))
	r.up.set("tok-a2", ok200("must-not-be-used", nil))
	rigAddAccount(t, r, "a1", 10)
	rigAddAccount(t, r, "a2", 20)

	code, body, _ := r.post(t, "/v1/messages", "s1", []byte(`{"m":1}`))
	if code != 403 || string(body) != edge {
		t.Fatalf("status %d body %q", code, body)
	}
	if len(r.up.requestsFor("tok-a2")) != 0 {
		t.Fatal("edge block must not fail over")
	}
	st := r.state(t)
	if st.Accounts["a1"].AuthBroken || !st.Accounts["a1"].CoolingUntil.IsZero() {
		t.Fatal("edge block must not classify")
	}
}

func TestUpstreamNeverSendsHeaders504(t *testing.T) {
	r := newRig(t, nil)
	r.up.set("tok-a1", func(w http.ResponseWriter, req *http.Request) {
		select {
		case <-req.Context().Done():
		case <-time.After(30 * time.Second):
		}
	})
	r.up.set("tok-a2", ok200("must-not-be-used", nil))
	rigAddAccount(t, r, "a1", 10)
	rigAddAccount(t, r, "a2", 20)
	// Tighten the response-header timeout for the test.
	cfg, _ := loadConfigForTest(r)
	cfg.ResponseHeaderTimeoutSeconds = 1
	saveConfigForTest(t, r, cfg)

	code, body, _ := r.post(t, "/v1/messages", "s1", []byte(`{"m":1}`))
	if code != http.StatusGatewayTimeout {
		t.Fatalf("status %d", code)
	}
	if !strings.Contains(string(body), "api_error") {
		t.Fatalf("body %s", body)
	}
	if len(r.up.requestsFor("tok-a2")) != 0 {
		t.Fatal("header timeout must never advance")
	}
	st := r.state(t)
	if !st.Accounts["a1"].CoolingUntil.IsZero() || st.Accounts["a1"].AuthBroken {
		t.Fatal("header timeout must not classify")
	}
}

func Test500PassesThroughBodyIntact(t *testing.T) {
	r := newRig(t, nil)
	// Larger than the 64 KiB prefix: exercises prefix + remainder streaming.
	big := `{"type":"error","error":{"type":"api_error","message":"` + strings.Repeat("x", 100*1024) + `"}}`
	r.up.set("tok-a1", statusWith(500, big, nil))
	r.up.set("tok-a2", ok200("must-not-be-used", nil))
	rigAddAccount(t, r, "a1", 10)
	rigAddAccount(t, r, "a2", 20)

	code, body, _ := r.post(t, "/v1/messages", "s1", []byte(`{"m":1}`))
	if code != 500 {
		t.Fatalf("status %d", code)
	}
	if hash(body) != hash([]byte(big)) {
		t.Fatal("500 body not hash-identical")
	}
	if len(r.up.requestsFor("tok-a2")) != 0 {
		t.Fatal("500 must not fail over")
	}
	if !r.state(t).Accounts["a1"].CoolingUntil.IsZero() {
		t.Fatal("500 must not cool")
	}
}

func TestMidRequestExhaustionReturnsLastFailure(t *testing.T) {
	r := newRig(t, nil)
	mk := func(tag string) func(http.ResponseWriter, *http.Request) {
		return statusWith(429, `{"type":"error","error":{"type":"rate_limit_error","message":"out-`+tag+`"}}`, nil)
	}
	r.up.set("tok-a1", mk("a1"))
	r.up.set("tok-a2", mk("a2"))
	r.up.set("tok-a3", mk("a3"))
	rigAddAccount(t, r, "a1", 10)
	rigAddAccount(t, r, "a2", 20)
	rigAddAccount(t, r, "a3", 30)

	code, body, _ := r.post(t, "/v1/messages", "s1", []byte(`{"m":1}`))
	if code != 429 || !strings.Contains(string(body), "out-a3") {
		t.Fatalf("status %d body %q: last failure must pass through", code, body)
	}
	// Each account tried at most once; no probe after in-request failover.
	if len(r.up.requestsFor("tok-a1")) != 1 || len(r.up.requestsFor("tok-a2")) != 1 || len(r.up.requestsFor("tok-a3")) != 1 {
		t.Fatalf("each account must be tried exactly once: %v",
			[]int{len(r.up.requestsFor("tok-a1")), len(r.up.requestsFor("tok-a2")), len(r.up.requestsFor("tok-a3"))})
	}
}

func TestZeroEligibleProbe429PassesThrough(t *testing.T) {
	r := newRig(t, nil)
	r.up.set("tok-a1", statusWith(429, `{"err":"a1-probe"}`, nil))
	r.up.set("tok-a2", statusWith(429, `{"err":"a2-probe"}`, nil))
	rigAddAccount(t, r, "a1", 10)
	rigAddAccount(t, r, "a2", 20)
	// Pre-cool both; a2 recovers sooner.
	rigCool(t, r, "a1", r.clock.Now().Add(time.Hour))
	rigCool(t, r, "a2", r.clock.Now().Add(10*time.Minute))

	code, body, _ := r.post(t, "/v1/messages", "s1", []byte(`{"m":1}`))
	if code != 429 || !strings.Contains(string(body), "a2-probe") {
		t.Fatalf("probe must hit a2 and pass through: %d %s", code, body)
	}
	if len(r.up.requestsFor("tok-a1")) != 0 {
		t.Fatal("probe must contact exactly one account")
	}
	if len(r.up.requestsFor("tok-a2")) != 1 {
		t.Fatal("exactly one probe")
	}
}

func TestZeroEligibleProbe2xxClearsCooldownAndPins(t *testing.T) {
	r := newRig(t, nil)
	r.up.set("tok-a1", ok200("a1", nil))
	r.up.set("tok-a2", ok200("recovered-a2", nil))
	rigAddAccount(t, r, "a1", 10)
	rigAddAccount(t, r, "a2", 20)
	rigCool(t, r, "a1", r.clock.Now().Add(time.Hour))
	rigCool(t, r, "a2", r.clock.Now().Add(10*time.Minute))

	code, body, _ := r.post(t, "/v1/messages", "s1", []byte(`{"m":1}`))
	if code != 200 || string(body) != "recovered-a2" {
		t.Fatalf("probe: %d %s", code, body)
	}
	st := r.state(t)
	if !st.Accounts["a2"].CoolingUntil.IsZero() {
		t.Fatal("2xx probe must clear cooldown")
	}
	if i := st.FindPin("anthropic", "s1"); i < 0 || st.Pins[i].AccountID != "a2" {
		t.Fatalf("2xx probe must pin the session: %+v", st.Pins)
	}
	// Next request on the session uses the pin without probing.
	r.up.reset()
	code, body, _ = r.post(t, "/v1/messages", "s1", []byte(`{"m":1}`))
	if code != 200 || string(body) != "recovered-a2" || len(r.up.requests()) != 1 {
		t.Fatalf("pinned request: %d %s", code, body)
	}
}

func TestZeroEligibleProbe401NoFailover(t *testing.T) {
	r := newRig(t, nil)
	r.up.set("tok-a1", ok200("must-not-be-used", nil))
	r.up.set("tok-a2", statusWith(401, `{"type":"error","error":{"type":"authentication_error","message":"bad"}}`, nil))
	rigAddAccount(t, r, "a1", 10)
	rigAddAccount(t, r, "a2", 20)
	rigCool(t, r, "a1", r.clock.Now().Add(time.Hour))
	rigCool(t, r, "a2", r.clock.Now().Add(10*time.Minute))

	code, body, _ := r.post(t, "/v1/messages", "s1", []byte(`{"m":1}`))
	if code != 401 || !strings.Contains(string(body), "bad") {
		t.Fatalf("probe 401 must pass through unchanged: %d %s", code, body)
	}
	if len(r.up.requestsFor("tok-a1")) != 0 {
		t.Fatal("no further failover after a probe")
	}
	if !r.state(t).Accounts["a2"].AuthBroken {
		t.Fatal("401 probe must mark auth-broken")
	}
}

func TestConcurrentFailoverConverges(t *testing.T) {
	r := newRig(t, nil)
	r.up.set("tok-a1", statusWith(429, `{"err":"out"}`, nil))
	r.up.set("tok-a2", ok200("ok-a2", nil))
	rigAddAccount(t, r, "a1", 10)
	rigAddAccount(t, r, "a2", 20)

	r.post(t, "/v1/messages", "s1", []byte(`{"m":1}`)) // pins a1, fails over to a2
	// Clear a1's cooldown and re-pin to set up the race: two concurrent
	// requests of one session, both starting eligible on a1, both hitting
	// a fresh 429 there.
	if err := r.store.Locked(func(st *state.State) (bool, error) {
		st.Accounts["a1"] = &state.AccountState{}
		st.SetPin("anthropic", "s1", "a1", r.clock.Now())
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, _, _ := r.post(t, "/v1/messages", "s1", []byte(`{"m":1}`))
			codes[i] = code
		}(i)
	}
	wg.Wait()
	if codes[0] != 200 || codes[1] != 200 {
		t.Fatalf("codes %v", codes)
	}
	st := r.state(t)
	if i := st.FindPin("anthropic", "s1"); i < 0 || st.Pins[i].AccountID != "a2" {
		t.Fatalf("concurrent failover must converge on a2: %+v", st.Pins)
	}
	// a1 must have been tried at least once per request that raced through
	// before the cooldown landed: 1 (prime) + 1 or 2 (concurrent).
	if n := len(r.up.requestsFor("tok-a1")); n < 2 || n > 3 {
		t.Fatalf("a1 requests: %d", n)
	}
}

func loadConfigForTest(r *rig) (*config.Config, error) {
	return config.Load(r.cfgPath)
}

func saveConfigForTest(t *testing.T, r *rig, cfg *config.Config) {
	t.Helper()
	if err := config.Save(r.cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
}
