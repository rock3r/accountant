package vault

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// File is a filesystem-backed vault: one file per credential under a
// dedicated directory. Protection at rest is filesystem permissions — the
// directory must be 0700 and every credential file 0600, both enforced,
// not assumed. For real deployments run the daemon as a dedicated user
// and keep the directory outside any home-directory sync/share (see
// docs/remote-deploy.md).
//
// Unlike the macOS Keychain backend there is no line-length limit, so no
// chunking: each credential is written whole, atomically (temp file +
// rename), and never partially visible.
type File struct {
	dir string
}

// NewFile returns a File vault rooted at dir, creating the directory with
// strict permissions when absent and refusing any pre-existing directory
// that is group- or world-accessible.
func NewFile(dir string) (*File, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(abs)
	switch {
	case err == nil:
		if !info.IsDir() {
			return nil, fmt.Errorf("vault: %s is not a directory", abs)
		}
		if info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("vault: %s must not be group/world accessible (mode %o)", abs, info.Mode().Perm())
		}
	case errors.Is(err, os.ErrNotExist):
		// MkdirAll applies the mode to every component it creates;
		// pre-existing parents are left alone, and the final component is
		// verified below either way.
		if err := os.MkdirAll(abs, 0o700); err != nil {
			return nil, err
		}
		info, err = os.Lstat(abs)
		if err != nil {
			return nil, err
		}
		if info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("vault: %s must not be group/world accessible (mode %o)", abs, info.Mode().Perm())
		}
	default:
		return nil, err
	}
	return &File{dir: abs}, nil
}

func (f *File) path(ref string) (string, error) {
	if !safeFileRef.MatchString(ref) {
		return "", fmt.Errorf("vault: unsafe credential ref")
	}
	return filepath.Join(f.dir, ref), nil
}

// Get reads the stored secret. The secret appears on this process's
// stdout/stderr pipes only.
func (f *File) Get(ref string) ([]byte, error) {
	p, err := f.path(ref)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, ref)
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("vault: %s is not a regular file", ref)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("vault: credential %s must not be group/world readable (mode %o)", ref, info.Mode().Perm())
	}
	secret, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	return secret, nil
}

// Put stores the secret atomically: a temp file in the same directory is
// fully written, synced, and renamed over the target, so readers see
// either the whole previous value or the whole new one.
func (f *File) Put(ref string, secret []byte) error {
	if len(ref) > maxFileRefLen {
		return fmt.Errorf("vault: credential ref too long")
	}
	p, err := f.path(ref)
	if err != nil {
		return err
	}
	if len(secret) == 0 {
		return fmt.Errorf("vault: empty credential")
	}
	tmp, err := os.CreateTemp(f.dir, "."+ref+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck // best-effort cleanup; no-op after rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close() //nolint:errcheck // already failing
		return err
	}
	if _, err := tmp.Write(secret); err != nil {
		tmp.Close() //nolint:errcheck // already failing
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close() //nolint:errcheck // already failing
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, p); err != nil {
		return err
	}
	return syncDir(f.dir)
}

// Delete removes the stored secret.
func (f *File) Delete(ref string) error {
	p, err := f.path(ref)
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s", ErrNotFound, ref)
	}
	return err
}

// syncDir fsyncs the directory so a completed rename survives a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close() //nolint:errcheck // read-only handle
	return d.Sync()
}
