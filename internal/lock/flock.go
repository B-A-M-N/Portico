// Package lock provides advisory file locking via BSD flock.
//
// Both the launcher and supervisor use this package so they serialize
// on the same lock mechanism. We use flock (not fcntl) for consistency
// with the launcher's startup lock.
package lock

import (
	"fmt"
	"os"
	"syscall"
)

// Lock acquires an exclusive, non-blocking BSD flock on the open file.
func Lock(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("flock: %w", err)
	}
	return nil
}

// Unlock releases the BSD flock on the open file.
func Unlock(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		return fmt.Errorf("unlock: %w", err)
	}
	return nil
}

// LockBlocking acquires an exclusive, blocking BSD flock on the open file.
func LockBlocking(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("flock: %w", err)
	}
	return nil
}
