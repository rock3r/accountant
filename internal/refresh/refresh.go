// Package refresh serializes credential refreshes across processes.
// Codex refresh tokens rotate on every exchange, so the daemon and CLI
// commands must never exchange the same refresh token twice: the loser
// gets refresh_token_reused and would mark a healthy account auth-broken.
package refresh

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"accountant/internal/lockfile"
	"accountant/internal/provider"
	"accountant/internal/vault"
)

// LockPath returns the per-credential lock file, next to the state file.
func LockPath(stateFile, ref string) string {
	return filepath.Join(filepath.Dir(stateFile), "refresh-"+ref+".lock")
}

// IfNeeded runs the read-check-refresh-write cycle for ref under the
// shared per-credential lock. The vault is re-read inside the lock: a
// credential another process (or worker) just refreshed comes back as-is
// and no second exchange happens. refreshed reports whether the vault now
// holds a new credential.
//
// Providers without CredentialExpirer/Refresher (Claude setup-tokens)
// short-circuit to a plain vault read.
func IfNeeded(ctx context.Context, v vault.Vault, prov provider.Provider, stateFile, ref string, margin time.Duration) (cred []byte, refreshed bool, err error) {
	exp, isExp := prov.(provider.CredentialExpirer)
	refresher, isRef := prov.(provider.Refresher)
	if !isExp || !isRef {
		cred, err := v.Get(ref)
		return cred, false, err
	}
	err = lockfile.Locked(LockPath(stateFile, ref), func() error {
		cred, err = v.Get(ref)
		if err != nil {
			return err
		}
		expiry, ok := exp.CredentialExpiry(cred)
		if !ok || expiry.After(time.Now().Add(margin)) {
			return nil // fresh enough, or no readable expiry
		}
		newCred, err := refresher.Refresh(ctx, cred)
		if err != nil {
			return err
		}
		// The refresh token has now rotated upstream: newCred is the only
		// valid copy, so persistence must not fail silently. Retry a
		// transient vault error before giving up.
		var perr error
		for attempt := 0; attempt < 3; attempt++ {
			if perr = v.Put(ref, newCred); perr == nil {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if perr != nil {
			return fmt.Errorf("rotated credential could not be saved (%v); if this account turns auth-broken, re-import it from a fresh `codex login`", perr)
		}
		cred = newCred
		refreshed = true
		return nil
	})
	return cred, refreshed, err
}
