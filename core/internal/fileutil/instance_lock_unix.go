//go:build !windows

package fileutil

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// InstanceLock represents a non-blocking exclusive file lock for single instance control.
type InstanceLock struct {
	file *os.File
	path string
}

// AcquireInstanceLock attempts to acquire a non-blocking exclusive lock.
// Returns an error if another instance already holds the lock.
func AcquireInstanceLock(lockPath string) (*InstanceLock, error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}

	// Try non-blocking exclusive lock
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if err == unix.EWOULDBLOCK {
			return nil, fmt.Errorf("another instance is already running")
		}
		return nil, fmt.Errorf("acquire lock: %w", err)
	}

	return &InstanceLock{
		file: f,
		path: lockPath,
	}, nil
}

// Release releases the lock and closes the file.
func (l *InstanceLock) Release() error {
	if l.file == nil {
		return nil
	}

	// Unlock
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)

	// Close file
	err := l.file.Close()
	l.file = nil

	return err
}
