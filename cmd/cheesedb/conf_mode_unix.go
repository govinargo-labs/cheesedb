//go:build unix

package main

import (
	"fmt"
	"io/fs"
	"strings"
)

// confModeError refuses a settings file that holds a key while other users can
// read it, the way ssh refuses a private key that anyone can read. The key is
// never part of the message.
func confModeError(path string, mode fs.FileMode, conf map[string]string) error {
	names := confKeyNames(conf)
	if len(names) == 0 || mode.Perm()&0o077 == 0 {
		return nil
	}
	return fmt.Errorf("%s holds %s and other users can read it (mode %04o). Make the file private:\n  chmod 600 %s",
		path, strings.Join(names, " and "), mode.Perm(), shellPath(path))
}

// shellPath quotes a path for the chmod line only when the shell would
// otherwise split or expand it: most paths read better unquoted.
func shellPath(path string) string {
	for _, r := range path {
		if !shellSafe(r) {
			return "'" + strings.ReplaceAll(path, "'", "'\\''") + "'"
		}
	}
	return path
}

// shellSafe reports whether a shell leaves this character alone inside a
// word. The set is deliberately narrow: an unnecessary pair of quotes is
// harmless, an unquoted character a shell expands is not.
func shellSafe(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("/._-@", r)
}
