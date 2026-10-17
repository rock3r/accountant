package cli

import (
	"context"
	"fmt"
	"time"

	"accountant/internal/provider"
	"accountant/internal/provider/anthropic"
	"accountant/internal/provider/codex"
	"accountant/internal/vault"
)

// providerFor returns the CLI-side provider implementation by ID, or nil.
func providerFor(id string) provider.Provider {
	switch id {
	case "anthropic":
		return anthropic.New("")
	case "codex":
		return codex.New("")
	default:
		return nil
	}
}

// cliRefreshMargin mirrors the proxy's proactive-refresh margin.
const cliRefreshMargin = 30 * time.Minute

// refreshIfExpiring refreshes a credential whose provider reports an expiry
// within cliRefreshMargin. Non-expiring providers (and credentials without
// a readable expiry) come back unchanged.
func refreshIfExpiring(ctx context.Context, prov provider.Provider, cred []byte) (out []byte, refreshed bool, err error) {
	exp, ok := prov.(provider.CredentialExpirer)
	if !ok {
		return cred, false, nil
	}
	expiry, ok := exp.CredentialExpiry(cred)
	if !ok || expiry.After(time.Now().Add(cliRefreshMargin)) {
		return cred, false, nil
	}
	refresher, ok := prov.(provider.Refresher)
	if !ok {
		return cred, false, nil
	}
	newCred, err := refresher.Refresh(ctx, cred)
	if err != nil {
		return nil, false, err
	}
	return newCred, true, nil
}

// ensureFreshCredential refreshes an expiring credential and persists the
// result to the vault before returning it.
func ensureFreshCredential(ctx context.Context, prov provider.Provider, ref string, cred []byte) (out []byte, refreshed bool, err error) {
	out, refreshed, err = refreshIfExpiring(ctx, prov, cred)
	if err != nil {
		return nil, false, fmt.Errorf("credential refresh failed: %w", err)
	}
	if !refreshed {
		return cred, false, nil
	}
	if err := vault.NewKeychain().Put(ref, out); err != nil {
		return nil, false, fmt.Errorf("storing refreshed credential: %w", err)
	}
	return out, true, nil
}
