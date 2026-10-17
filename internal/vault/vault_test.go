package vault

import (
	"errors"
	"testing"
)

func TestMemoryVault(t *testing.T) {
	v := NewMemory()
	if _, err := v.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := v.Put("a", []byte("secret")); err != nil {
		t.Fatal(err)
	}
	got, err := v.Get("a")
	if err != nil || string(got) != "secret" {
		t.Fatalf("got %q %v", got, err)
	}
	// Returned slices are copies.
	got[0] = 'X'
	again, _ := v.Get("a")
	if string(again) != "secret" {
		t.Fatal("mutation leaked into vault")
	}
	if err := v.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Get("a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}
