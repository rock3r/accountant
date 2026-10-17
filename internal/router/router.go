// Package router implements the exact routing rules from DESIGN.md:
// priority-order selection, session pins, header-driven quota state,
// cooldowns, and pre-byte failover.
package router

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"accountant/internal/config"
	"accountant/internal/provider"
	"accountant/internal/state"
)

// Clock abstracts time so tests can use a fake clock.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// RealClock returns the wall clock.
func RealClock() Clock { return realClock{} }

const (
	// pinTTL: a pin expires after 24 hours without a request.
	pinTTL = 24 * time.Hour
	// maxCooldown: no cooldown runs longer than 8 days, so a misparsed
	// value or a skewed clock cannot strand an account.
	maxCooldown = 8 * 24 * time.Hour
	// defaultCooldown applies to a 429 with no exhausted window and no
	// retry-after.
	defaultCooldown = 5 * time.Minute
)

// Decision is the outcome of Acquire.
type Decision struct {
	Account     config.Account
	Probe       bool // zero-eligible probe: pass response through, no failover
	NoneEnabled bool // no enabled accounts at all: caller returns local 503
}

// Router picks accounts, pins sessions, and records quota state. One mutex
// guards selection, re-pinning, quota updates, and persistence.
type Router struct {
	mu      sync.Mutex
	cfgPath string
	store   *state.Store
	clock   Clock
	log     *slog.Logger
}

// New creates a Router. Config and state are read fresh (under the shared
// lock) on every operation, so CLI edits take effect without a restart.
func New(cfgPath string, store *state.Store, clock Clock, log *slog.Logger) *Router {
	return &Router{cfgPath: cfgPath, store: store, clock: clock, log: log}
}

func (r *Router) loadConfig() (*config.Config, error) {
	return config.Load(r.cfgPath)
}

// NormalizeUtilization converts a raw utilization value to a fraction using
// the configured scale. With the "fraction" default, values above 1 are
// still divided by 100 as a guard against an upstream scale change.
func NormalizeUtilization(v float64, scale string) float64 {
	switch scale {
	case "percent":
		return v / 100
	default: // "fraction"
		if v > 1 {
			return v / 100
		}
		return v
	}
}

// windowBlocks reports whether a recorded window sample currently makes an
// account ineligible: a real sample, utilization at or above 1.0, and a
// reset time still in the future. A sample whose reset has passed counts as
// unknown; unknown counts as eligible.
func windowBlocks(w state.Window, now time.Time, scale string) bool {
	if !w.HasSample || w.ResetAt.IsZero() || !w.ResetAt.After(now) {
		return false
	}
	return NormalizeUtilization(w.Utilization, scale) >= 1.0
}

// eligible implements the exact rule: not cooling, not auth-broken, and
// last known 5-hour and weekly utilization both below 100%.
func eligible(as *state.AccountState, now time.Time, scale string) bool {
	if as == nil {
		return true // no state recorded yet: eligible
	}
	if as.AuthBroken {
		return false
	}
	if as.CoolingUntil.After(now) {
		return false
	}
	if windowBlocks(as.FiveHour, now, scale) {
		return false
	}
	if windowBlocks(as.SevenDay, now, scale) {
		return false
	}
	return true
}

// recoveryTime is used to pick the zero-eligible probe target:
// cooling_until if set and in the future, else the earliest future reset_at
// among the account's fresh exhausted windows, else now.
func recoveryTime(as *state.AccountState, now time.Time, scale string) time.Time {
	if as == nil {
		return now
	}
	if as.CoolingUntil.After(now) {
		return as.CoolingUntil
	}
	best := time.Time{}
	for _, w := range []state.Window{as.FiveHour, as.SevenDay} {
		if windowBlocks(w, now, scale) && w.ResetAt.After(now) {
			if best.IsZero() || w.ResetAt.Before(best) {
				best = w.ResetAt
			}
		}
	}
	if best.IsZero() {
		return now
	}
	return best
}

func sortAccounts(accounts []config.Account) {
	sort.SliceStable(accounts, func(i, j int) bool {
		if accounts[i].Priority != accounts[j].Priority {
			return accounts[i].Priority < accounts[j].Priority
		}
		return accounts[i].ID < accounts[j].ID
	})
}

// HashSession returns the log-safe form of a session key.
func HashSession(sessionKey string) string {
	sum := sha256.Sum256([]byte(sessionKey))
	return hex.EncodeToString(sum[:4])
}

