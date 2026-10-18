package vault

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newTestFile(t *testing.T) *File {
	t.Helper()
	f, err := NewFile(filepath.Join(t.TempDir(), "vault"))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestFileVaultRoundTrip(t *testing.T) {
	f := newTestFile(t)
	ref := "test-ref-1"
	if _, err := f.Get(ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := f.Put(ref, []byte("sk-test-123")); err != nil {
		t.Fatal(err)
	}
	got, err := f.Get(ref)
	if err != nil || string(got) != "sk-test-123" {
		t.Fatalf("got %q, %v", got, err)
	}
	// Overwrite.
	if err := f.Put(ref, []byte("sk-test-456")); err != nil {
		t.Fatal(err)
	}
	if got, err = f.Get(ref); err != nil || string(got) != "sk-test-456" {
		t.Fatalf("after overwrite got %q, %v", got, err)
	}
	// Structured credentials round-trip byte-identically.
	json := `{"access_token":"at","refresh_token":"rt,with\tpunct"}`
	if err := f.Put(ref, []byte(json)); err != nil {
		t.Fatal(err)
	}
	if got, err = f.Get(ref); err != nil || string(got) != json {
		t.Fatalf("json cred: got %q, %v", got, err)
	}
	if err := f.Delete(ref); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Get(ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := f.Delete(ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete: %v", err)
	}
}

func TestFileVaultPermissionsEnforced(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vault")
	f, err := NewFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Put("ref", []byte("secret")); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(filepath.Join(dir, "ref"))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credential file mode is %o, want 600", info.Mode().Perm())
	}
	// A credential made group-readable (e.g. copied in) must be refused,
	// not served.
	if err := os.Chmod(filepath.Join(dir, "ref"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Get("ref"); err == nil || strings.Contains(err.Error(), "group/world") == false {
		t.Fatalf("want permission error, got %v", err)
	}
	// A pre-existing permissive directory must be refused at open time.
	bad := filepath.Join(t.TempDir(), "bad-vault")
	if err := os.Mkdir(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFile(bad); err == nil || strings.Contains(err.Error(), "group/world") == false {
		t.Fatalf("want permission error on open, got %v", err)
	}
	// A symlinked vault root must be refused.
	link := filepath.Join(t.TempDir(), "link-vault")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFile(link); err == nil {
		t.Fatal("symlinked vault root must be refused")
	}
	// A credential replaced by a symlink must be refused.
	if err := os.Remove(filepath.Join(dir, "ref")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hostname", filepath.Join(dir, "ref")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Get("ref"); err == nil {
		t.Fatal("symlinked credential must be refused")
	}
}

func TestFileVaultRefValidation(t *testing.T) {
	f := newTestFile(t)
	for _, ref := range []string{"ok-ref.1_2~3=x@y:z", "codex-work", "anthropic-primary"} {
		if err := f.Put(ref, []byte("x")); err != nil {
			t.Errorf("ref %q rejected: %v", ref, err)
		}
	}
	for _, ref := range []string{"", "a/b", "..", "a/../b", ".", strings.Repeat("a", 129), "has space", "semi;colon"} {
		if err := f.Put(ref, []byte("x")); err == nil {
			t.Errorf("unsafe ref %q accepted", ref)
		}
	}
}

func TestFileVaultEmptyRefAndSecret(t *testing.T) {
	f := newTestFile(t)
	if err := f.Put("ref", nil); err == nil {
		t.Error("empty secret must be rejected")
	}
	if err := f.Put("ref", []byte{}); err == nil {
		t.Error("empty secret must be rejected")
	}
}

// TestFileVaultConcurrentReadWrite checks the atomic-rename property:
// a reader never observes a partial or interleaved secret while a writer
// rewrites it.
func TestFileVaultConcurrentReadWrite(t *testing.T) {
	f := newTestFile(t)
	ref := "hot-ref"
	long := strings.Repeat("A", 4096)
	other := strings.Repeat("B", 4096)
	if err := f.Put(ref, []byte(long)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() { //nolint:varcheck
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			val := long
			if i%2 == 1 {
				val = other
			}
			if err := f.Put(ref, []byte(val)); err != nil {
				t.Errorf("put: %v", err)
				return
			}
			i++
		}
	}()
	for i := 0; i < 200; i++ {
		got, err := f.Get(ref)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if string(got) != long && string(got) != other {
			t.Fatalf("torn read: %d bytes starting %q", len(got), got[:8])
		}
	}
	close(stop)
	wg.Wait()
	// No temp files may linger.
	entries, _ := os.ReadDir(f.dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("leftover temp file %q", e.Name())
		}
	}
}

// TestFileVaultPutFailsOnMissingDir exercises the error path when the
// directory disappears between NewFile and Put.
func TestFileVaultPutFailsOnMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vault")
	f, err := NewFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	err = f.Put("ref", []byte("sk-secret-value"))
	if err == nil {
		t.Fatal("expected error writing into missing directory")
	}
	if strings.Contains(err.Error(), "sk-secret-value") {
		t.Fatal("error must not include the secret")
	}
}
