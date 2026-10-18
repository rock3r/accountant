package cli

import (
	"fmt"
	"path/filepath"

	"accountant/internal/config"
	"accountant/internal/vault"
)

// openVault returns the vault selected by the configuration: "auto"
// (default) picks the macOS Keychain on darwin and the file vault on
// every other platform; "keychain" and "file" select explicitly.
func openVault(paths config.Paths) (vault.Vault, error) {
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		return nil, err
	}
	dir := cfg.VaultDir
	if dir == "" {
		dir = filepath.Join(paths.StateDir, "vault")
	}
	switch cfg.VaultBackend {
	case "", "auto":
		if vault.KeychainAvailable {
			return vault.NewPlatformVault()
		}
		return vault.NewFile(dir)
	case "keychain":
		if !vault.KeychainAvailable {
			return nil, fmt.Errorf("vault_backend %q requires macOS", cfg.VaultBackend)
		}
		return vault.NewPlatformVault()
	case "file":
		return vault.NewFile(dir)
	default:
		return nil, fmt.Errorf("vault_backend %q is not a valid backend", cfg.VaultBackend)
	}
}

// openVaultOrNil is the best-effort variant for cleanup paths that must
// not fail hard when the vault cannot be opened.
func openVaultOrNil(paths config.Paths) vault.Vault {
	v, err := openVault(paths)
	if err != nil {
		return nil
	}
	return v
}
