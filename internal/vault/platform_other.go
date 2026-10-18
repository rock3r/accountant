//go:build !darwin && !linux

package vault

import "fmt"

// KeychainAvailable reports whether the macOS Keychain backend is usable
// on this platform.
const KeychainAvailable = false

// SecretServiceAvailable reports whether the Secret Service backend is
// usable on this platform.
const SecretServiceAvailable = false

// NewPlatformVault is never called on unsupported platforms; it exists
// so the selector compiles uniformly.
func NewPlatformVault() (Vault, error) {
	return nil, fmt.Errorf("vault: no platform vault on this OS; set vault_backend = \"file\"")
}

// NewSecretService is unavailable on this platform.
func NewSecretService() (Vault, error) {
	return nil, fmt.Errorf("vault: Secret Service backend requires Linux")
}
