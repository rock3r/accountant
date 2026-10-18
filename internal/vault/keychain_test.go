//go:build darwin

package vault

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
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

// liveChunks returns the head of a chunked ref for inspection.
func liveChunks(t *testing.T, k *Keychain, ref string) chunkHead {
	t.Helper()
	raw, err := k.getRaw(keychainService, ref)
	if err != nil {
		t.Fatalf("read head: %v", err)
	}
	head, ok := parseChunkHead(string(raw))
	if !ok {
		t.Fatalf("ref %s is not chunked: %q", ref, raw)
	}
	return head
}

// assertChunkless asserts no chunk material for ref remains anywhere.
func assertChunkless(t *testing.T, k *Keychain, ref string, heads ...chunkHead) {
	t.Helper()
	if _, err := k.getRaw(chunkService, pendingRef(ref)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pending marker survived: %v", err)
	}
	for _, h := range heads {
		for i := 1; i <= h.count; i++ {
			name := chunkRef(ref, i)
			if h.gen != 0 {
				name = chunkItemRef(ref, h.gen, i)
			}
			service := keychainService
			if h.gen != 0 {
				service = chunkService
			}
			if _, err := k.getRaw(service, name); !errors.Is(err, ErrNotFound) {
				t.Fatalf("chunk %s survived: %v", name, err)
			}
		}
	}
}

// TestKeychainChunkedRoundTrip exercises large credentials: security -i
// truncates input lines at ~4 KB, so big payloads must be chunked. Chunks
// live in a disjoint keychain service and every rewrite uses a fresh
// generation, so rewrites never mutate chunks a concurrent reader may be
// following.
func TestKeychainChunkedRoundTrip(t *testing.T) {
	if os.Getenv("ACCOUNTANT_TEST_KEYCHAIN") == "" {
		t.Skip("set ACCOUNTANT_TEST_KEYCHAIN=1 to run against the real Keychain")
	}
	k := NewKeychain()
	ref := "test-ref-vault-chunked"
	_ = k.Delete(ref)

	// ~5 KB of JSON-ish data: well past the single-line limit.
	big := []byte(`{"access_token":"` + strings.Repeat("aB3-_.", 900) + `","refresh_token":"rt","account_id":"acc"}`)
	if err := k.Put(ref, big); err != nil {
		t.Fatal(err)
	}
	got, err := k.Get(ref)
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("chunked round trip: %d bytes, err %v", len(got), err)
	}
	head1 := liveChunks(t, k, ref)
	if head1.gen == 0 {
		t.Fatal("new writes must use generation-qualified chunks, not the legacy layout")
	}

	// Rewriting a credential of similar size must commit a fresh
	// generation and garbage-collect the previous one.
	big2 := []byte(`{"access_token":"` + strings.Repeat("c4-_.", 950) + `","refresh_token":"rt","account_id":"acc"}`)
	if err := k.Put(ref, big2); err != nil {
		t.Fatal(err)
	}
	got, err = k.Get(ref)
	if err != nil || !bytes.Equal(got, big2) {
		t.Fatalf("chunked rewrite: got %d bytes, err %v", len(got), err)
	}
	head2 := liveChunks(t, k, ref)
	assertChunkless(t, k, ref, head1)

	// Shrinking back to an inline value must remove the chunk items.
	if err := k.Put(ref, []byte("sk-small")); err != nil {
		t.Fatal(err)
	}
	if got, err = k.Get(ref); err != nil || string(got) != "sk-small" {
		t.Fatalf("after shrink: got %q %v", got, err)
	}
	assertChunkless(t, k, ref, head2)
	if _, err := k.getRaw(keychainService, chunkRef(ref, 1)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("legacy-name chunk appeared: %v", err)
	}

	// Growing again must work, and delete must remove everything.
	if err := k.Put(ref, big); err != nil {
		t.Fatal(err)
	}
	head3 := liveChunks(t, k, ref)
	if err := k.Delete(ref); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Get(ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	assertChunkless(t, k, ref, head3)
}

// TestKeychainChunkRefCollision pins the fix for chunk names colliding
// with legal credential refs: a real credential named "<ref>.c1" (or any
// .gN.cM shape) must survive chunked writes to <ref>.
func TestKeychainChunkRefCollision(t *testing.T) {
	if os.Getenv("ACCOUNTANT_TEST_KEYCHAIN") == "" {
		t.Skip("set ACCOUNTANT_TEST_KEYCHAIN=1 to run against the real Keychain")
	}
	k := NewKeychain()
	ref, shadow1, shadow2 := "test-ref-coll", "test-ref-coll.c1", "test-ref-coll.g7.c1"
	_ = k.Delete(ref)
	_ = k.Delete(shadow1)
	_ = k.Delete(shadow2)

	if err := k.Put(shadow1, []byte("other-account-secret")); err != nil {
		t.Fatal(err)
	}
	if err := k.Put(shadow2, []byte("another-secret")); err != nil {
		t.Fatal(err)
	}
	big := []byte(`{"access_token":"` + strings.Repeat("z9", 2200) + `"}`)
	if err := k.Put(ref, big); err != nil {
		t.Fatal(err)
	}
	if got, err := k.Get(shadow1); err != nil || string(got) != "other-account-secret" {
		t.Fatalf("shadow credential clobbered: %q %v", got, err)
	}
	if got, err := k.Get(shadow2); err != nil || string(got) != "another-secret" {
		t.Fatalf("shadow credential clobbered: %q %v", got, err)
	}
	// Shrinking and deleting must not touch the shadows either.
	if err := k.Put(ref, []byte("small")); err != nil {
		t.Fatal(err)
	}
	if err := k.Delete(ref); err != nil {
		t.Fatal(err)
	}
	if got, err := k.Get(shadow1); err != nil || string(got) != "other-account-secret" {
		t.Fatalf("shadow credential deleted: %q %v", got, err)
	}
	_ = k.Delete(shadow1)
	_ = k.Delete(shadow2)
}

