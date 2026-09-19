//go:build windows

package securefile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// OpenCanonicalRegularRetained opens a canonical regular file while allowing
// only other readers. Keeping the returned handle open prevents ordinary
// writers, replacement, and deletion until the caller closes it.
func OpenCanonicalRegularRetained(path string) (*os.File, error) {
	absolute, root, components, err := canonicalWindowsComponents(path)
	if err != nil || len(components) == 0 {
		return nil, fmt.Errorf("path is not canonical")
	}
	if err := verifyWindowsAncestors(root, components[:len(components)-1]); err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(absolute)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil || !safeWindowsFileAttributes(information.FileAttributes) || information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("path is not a regular file")
	}
	resolved, err := finalPathByHandle(handle)
	if err != nil || !strings.EqualFold(filepath.Clean(resolved), absolute) {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("opened file changed identity")
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("open retained regular file")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		_ = file.Close()
		return nil, fmt.Errorf("path is not a regular file")
	}
	if err := verifyWindowsAncestors(root, components[:len(components)-1]); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}
