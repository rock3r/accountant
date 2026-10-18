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
	// Structured credentials (Codex OAuth JSON) round-trip too.
	jsonCred := `{"access_token":"at.with.dots","refresh_token":"rt,with\tpunct","account_id":"acc"}`
	if err := k.Put(ref, []byte(jsonCred)); err != nil {
		t.Fatal(err)
	}
	got, err = k.Get(ref)
	if err != nil || string(got) != jsonCred {
		t.Fatalf("json cred: got %q %v", got, err)
	}
	if err := k.Delete(ref); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Get(ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}

func TestDecodeStored(t *testing.T) {
	if got, ok := decodeStored([]byte("736b2d74657374")); !ok || string(got) != "sk-test" {
		t.Errorf("hex value: got %q, %v", got, ok)
	}
	// Legacy raw secrets are returned unchanged.
	if _, ok := decodeStored([]byte("sk-ant-oat01-raw")); ok {
		t.Error("raw token must not decode")
	}
	if _, ok := decodeStored([]byte("")); ok {
		t.Error("empty must not decode")
	}
	if _, ok := decodeStored([]byte("ABCDEF")); ok {
		t.Error("non-canonical (uppercase) hex must not decode")
	}
}
