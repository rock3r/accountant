//go:build darwin

package vault

import (
	"errors"
	"os"
	"testing"
)

// TestKeychainRoundTrip exercises the real macOS Keychain with a test ref.
func TestKeychainRoundTrip(t *testing.T) {
	if os.Getenv("ACCOUNTANT_TEST_KEYCHAIN") == "" {
		t.Skip("set ACCOUNTANT_TEST_KEYCHAIN=1 to run against the real Keychain")
	}
	k := NewKeychain()
	ref := "test-ref-vault"
	_ = k.Delete(ref)

	if _, err := k.Get(ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := k.Put(ref, []byte("sk-test-dummy-123")); err != nil {
		t.Fatal(err)
	}
	got, err := k.Get(ref)
	if err != nil || string(got) != "sk-test-dummy-123" {
		t.Fatalf("got %q %v", got, err)
	}
	if err := k.Put(ref, []byte("sk-test-dummy-456")); err != nil {
		t.Fatal(err)
	}
	got, err = k.Get(ref)
	if err != nil || string(got) != "sk-test-dummy-456" {
		t.Fatalf("after update got %q %v", got, err)
	}
	if err := k.Delete(ref); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Get(ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}
