// Package lockfile provides the single flock-based lock that guards every
// read-modify-write of accountant's state and config files.
package lockfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// Locked runs fn while holding an exclusive OS-level lock on the lock file
// at path. The lock is process- and goroutine-safe at the OS level; callers
// must still serialize goroutines within their own process before calling
// Locked.
func Locked(path string, fn func() error) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0700); err != nil { // no-op where unsupported
		return err
	}
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if err := lockFile(lock); err != nil {
		return fmt.Errorf("locking %s: %w", path, err)
	}
	defer func() { _ = unlockFile(lock) }()
	return fn()
}
