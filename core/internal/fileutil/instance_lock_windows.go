//go:build windows

package fileutil

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
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

	var overlapped windows.Overlapped
	handle := windows.Handle(f.Fd())

	// Try non-blocking exclusive lock with LOCKFILE_FAIL_IMMEDIATELY
	if err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another instance is already running")
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
	var overlapped windows.Overlapped
	handle := windows.Handle(l.file.Fd())
	_ = windows.UnlockFileEx(handle, 0, 1, 0, &overlapped)

	// Close file
	err := l.file.Close()
	l.file = nil

	return err
}
