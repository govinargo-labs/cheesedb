//go:build !(darwin || linux || freebsd || openbsd || netbsd || illumos || windows)

package cheesedb

import "os"

// lockFile cannot lock on systems without flock(2) in Go's syscall package
// or Windows sharing modes (for example Solaris, AIX, Plan 9). It only
// creates the file: there is no lock, and the host must run one process
// per store folder (README Limits).
func lockFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
}
