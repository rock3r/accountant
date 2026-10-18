//go:build linux

package vault

// KeychainAvailable reports whether the macOS Keychain backend is usable
// on this platform.
const KeychainAvailable = false

// SecretServiceAvailable reports whether the Secret Service backend is
// usable on this platform.
const SecretServiceAvailable = true

// NewPlatformVault returns the platform's preferred vault: the Secret
// Service (the freedesktop standard, any provider). Headless boxes
// without a provider select the file backend explicitly via
// vault_backend = "file"; auto stays explicit rather than silently
// scattering credentials across backends.
func NewPlatformVault() (Vault, error) { return NewSecretService() }
