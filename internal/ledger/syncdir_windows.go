//go:build windows

package ledger

// syncDir is a no-op on Windows: os.File.Sync on a directory handle fails
// with "Access is denied". NTFS journals metadata, so a crash can still
// recover an index folder's directory entry, but a new index folder is not
// guaranteed on disk when this returns (README Limits).
func syncDir(dir string) error {
	return nil
}
