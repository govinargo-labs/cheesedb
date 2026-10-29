//go:build windows

package cheesedb

import (
	"errors"
	"os"
	"syscall"
)

// errSharingViolation is Windows' ERROR_SHARING_VIOLATION (the syscall
// package has no name for it).
const errSharingViolation = syscall.Errno(32)

// lockFile opens path with sharing switched off (share mode 0). While this
// handle is open, any other open of the file, in this process or another,
// fails with a sharing violation: Windows' own exclusive lock. Windows
// closes the handle when the process exits or crashes, so a crash never
// leaves a stale lock. (os.OpenFile always allows sharing, so this calls
// CreateFile directly.)
func lockFile(path string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil,
		syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, errSharingViolation) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}
