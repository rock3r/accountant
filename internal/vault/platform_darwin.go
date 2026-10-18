//go:build darwin

package vault

// KeychainAvailable reports whether the macOS Keychain backend is usable
// on this platform.
const KeychainAvailable = true

// NewPlatformVault returns the platform's preferred vault (Keychain).
func NewPlatformVault() (Vault, error) { return NewKeychain(), nil }
