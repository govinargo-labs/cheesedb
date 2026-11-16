//go:build !windows

package ledger

import "os"

// syncDir flushes the directory entry itself, so an index folder created
// or renamed just before a crash is still there after it.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