// Acquire picks the account for a session, re-checking any existing pin.
// It runs selection again (and re-pins, logged) when the pinned account is
// no longer eligible, and falls back to the zero-eligible probe when no
// account is eligible at all.
func (r *Router) Acquire(providerID, sessionKey string) (Decision, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	cfg, err := r.loadConfig()
	if err != nil {
		return Decision{}, err
	}
	now := r.clock.Now()

	enabled := enabledAccounts(cfg, providerID)
	if len(enabled) == 0 {
		return Decision{NoneEnabled: true}, nil
	}

	var dec Decision
	err = r.store.Locked(func(st *state.State) (bool, error) {
		dirty := prunePins(st, now)

		if i := st.FindPin(providerID, sessionKey); i >= 0 {
			pin := st.Pins[i]
			if acct := findIn(enabled, pin.AccountID); acct != nil {
				if eligible(st.Account(pin.AccountID), now, cfg.UtilizationScale) {
					if !pin.LastSeen.Equal(now) {
						st.Pins[i].LastSeen = now
						dirty = true
					}
					dec = Decision{Account: *acct}
					return dirty, nil
				}
				r.log.Info("pinned account no longer eligible, re-selecting",
					"session", HashSession(sessionKey), "account", pin.AccountID)
			}
		}

		// New selection: eligible account with the lowest priority number.
		candidates := make([]config.Account, 0, len(enabled))
		for _, a := range enabled {
			if eligible(st.Account(a.ID), now, cfg.UtilizationScale) {
				candidates = append(candidates, a)
			}
		}
		sortAccounts(candidates)
		if len(candidates) > 0 {
			st.SetPin(providerID, sessionKey, candidates[0].ID, now)
			r.log.Info("session pinned",
				"session", HashSession(sessionKey), "account", candidates[0].ID)
			dec = Decision{Account: candidates[0]}
			return true, nil
		}

		// Zero eligible accounts: probe once. Enabled, non-auth-broken
		// account with the earliest recovery time; ties to lowest priority.
		// If every enabled account is auth-broken, the enabled account with
		// the lowest priority number.
		dec = Decision{Account: probeTarget(enabled, st, now, cfg.UtilizationScale), Probe: true}
		return dirty, nil
	})
	return dec, err
}

// Failover advances a session to the next eligible account after a
// quota-exhausted or auth-broken verdict, before any response bytes reached
// the client. Re-pin is compare-and-swap on the session's current account,
// so two concurrent requests of one session that both fail over converge on
// the same next account. ok is false when no untried eligible account
// remains.
func (r *Router) Failover(providerID, sessionKey, fromAccountID string, tried map[string]bool) (config.Account, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	cfg, err := r.loadConfig()
	if err != nil {
		return config.Account{}, false, err
	}
	now := r.clock.Now()
	enabled := enabledAccounts(cfg, providerID)

	var next config.Account
	ok := false
	err = r.store.Locked(func(st *state.State) (bool, error) {
		dirty := prunePins(st, now)
		// Compare-and-swap: if the pin already moved (a concurrent request
		// of this session failed over first), converge on that account.
		if i := st.FindPin(providerID, sessionKey); i >= 0 && st.Pins[i].AccountID != fromAccountID {
			cur := st.Pins[i].AccountID
			if !tried[cur] {
				if acct := findIn(enabled, cur); acct != nil && eligible(st.Account(cur), now, cfg.UtilizationScale) {
					st.Pins[i].LastSeen = now
					next, ok = *acct, true
					return true, nil
				}
			}
		}
		candidates := make([]config.Account, 0, len(enabled))
		for _, a := range enabled {
			if tried[a.ID] {
				continue
			}
			if eligible(st.Account(a.ID), now, cfg.UtilizationScale) {
				candidates = append(candidates, a)
			}
		}
		sortAccounts(candidates)
		if len(candidates) == 0 {
			return dirty, nil
		}
		st.SetPin(providerID, sessionKey, candidates[0].ID, now)
		r.log.Info("session re-pinned after failover",
			"session", HashSession(sessionKey), "from", fromAccountID, "account", candidates[0].ID)
		next, ok = candidates[0], true
		return true, nil
	})
	if err != nil {
		return config.Account{}, false, err
	}
	return next, ok, nil
}

// Record applies a finished response to account state: quota samples from
// headers, cooldown on quota exhaustion, auth-broken on auth verdicts, and
// cooldown clear on success.
func (r *Router) Record(accountID string, resp *http.Response, snap provider.QuotaSnapshot, hasSnap bool, verdict provider.Verdict) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	cfg, err := r.loadConfig()
	if err != nil {
		return err
	}
	now := r.clock.Now()

	return r.store.Locked(func(st *state.State) (bool, error) {
		as := st.Account(accountID)
		if hasSnap {
			applyWindow(&as.FiveHour, snap.FiveHour, now)
			applyWindow(&as.SevenDay, snap.SevenDay, now)
			if snap.Status != "" {
				as.Status = snap.Status
			}
			as.UpdatedAt = now
		}
		switch verdict {
		case provider.VerdictOK:
			if !as.CoolingUntil.IsZero() {
				as.CoolingUntil = time.Time{}
				r.log.Info("account cooldown cleared by 2xx", "account", accountID)
			}
		case provider.VerdictQuotaExhausted:
			until := cooldownFrom(snap, resp, now, cfg.UtilizationScale)
			as.CoolingUntil = until
			if until.IsZero() {
				r.log.Info("account quota-exhausted but reset already passed; stays eligible", "account", accountID)
			} else {
				r.log.Info("account cooling",
					"account", accountID, "until", until.Format(time.RFC3339))
			}
		case provider.VerdictAuthBroken:
			if !as.AuthBroken {
				as.AuthBroken = true
				r.log.Warn("account marked auth-broken", "account", accountID)
			}
		case provider.VerdictTransient:
			// Pass through: no cooldown, no failover, samples still recorded.
		}
		return true, nil
	})
}

