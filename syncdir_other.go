//go:build !windows

package cheesedb

import "os"

// syncDir flushes the directory entry itself, so a file created or renamed
// just before a crash is still there after it.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
