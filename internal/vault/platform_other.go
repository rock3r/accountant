//go:build !darwin

package vault

import "fmt"

// KeychainAvailable reports whether the macOS Keychain backend is usable
// on this platform.
const KeychainAvailable = false

// NewPlatformVault is never called on non-darwin platforms; it exists so
// the selector compiles uniformly.
func NewPlatformVault() (Vault, error) {
	return nil, fmt.Errorf("vault: no platform vault on this OS")
}