// TestKeychainLegacyChunkedRead keeps the v2 layout (fixed ref.cN chunks
// in the main service, count-only head) readable and deletable.
func TestKeychainLegacyChunkedRead(t *testing.T) {
	if os.Getenv("ACCOUNTANT_TEST_KEYCHAIN") == "" {
		t.Skip("set ACCOUNTANT_TEST_KEYCHAIN=1 to run against the real Keychain")
	}
	k := NewKeychain()
	ref := "test-ref-vault-legacy"
	_ = k.Delete(ref)

	secret := []byte(`{"access_token":"` + strings.Repeat("L1-", 1000) + `"}`)
	hexed := hex.EncodeToString(secret)
	mid := len(hexed) / 2
	if err := k.putInline(keychainService, ref, fmt.Sprintf("%s2", chunksMagic)); err != nil {
		t.Fatal(err)
	}
	if err := k.putInline(keychainService, chunkRef(ref, 1), hexed[:mid]); err != nil {
		t.Fatal(err)
	}
	if err := k.putInline(keychainService, chunkRef(ref, 2), hexed[mid:]); err != nil {
		t.Fatal(err)
	}
	got, err := k.Get(ref)
	if err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("legacy chunked read: got %d bytes, err %v", len(got), err)
	}
	// Delete sweeps the legacy chunks too.
	if err := k.Delete(ref); err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{chunkRef(ref, 1), chunkRef(ref, 2)} {
		if _, err := k.getRaw(keychainService, r); !errors.Is(err, ErrNotFound) {
			t.Fatalf("legacy chunk %s survived: %v", r, err)
		}
	}
}

// TestKeychainPendingRecovery simulates a rotation that crashed between
// the pending marker and the head commit: the next Put (or Delete) must
// garbage-collect the abandoned generation.
func TestKeychainPendingRecovery(t *testing.T) {
	if os.Getenv("ACCOUNTANT_TEST_KEYCHAIN") == "" {
		t.Skip("set ACCOUNTANT_TEST_KEYCHAIN=1 to run against the real Keychain")
	}
	k := NewKeychain()
	ref := "test-ref-vault-pending"
	_ = k.Delete(ref)

	if err := k.Put(ref, []byte("old-inline-secret")); err != nil {
		t.Fatal(err)
	}
	// Simulated crashed rotation: pending + one stray chunk, head still
	// pointing at the inline value.
	strayGen := int64(1234567890)
	if err := k.putInline(chunkService, pendingRef(ref), fmt.Sprintf("%d:2", strayGen)); err != nil {
		t.Fatal(err)
	}
	if err := k.putInline(chunkService, chunkItemRef(ref, strayGen, 1), "deadbeef"); err != nil {
		t.Fatal(err)
	}
	if err := k.Put(ref, []byte("new-inline-secret")); err != nil {
		t.Fatal(err)
	}
	if got, err := k.Get(ref); err != nil || string(got) != "new-inline-secret" {
		t.Fatalf("after recovery put: got %q %v", got, err)
	}
	assertChunkless(t, k, ref, chunkHead{gen: strayGen, count: 2})

	// Same scenario, cleaned by Delete instead.
	if err := k.putInline(chunkService, pendingRef(ref), fmt.Sprintf("%d:1", strayGen)); err != nil {
		t.Fatal(err)
	}
	if err := k.putInline(chunkService, chunkItemRef(ref, strayGen, 1), "deadbeef"); err != nil {
		t.Fatal(err)
	}
	if err := k.Delete(ref); err != nil {
		t.Fatal(err)
	}
	assertChunkless(t, k, ref, chunkHead{gen: strayGen, count: 1})
	if _, err := k.Get(ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}

func TestParseChunkHead(t *testing.T) {
	cases := []struct {
		in   string
		want chunkHead
		ok   bool
	}{
		{"acct1:chunks:123:4", chunkHead{gen: 123, count: 4}, true},
		{"acct1:chunks:2", chunkHead{gen: 0, count: 2}, true}, // legacy v2
		{"987:3", chunkHead{gen: 987, count: 3}, true},        // pending marker
		{"acct1:chunks:0:4", chunkHead{}, false},
		{"acct1:chunks:5:0", chunkHead{}, false},
		{"acct1:chunks:x:4", chunkHead{}, false},
		{"acct1:chunks:9999999999999999999999:4", chunkHead{}, false},
		{"acct1:chunks:-3", chunkHead{}, false},
		{"", chunkHead{}, false},
		{"736b2d7465", chunkHead{}, false},
	}
	for _, c := range cases {
		got, ok := parseChunkHead(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("parseChunkHead(%q) = %+v, %v; want %+v, %v", c.in, got, ok, c.want, c.ok)
		}
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
