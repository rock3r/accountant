//go:build darwin

package vault

import "fmt"

// KeychainAvailable reports whether the macOS Keychain backend is usable
// on this platform.
const KeychainAvailable = true

// SecretServiceAvailable reports whether the Secret Service backend is
// usable on this platform.
const SecretServiceAvailable = false

// NewPlatformVault returns the platform's preferred vault (Keychain).
func NewPlatformVault() (Vault, error) { return NewKeychain(), nil }

// NewSecretService is unavailable on this platform.
func NewSecretService() (Vault, error) {
	return nil, fmt.Errorf("vault: Secret Service backend requires Linux")
}
