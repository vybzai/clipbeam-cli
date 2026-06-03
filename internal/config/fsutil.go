package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// ensureDir0700 creates dir (and parents) mode 0700, and re-chmods it 0700 if it
// pre-existed looser. It self-heals a vanished dir (PLAN §4: dirs 0700, self-heal +
// re-chmod on load, mirrors Swift ensureDirectory).
func ensureDir0700(dir string) error {
	if dir == "" {
		return fmt.Errorf("clipbeam: empty directory path")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("clipbeam: create dir %s: %w", dir, err)
	}
	// MkdirAll does not tighten an existing looser dir; do it explicitly.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("clipbeam: chmod 0700 %s: %w", dir, err)
	}
	return nil
}

// writeFileAtomic0600 writes data to path atomically at mode 0600: it writes a
// sibling temp file, fsyncs it, chmods 0600, then renames it over path. The parent
// dir is created 0700 first (self-heal). The rename is atomic on POSIX, so a reader
// never sees a torn file (PLAN §4: config/recents/token 0600 atomic).
func writeFileAtomic0600(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := ensureDir0700(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".clipbeam-tmp-*")
	if err != nil {
		return fmt.Errorf("clipbeam: create temp for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if anything below fails before the rename.
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("clipbeam: write temp for %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("clipbeam: fsync temp for %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("clipbeam: close temp for %s: %w", path, err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("clipbeam: chmod 0600 temp for %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("clipbeam: rename temp into %s: %w", path, err)
	}
	tmpName = "" // renamed; suppress cleanup
	return nil
}
