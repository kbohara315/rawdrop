//go:build unix

package journal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Lock takes an exclusive, non-blocking advisory lock on the store's
// jobs.lock, so a second RawDrop instance cannot own the same job. The lock
// is released by Close, and by process exit. Repeated calls on the same
// store are idempotent.
func (s *Store) Lock() error {
	if s.lock != nil {
		return nil
	}
	path := filepath.Join(filepath.Dir(s.Path), lockFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("journal: open lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return fmt.Errorf("journal: %s is locked by another RawDrop instance", filepath.Dir(s.Path))
		}
		return fmt.Errorf("journal: lock %s: %w", path, err)
	}
	s.lock = f
	return nil
}

// Close releases the lock. Close without a lock is a no-op.
func (s *Store) Close() error {
	if s.lock == nil {
		return nil
	}
	f := s.lock
	s.lock = nil
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("journal: release lock: %w", err)
	}
	return nil
}