// PinSession force-pins a session to an account. Used after a successful
// zero-eligible probe ("a 2xx clears cooldown and pins the session").
func (r *Router) PinSession(providerID, sessionKey, accountID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock.Now()
	return r.store.Locked(func(st *state.State) (bool, error) {
		st.SetPin(providerID, sessionKey, accountID, now)
		r.log.Info("session pinned after successful probe",
			"session", HashSession(sessionKey), "account", accountID)
		return true, nil
	})
}

// ClearAuthBroken clears the auth-broken mark; called by account test when
// validation passes again.
func (r *Router) ClearAuthBroken(accountID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.store.Locked(func(st *state.State) (bool, error) {
		as := st.Account(accountID)
		if !as.AuthBroken {
			return false, nil
		}
		as.AuthBroken = false
		return true, nil
	})
}

// applyWindow overlays a parsed sample onto stored window state.
func applyWindow(dst *state.Window, src provider.WindowSample, now time.Time) {
	if src.HasUtilization {
		dst.Utilization = src.Utilization
	}
	if src.HasReset {
		dst.ResetAt = src.ResetAt
	}
	if src.Status != "" {
		dst.Status = src.Status
	}
	if src.HasUtilization || src.HasReset || src.Status != "" {
		dst.SampledAt = now
		dst.HasSample = true
	}
}

// cooldownFrom implements the exact chain: cool until the *-reset time of
// the window that reads exhausted — the later of the two when both do; if
// no window reads exhausted, until now plus retry-after (delta-seconds or
// HTTP-date), else 5 minutes. Clamped: a reset in the past means
// immediately eligible, and no cooldown runs longer than 8 days.
func cooldownFrom(snap provider.QuotaSnapshot, resp *http.Response, now time.Time, scale string) time.Time {
	var until time.Time
	latest := time.Time{}
	for _, w := range []provider.WindowSample{snap.FiveHour, snap.SevenDay} {
		if w.HasUtilization && NormalizeUtilization(w.Utilization, scale) >= 1.0 && w.HasReset {
			if w.ResetAt.After(latest) {
				latest = w.ResetAt
			}
		}
	}
	switch {
	case !latest.IsZero():
		until = latest
	default:
		if d, ok := ParseRetryAfter(resp.Header.Get("retry-after"), now); ok {
			until = now.Add(d)
		} else {
			until = now.Add(defaultCooldown)
		}
	}
	if !until.After(now) {
		return time.Time{}
	}
	if max := now.Add(maxCooldown); until.After(max) {
		until = max
	}
	return until
}

// ParseRetryAfter parses delta-seconds or an HTTP-date. ok is false when
// the header is missing, unparseable, or not in the future.
func ParseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		d := time.Duration(secs * float64(time.Second))
		return d, d > 0
	}
	if t, err := http.ParseTime(v); err == nil {
		d := t.Sub(now)
		return d, d > 0
	}
	return 0, false
}

func enabledAccounts(cfg *config.Config, providerID string) []config.Account {
	var out []config.Account
	for _, a := range cfg.Accounts {
		if a.Provider == providerID && a.IsEnabled() {
			out = append(out, a)
		}
	}
	return out
}

func findIn(accounts []config.Account, id string) *config.Account {
	for i := range accounts {
		if accounts[i].ID == id {
			return &accounts[i]
		}
	}
	return nil
}

func prunePins(st *state.State, now time.Time) (dirty bool) {
	kept := st.Pins[:0]
	for _, p := range st.Pins {
		if now.Sub(p.LastSeen) < pinTTL {
			kept = append(kept, p)
		} else {
			dirty = true
		}
	}
	st.Pins = kept
	return dirty
}

// probeTarget implements the exact zero-eligible probe target rule.
func probeTarget(enabled []config.Account, st *state.State, now time.Time, scale string) config.Account {
	type cand struct {
		a config.Account
		t time.Time
	}
	var cands []cand
	for _, a := range enabled {
		if st.Account(a.ID).AuthBroken {
			continue
		}
		cands = append(cands, cand{a, recoveryTime(st.Account(a.ID), now, scale)})
	}
	if len(cands) == 0 {
		sorted := append([]config.Account(nil), enabled...)
		sortAccounts(sorted)
		return sorted[0]
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if !cands[i].t.Equal(cands[j].t) {
			return cands[i].t.Before(cands[j].t)
		}
		if cands[i].a.Priority != cands[j].a.Priority {
			return cands[i].a.Priority < cands[j].a.Priority
		}
		return cands[i].a.ID < cands[j].a.ID
	})
	return cands[0].a
}
