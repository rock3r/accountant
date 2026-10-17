// Package vault stores account credentials and hands them only to the proxy.
// The interface is what keeps non-Keychain backends possible later.
package vault

import (
	"errors"
	"fmt"
	"sync"
)

// ErrNotFound is returned when a reference has no stored secret.
var ErrNotFound = errors.New("vault: credential not found")

// Vault stores one credential per account, addressed by an opaque ref.
type Vault interface {
	Get(ref string) ([]byte, error)
	Put(ref string, secret []byte) error
	Delete(ref string) error
}

// Memory is an in-memory backend for tests.
type Memory struct {
	mu      sync.Mutex
	secrets map[string][]byte
}

// NewMemory returns an empty in-memory vault.
func NewMemory() *Memory {
	return &Memory{secrets: map[string][]byte{}}
}

// Get returns a copy of the stored secret.
func (m *Memory) Get(ref string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.secrets[ref]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, ref)
	}
	return append([]byte(nil), s...), nil
}

// Put stores a copy of the secret.
func (m *Memory) Put(ref string, secret []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.secrets[ref] = append([]byte(nil), secret...)
	return nil
}

// Delete removes the secret.
func (m *Memory) Delete(ref string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.secrets[ref]; !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, ref)
	}
	delete(m.secrets, ref)
	return nil
}
