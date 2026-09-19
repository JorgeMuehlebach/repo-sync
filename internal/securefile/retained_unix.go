//go:build !windows

package securefile

import "os"

// OpenCanonicalRegularRetained opens a canonical regular file for a caller
// that keeps the descriptor alive through use. POSIX descriptors retain the
// verified object even if its directory entry later changes.
func OpenCanonicalRegularRetained(path string) (*os.File, error) {
	return OpenCanonicalRegular(path)
}
