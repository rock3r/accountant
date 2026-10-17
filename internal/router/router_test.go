package router

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"accountant/internal/config"
	"accountant/internal/state"
)

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

func testConfig(t *testing.T, accounts ...config.Account) string {
	t.Helper()
	dir := t.TempDir()
	cfgPath := dir + "/config.toml"
	cfg := &config.Config{Accounts: accounts}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

func acct(id string, priority int) config.Account {
	en := true
	return config.Account{ID: id, Provider: "anthropic", Label: id, Priority: priority, Enabled: &en, CredentialRef: id}
}

func testRouter(t *testing.T, cfgPath string, clock Clock) (*Router, *state.Store) {
	t.Helper()
	dir := t.TempDir()
	store := state.NewStore(dir + "/state.json")
	return New(cfgPath, store, clock, testLogger()), store
}

func mustAcquire(t *testing.T, r *Router, session string) Decision {
	t.Helper()
	dec, err := r.Acquire("anthropic", session)
	if err != nil {
		t.Fatal(err)
	}
	return dec
}

func TestPriorityOrderOnNewSessions(t *testing.T) {
	now := time.Now()
	cfgPath := testConfig(t, acct("a2", 20), acct("a1", 10))
	r, _ := testRouter(t, cfgPath, newFakeClock(now))

	if d := mustAcquire(t, r, "s1"); d.Account.ID != "a1" || d.Probe {
		t.Fatalf("s1: got %+v, want a1 non-probe", d)
	}
	// Policy: two brand-new sessions both start on account 1.
	if d := mustAcquire(t, r, "s2"); d.Account.ID != "a1" || d.Probe {
		t.Fatalf("s2: got %+v, want a1 non-probe", d)
	}
}

func TestPinStickiness(t *testing.T) {
	now := time.Now()
	cfgPath := testConfig(t, acct("a1", 10), acct("a2", 20))
	r, _ := testRouter(t, cfgPath, newFakeClock(now))

	mustAcquire(t, r, "s1")
	// While a1 stays eligible, the pin sticks across requests.
	if d := mustAcquire(t, r, "s1"); d.Account.ID != "a1" {
		t.Fatalf("s1 should stay pinned to a1, got %s", d.Account.ID)
	}
	// Once a1 cools, the next request re-selects before any upstream call.
	coolAccount(t, r, "a1", now.Add(time.Hour))
	if d := mustAcquire(t, r, "s1"); d.Account.ID != "a2" {
		t.Fatalf("s1 should re-select a2 while a1 cools, got %s", d.Account.ID)
	}
}

func TestParallelSessionsDifferWhenFirstCooling(t *testing.T) {
	now := time.Now()
	cfgPath := testConfig(t, acct("a1", 10), acct("a2", 20))
	r, _ := testRouter(t, cfgPath, newFakeClock(now))

	if d := mustAcquire(t, r, "s1"); d.Account.ID != "a1" {
		t.Fatalf("s1: got %s", d.Account.ID)
	}
	coolAccount(t, r, "a1", now.Add(time.Hour))
	if d := mustAcquire(t, r, "s2"); d.Account.ID != "a2" {
		t.Fatalf("s2: got %s, want a2", d.Account.ID)
	}
}

func TestNoPinMigrationOnRecovery(t *testing.T) {
	now := time.Now()
	cfgPath := testConfig(t, acct("a1", 10), acct("a2", 20))
	clock := newFakeClock(now)
	r, _ := testRouter(t, cfgPath, clock)

	mustAcquire(t, r, "s1")
	coolAccount(t, r, "a1", now.Add(time.Hour))
	if d := mustAcquire(t, r, "s1"); d.Account.ID != "a2" {
		t.Fatalf("s1: got %s, want a2", d.Account.ID)
	}
	clock.Advance(2 * time.Hour) // a1 recovers
	if d := mustAcquire(t, r, "s1"); d.Account.ID != "a2" {
		t.Fatalf("s1 must not migrate back: got %s", d.Account.ID)
	}
	// But a new session takes the recovered priority account.
	if d := mustAcquire(t, r, "s3"); d.Account.ID != "a1" {
		t.Fatalf("s3: got %s, want a1", d.Account.ID)
	}
}

func TestPinExpiryAfter24h(t *testing.T) {
	now := time.Now()
	cfgPath := testConfig(t, acct("a1", 10), acct("a2", 20))
	clock := newFakeClock(now)
	r, store := testRouter(t, cfgPath, clock)

	mustAcquire(t, r, "s1")
	// Disable a1; after 24h without a request the pin expires and s1 re-selects.
	clock.Advance(25 * time.Hour)
	if d := mustAcquire(t, r, "s1"); d.Account.ID != "a1" {
		t.Fatalf("pin alive but idle 25h must expire and re-select a1: got %s", d.Account.ID)
	}
	st, err := state.Load(store.File())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Pins) != 1 {
		t.Fatalf("expired pins must be pruned, got %d", len(st.Pins))
	}
}

