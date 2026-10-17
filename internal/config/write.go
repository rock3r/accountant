package config

import (
	"os"
	"path/filepath"

	"accountant/internal/lockfile"
)

// Save writes the config atomically (temp file, fsync, rename, 0600).
func Save(path string, c *Config) error {
	data, err := Marshal(c)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// LockedUpdate runs fn over a freshly loaded config while holding the shared
// lock file, then saves atomically when fn returns nil.
func LockedUpdate(configPath, lockPath string, fn func(*Config) error) error {
	return lockfile.Locked(lockPath, func() error {
		c, err := Load(configPath)
		if err != nil {
			return err
		}
		if err := fn(c); err != nil {
			return err
		}
		return Save(configPath, c)
	})
}
