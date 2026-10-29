//go:build darwin || linux || freebsd || openbsd || netbsd || illumos

package cheesedb

import (
	"errors"
	"os"
	"syscall"
)

// lockFile takes an exclusive flock(2) on path, without waiting. The lock
// belongs to the open file: the OS drops it when the file is closed or the
// process dies, even by kill -9, so a crash never leaves a stale lock. A
// second open of the file, in this process or another, cannot take it.
func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return f, nil
}
