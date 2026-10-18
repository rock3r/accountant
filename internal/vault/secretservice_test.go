//go:build linux

package vault

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// newTestSecretService skips when the Secret Service is unusable (no
// provider on CI runners, locked collection on machines nobody has
// logged into); where it is available it exercises the real provider.
func newTestSecretService(t *testing.T) *SecretService {
	t.Helper()
	s, err := NewSecretService()
	if err != nil {
		t.Skipf("Secret Service unavailable or locked: %v", err)
	}
	return s
}

// TestSecretServiceProbeNeverHangs runs everywhere: the probe must
// return quickly with a diagnostic, never block on a keyring prompt —
// a daemon cannot answer one.
func TestSecretServiceProbeNeverHangs(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		_, err := NewSecretService()
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			return // unlocked provider: round-trip tests cover the rest
		}
		msg := err.Error()
		for _, want := range []string{"org.freedesktop.secrets", "locked", "session bus", "timed out"} {
			if strings.Contains(msg, want) {
				return // a diagnostic, not a hang and not a panic
			}
		}
		t.Fatalf("unexpected probe error: %v", err)
	case <-time.After(8 * time.Second):
		t.Fatal("probe hung: NewSecretService must never block on a keyring prompt")
	}
}

func TestSecretServiceRoundTrip(t *testing.T) {
	s := newTestSecretService(t)
	ref := "accountant-vault-test-roundtrip"
	t.Cleanup(func() { _ = s.Delete(ref) })

	if _, err := s.Get(ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := s.Put(ref, []byte("sk-test-123")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ref)
	if err != nil || string(got) != "sk-test-123" {
		t.Fatalf("got %q, %v", got, err)
	}
	// Overwrite.
	if err := s.Put(ref, []byte("sk-test-456")); err != nil {
		t.Fatal(err)
	}
	if got, err = s.Get(ref); err != nil || string(got) != "sk-test-456" {
		t.Fatalf("after overwrite got %q, %v", got, err)
	}
	// Structured credentials round-trip byte-identically.
	json := `{"access_token":"at","refresh_token":"rt,with punct"}`
	if err := s.Put(ref, []byte(json)); err != nil {
		t.Fatal(err)
	}
	if got, err = s.Get(ref); err != nil || string(got) != json {
		t.Fatalf("json cred: got %q, %v", got, err)
	}
	if err := s.Delete(ref); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := s.Delete(ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete: %v", err)
	}
}

func TestSecretServiceValidation(t *testing.T) {
	s := newTestSecretService(t)
	for _, ref := range []string{"", "a/b", "..", "has space", strings.Repeat("a", 129)} {
		if err := s.Put(ref, []byte("x")); err == nil {
			t.Errorf("unsafe ref %q accepted", ref)
		}
	}
	if err := s.Put("accountant-vault-test-ref", nil); err == nil {
		t.Error("empty secret must be rejected")
	}
	if err := s.Put("accountant-vault-test-nul", []byte("a\x00b")); err == nil {
		t.Error("NUL-containing secret must be rejected")
	}
}
