package cli

import (
	"fmt"
	"path/filepath"

	"accountant/internal/config"
	"accountant/internal/vault"
)

// openVault returns the vault selected by the configuration. "auto"
// (default) picks the macOS Keychain on darwin and the Secret Service
// (freedesktop standard, any provider) on Linux; "keychain",
// "secret-service" and "file" select explicitly. Headless Linux boxes
// without a Secret Service provider set "file".
func openVault(paths config.Paths) (vault.Vault, error) {
	v, _, err := resolveVault(paths)
	return v, err
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

// vaultBackendName reports which backend auto resolves to, for doctor.
func vaultBackendName(paths config.Paths) string {
	_, name, err := resolveVault(paths)
	if err != nil {
		return "unavailable"
	}
	return name
}

func resolveVault(paths config.Paths) (vault.Vault, string, error) {
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		return nil, "", err
	}
	dir := cfg.VaultDir
	if dir == "" {
		dir = filepath.Join(paths.StateDir, "vault")
	}
	switch cfg.VaultBackend {
	case "", "auto":
		if vault.KeychainAvailable {
			v, err := vault.NewPlatformVault()
			return v, "keychain", err
		}
		if vault.SecretServiceAvailable {
			v, err := vault.NewPlatformVault()
			return v, "secret-service", err
		}
		v, err := vault.NewFile(dir)
		return v, "file", err
	case "keychain":
		if !vault.KeychainAvailable {
			return nil, "", fmt.Errorf("vault_backend %q requires macOS", cfg.VaultBackend)
		}
		v, err := vault.NewPlatformVault()
		return v, "keychain", err
	case "secret-service":
		if !vault.SecretServiceAvailable {
			return nil, "", fmt.Errorf("vault_backend %q requires Linux with an org.freedesktop.secrets provider (gnome-keyring standalone, KWallet, or KeePassXC)", cfg.VaultBackend)
		}
		v, err := vault.NewSecretService()
		return v, "secret-service", err
	case "file":
		v, err := vault.NewFile(dir)
		return v, "file", err
	default:
		return nil, "", fmt.Errorf("vault_backend %q is not a valid backend", cfg.VaultBackend)
	}
}