func TestFailoverConvergesConcurrentSessions(t *testing.T) {
	now := time.Now()
	cfgPath := testConfig(t, acct("a1", 10), acct("a2", 20), acct("a3", 30))
	r, _ := testRouter(t, cfgPath, newFakeClock(now))
	mustAcquire(t, r, "s1")

	var wg sync.WaitGroup
	results := make([]string, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			next, ok, err := r.Failover("anthropic", "s1", "a1", map[string]bool{"a1": true})
			if err != nil || !ok {
				t.Errorf("failover %d: ok=%v err=%v", i, ok, err)
				return
			}
			results[i] = next.ID
		}(i)
	}
	wg.Wait()
	if results[0] != results[1] || results[0] != "a2" {
		t.Fatalf("concurrent failovers must converge on a2, got %v", results)
	}
}

func TestCooldownChain(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	cfgPath := testConfig(t, acct("a1", 10))
	r, store := testRouter(t, cfgPath, newFakeClock(now))

	// Bare 429, no unified headers, no retry-after: 5 minutes.
	resp := &http.Response{StatusCode: 429, Header: http.Header{}}
	if err := r.Record("a1", resp, snapNone(), false, verdictQuota()); err != nil {
		t.Fatal(err)
	}
	assertCooling(t, store, "a1", now.Add(5*time.Minute))

	// 429 with retry-after but no exhausted window: cool by retry-after.
	r2 := &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"900"}}}
	if err := r.Record("a1", r2, snapNone(), false, verdictQuota()); err != nil {
		t.Fatal(err)
	}
	assertCooling(t, store, "a1", now.Add(15*time.Minute))
}

func TestCooldownFromExhaustedWindow(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	cfgPath := testConfig(t, acct("a1", 10))
	r, store := testRouter(t, cfgPath, newFakeClock(now))

	reset5h := now.Add(2 * time.Hour)
	reset7d := now.Add(4 * 24 * time.Hour)
	snap := snapWindows(
		window(0.5, reset5h), // not exhausted
		window(1.0, reset7d), // exhausted
	)
	resp := &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"60"}}}
	if err := r.Record("a1", resp, snap, true, verdictQuota()); err != nil {
		t.Fatal(err)
	}
	// The exhausted window's reset wins over retry-after.
	assertCooling(t, store, "a1", reset7d)
}

func TestCooldownClampAndPastReset(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	cfgPath := testConfig(t, acct("a1", 10))
	r, store := testRouter(t, cfgPath, newFakeClock(now))

	// Reset beyond the 8-day clamp.
	far := now.Add(30 * 24 * time.Hour)
	snap := snapWindows(window(1.0, far), window(0, time.Time{}))
	resp := &http.Response{StatusCode: 429, Header: http.Header{}}
	if err := r.Record("a1", resp, snap, true, verdictQuota()); err != nil {
		t.Fatal(err)
	}
	assertCooling(t, store, "a1", now.Add(8*24*time.Hour))

	// Reset in the past: immediately eligible.
	past := now.Add(-time.Minute)
	snap2 := snapWindows(window(1.0, past), window(0, time.Time{}))
	if err := r.Record("a1", resp, snap2, true, verdictQuota()); err != nil {
		t.Fatal(err)
	}
	st, err := state.Load(store.File())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Accounts["a1"].CoolingUntil.IsZero() {
		t.Fatalf("past reset must not cool, got %v", st.Accounts["a1"].CoolingUntil)
	}
}

