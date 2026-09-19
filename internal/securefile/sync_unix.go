//go:build !windows

package securefile

// SyncRegularNoFollow durably flushes one regular file without following a
// symbolic link in its final path component.
func SyncRegularNoFollow(path string) error {
	file, err := OpenRegularNoFollow(path)
	if err != nil {
		return err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
