package cli

import (
	"context"
	"fmt"
	"time"

	"accountant/internal/config"
	"accountant/internal/provider"
	"accountant/internal/provider/anthropic"
	"accountant/internal/provider/codex"
	"accountant/internal/refresh"
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

// ensureFreshCredential refreshes an expiring credential and persists the
// result to the vault before returning it. The read-refresh-write cycle
// runs under the shared per-credential lock, so it cannot race a refresh
// by the running daemon (or another CLI invocation) on the same rotating
// refresh token.
func ensureFreshCredential(ctx context.Context, prov provider.Provider, paths config.Paths, ref string, cred []byte) (out []byte, refreshed bool, err error) {
	exp, ok := prov.(provider.CredentialExpirer)
	if !ok {
		return cred, false, nil
	}
	expiry, ok := exp.CredentialExpiry(cred)
	if !ok || expiry.After(time.Now().Add(cliRefreshMargin)) {
		return cred, false, nil
	}
	if _, ok := prov.(provider.Refresher); !ok {
		return cred, false, nil
	}
	out, refreshed, err = refresh.IfNeeded(ctx, vault.NewKeychain(), prov, paths.StateFile, ref, cliRefreshMargin)
	if err != nil {
		return nil, false, fmt.Errorf("credential refresh failed: %w", err)
	}
	return out, refreshed, nil
}
