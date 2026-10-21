//go:build !unix

// This file keeps the journal portable to platforms without flock. The
// fallback uses an exclusively created lock file, which detects a second
// running instance but, unlike flock, is left behind after a crash.

package journal

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Lock takes ownership of the store by creating its jobs.lock exclusively.
// Repeated calls on the same store are idempotent.
func (s *Store) Lock() error {
	if s.lock != nil {
		return nil
	}
	path := filepath.Join(filepath.Dir(s.Path), lockFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("journal: %s is locked by another RawDrop instance", filepath.Dir(s.Path))
		}
		return fmt.Errorf("journal: open lock %s: %w", path, err)
	}
	s.lock = f
	return nil
}

// Close removes the fallback lock file. Close without a lock is a no-op.
func (s *Store) Close() error {
	if s.lock == nil {
		return nil
	}
	path := s.lock.Name()
	err := s.lock.Close()
	s.lock = nil
	if removeErr := os.Remove(path); err == nil {
		err = removeErr
	}
	if err != nil {
		return fmt.Errorf("journal: release lock: %w", err)
	}
	return nil
}
