//go:build !unix

package main

import "io/fs"

// confModeError does nothing here: where the permission bits do not say who can
// read a file, there is nothing to check.
func confModeError(path string, mode fs.FileMode, conf map[string]string) error {
	return nil
}
