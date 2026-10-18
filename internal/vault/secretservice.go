//go:build linux

package vault

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	"github.com/godbus/dbus/v5"
	keyring "github.com/zalando/go-keyring"
)

// secretServiceAccount is the Secret Service collection/account all
// accountant credentials live under, mirroring the Keychain service
// name on macOS.
const secretServiceAccount = "accountant"

// opTimeout bounds every Secret Service call. A daemon can never answer
// a keyring unlock prompt, so a locked collection must fail fast with a
// clear error instead of hanging the proxy.
const opTimeout = 10 * time.Second

// SecretService is a vault backed by the freedesktop Secret Service API
// (org.freedesktop.secrets on the session D-Bus) — the Linux equivalent
// of the macOS Keychain. Any provider implementing the standard works:
// gnome-keyring (standalone, no GNOME desktop required), KWallet, or
// KeePassXC's Secret Service integration. The credentials are encrypted
// at rest by the provider and unlocked by the user's login.
//
// Unlock lifecycle, stated honestly: like the macOS login Keychain, the
// collection unlocks at PAM login. This backend never prompts; when the
// collection is locked (no console login since boot) it fails with a
// clear error instead, and headless boxes should use the file backend.
// See docs/remote-deploy.md.
type SecretService struct{}

// NewSecretService returns a Secret Service vault. It fails fast when
// no provider answers on the session bus, or when the default
// collection is locked — both without ever invoking a prompt.
func NewSecretService() (*SecretService, error) {
	if err := probeSecretService(); err != nil {
		return nil, err
	}
	return &SecretService{}, nil
}

const (
	secretsBusName    = "org.freedesktop.secrets"
	defaultCollection = "/org/freedesktop/secrets/aliases/default"
	collectionIface   = "org.freedesktop.Secret.Collection"
	lockedProp        = "Locked"
)

// probeSecretService checks the standard, provider-neutral facts: is
// org.freedesktop.secrets on the session bus, and is the default
// collection unlocked?
func probeSecretService() error {
	return guarded(5*time.Second, func() error {
		conn, err := dbus.SessionBus()
		if err != nil {
			return fmt.Errorf("session bus unavailable: %w", err)
		}
		var names []string
		if err := conn.BusObject().Call("org.freedesktop.DBus.ListNames", 0).Store(&names); err != nil {
			return fmt.Errorf("listing bus names: %w", err)
		}
		found := false
		for _, n := range names {
			if n == secretsBusName {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("no org.freedesktop.secrets provider on the session bus (install one — gnome-keyring standalone, KWallet, or KeePassXC — or set vault_backend = %q)", "file")
		}
		obj := conn.Object(secretsBusName, defaultCollection)
		locked, err := propBool(obj, collectionIface, lockedProp)
		if err != nil {
			return fmt.Errorf("reading default collection state: %w", err)
		}
		if locked {
			return fmt.Errorf("the Secret Service collection is locked — log in once on the console (or enable auto-login); headless boxes should set vault_backend = %q", "file")
		}
		return nil
	})
}

func propBool(obj dbus.BusObject, iface, name string) (bool, error) {
	v, err := obj.GetProperty(iface + "." + name)
	if err != nil {
		return false, err
	}
	b, ok := v.Value().(bool)
	if !ok {
		return false, fmt.Errorf("property %s.%s is not a boolean", iface, name)
	}
	return b, nil
}

// guarded runs f with a hard timeout. On timeout the caller gets an
// error; f's goroutine is abandoned (it is parked on a D-Bus channel
// receive and cannot take the prompt path again).
func guarded(d time.Duration, f func() error) error {
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		return fmt.Errorf("vault: Secret Service operation timed out after %s (provider unresponsive or prompting?)", d)
	}
}

// Get returns the stored secret for ref. The secret never passes through
// a command line or a file accountant manages.
func (s *SecretService) Get(ref string) ([]byte, error) {
	if err := validateRef(ref); err != nil {
		return nil, err
	}
	var secret string
	err := guarded(opTimeout, func() error {
		got, err := keyring.Get(secretServiceAccount, ref)
		secret = got
		return err
	})
	if errors.Is(err, keyring.ErrNotFound) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, ref)
	}
	if err != nil {
		return nil, err
	}
	return []byte(secret), nil
}

// Put stores the secret under ref in the unlocked default collection.
func (s *SecretService) Put(ref string, secret []byte) error {
	if err := validateRef(ref); err != nil {
		return err
	}
	if len(secret) == 0 {
		return fmt.Errorf("vault: empty credential")
	}
	// D-Bus strings cannot carry NUL bytes; all accountant credentials
	// (API tokens, OAuth JSON) are ASCII, so reject rather than corrupt.
	if bytes.IndexByte(secret, 0) >= 0 {
		return fmt.Errorf("vault: credential contains NUL bytes")
	}
	return guarded(opTimeout, func() error {
		return keyring.Set(secretServiceAccount, ref, string(secret))
	})
}

// Delete removes the stored secret for ref.
func (s *SecretService) Delete(ref string) error {
	if err := validateRef(ref); err != nil {
		return err
	}
	err := guarded(opTimeout, func() error {
		return keyring.Delete(secretServiceAccount, ref)
	})
	if errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("%w: %s", ErrNotFound, ref)
	}
	return err
}