func TestStaleSampleStopsBlocking(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	cfgPath := testConfig(t, acct("a1", 10), acct("a2", 20))
	clock := newFakeClock(now)
	r, _ := testRouter(t, cfgPath, clock)

	// a1 records a fresh 100% 5h sample; new sessions go to a2.
	reset := now.Add(time.Hour)
	resp := &http.Response{StatusCode: 200, Header: http.Header{}}
	snap := snapWindows(window(1.0, reset), window(0, time.Time{}))
	if err := r.Record("a1", resp, snap, true, verdictOK()); err != nil {
		t.Fatal(err)
	}
	if d := mustAcquire(t, r, "s1"); d.Account.ID != "a2" {
		t.Fatalf("with a1 at 100%%, s1 should take a2, got %s", d.Account.ID)
	}
	// Once the clock passes the reset, the stale sample no longer blocks:
	// a new session takes the priority account again, no probe.
	clock.Advance(2 * time.Hour)
	if d := mustAcquire(t, r, "s2"); d.Account.ID != "a1" || d.Probe {
		t.Fatalf("after reset, s2 should take a1 non-probe, got %+v", d)
	}
}

func TestZeroEligibleProbeTarget(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	cfgPath := testConfig(t, acct("a1", 10), acct("a2", 20), acct("a3", 30))
	r, store := testRouter(t, cfgPath, newFakeClock(now))

	// a1 cools for an hour, a2 for ten minutes: probe goes to a2 (earliest recovery).
	coolAccount(t, r, "a1", now.Add(time.Hour))
	coolAccount(t, r, "a2", now.Add(10*time.Minute))
	coolAccount(t, r, "a3", now.Add(30*time.Minute))
	d := mustAcquire(t, r, "s1")
	if !d.Probe || d.Account.ID != "a2" {
		t.Fatalf("probe should target a2, got %+v", d)
	}

	// With a2 auth-broken, probe skips it: a3 recovers next... a3 cools 30m,
	// a1 cools 60m, so a3.
	markAuthBroken(t, r, "a2")
	d = mustAcquire(t, r, "s2")
	if !d.Probe || d.Account.ID != "a3" {
		t.Fatalf("probe should target a3, got %+v", d)
	}

	// All auth-broken: lowest priority number wins.
	markAuthBroken(t, r, "a1")
	markAuthBroken(t, r, "a3")
	d = mustAcquire(t, r, "s3")
	if !d.Probe || d.Account.ID != "a1" {
		t.Fatalf("all-auth-broken probe should target a1 (lowest priority), got %+v", d)
	}
	_ = store
}

func TestNoEnabledAccounts(t *testing.T) {
	now := time.Now()
	off := false
	disabled := acct("a1", 10)
	disabled.Enabled = &off
	cfgPath := testConfig(t, disabled)
	r, _ := testRouter(t, cfgPath, newFakeClock(now))
	if d := mustAcquire(t, r, "s1"); !d.NoneEnabled {
		t.Fatalf("want NoneEnabled, got %+v", d)
	}
}

func TestPinSurvivesRestart(t *testing.T) {
	now := time.Now()
	cfgPath := testConfig(t, acct("a1", 10), acct("a2", 20))
	clock := newFakeClock(now)
	dir := t.TempDir()
	store := state.NewStore(dir + "/state.json")
	r1 := New(cfgPath, store, clock, testLogger())
	mustAcquire(t, r1, "s1")
	coolAccount(t, r1, "a1", now.Add(time.Hour))
	if d := mustAcquire(t, r1, "s1"); d.Account.ID != "a2" {
		t.Fatalf("s1: got %s", d.Account.ID)
	}
	// "Restart": new router over the same state file.
	r2 := New(cfgPath, store, clock, testLogger())
	if d, err := r2.Acquire("anthropic", "s1"); err != nil || d.Account.ID != "a2" {
		t.Fatalf("after restart s1 should stay on a2, got %+v err=%v", d, err)
	}
}

// helpers

func coolAccount(t *testing.T, r *Router, id string, until time.Time) {
	t.Helper()
	now := r.clock.Now()
	resp := &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{itoa(int(until.Sub(now).Seconds()))}}}
	if err := r.Record(id, resp, snapNone(), false, verdictQuota()); err != nil {
		t.Fatal(err)
	}
}

func markAuthBroken(t *testing.T, r *Router, id string) {
	t.Helper()
	resp := &http.Response{StatusCode: 401, Header: http.Header{}}
	if err := r.Record(id, resp, snapNone(), false, verdictAuth()); err != nil {
		t.Fatal(err)
	}
}

func assertCooling(t *testing.T, store *state.Store, id string, want time.Time) {
	t.Helper()
	st, err := state.Load(store.File())
	if err != nil {
		t.Fatal(err)
	}
	got := st.Accounts[id].CoolingUntil
	if !got.Equal(want) {
		t.Fatalf("cooling until %v, want %v", got, want)
	}
}
