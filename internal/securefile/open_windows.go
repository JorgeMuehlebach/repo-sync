//go:build windows

package securefile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// OpenRegularNoFollow opens the final path component as a reparse point so a
// last-moment junction or symbolic-link swap is rejected rather than followed.
func OpenRegularNoFollow(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	var handleInformation windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &handleInformation); err != nil || !safeWindowsFileAttributes(handleInformation.FileAttributes) {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("path is a reparse point")
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("open regular file")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		_ = file.Close()
		return nil, fmt.Errorf("path is not a regular file")
	}
	return file, nil
}

func VerifyDirectoryNoReparse(path string) (os.FileInfo, error) {
	absolute, root, components, err := canonicalWindowsComponents(path)
	if err != nil || len(components) == 0 {
		return nil, fmt.Errorf("path is not a canonical directory")
	}
	if err := verifyWindowsAncestors(root, components[:len(components)-1]); err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(absolute)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil || !safeWindowsFileAttributes(information.FileAttributes) || information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("path is not a safe directory")
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("open safe directory")
	}
	info, err := file.Stat()
	resolved, resolveErr := finalPathByHandle(handle)
	_ = file.Close()
	if err != nil || resolveErr != nil || !strings.EqualFold(filepath.Clean(resolved), absolute) || !info.IsDir() {
		return nil, fmt.Errorf("path is not a safe directory")
	}
	if err := verifyWindowsAncestors(root, components[:len(components)-1]); err != nil {
		return nil, err
	}
	return info, nil
}

// OpenRegularBeneath rejects every reparse-point ancestor, opens the final
// component without following reparses, and verifies the final handle still
// resolves to the exact expected path. The after-open ancestor check closes a
// swap window while the final-handle path catches transient junction swaps.
func OpenRegularBeneath(root, relative string) (*os.File, error) {
	components, err := windowsBeneathComponents(relative)
	if err != nil {
		return nil, err
	}
	if err := verifyWindowsAncestors(root, components[:len(components)-1]); err != nil {
		return nil, err
	}
	expected, err := filepath.Abs(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil {
		return nil, err
	}
	file, err := OpenRegularNoFollow(expected)
	if err != nil {
		return nil, err
	}
	resolved, err := finalPathByHandle(windows.Handle(file.Fd()))
	if err != nil || !strings.EqualFold(filepath.Clean(resolved), filepath.Clean(expected)) {
		_ = file.Close()
		return nil, fmt.Errorf("opened file escaped its repository root")
	}
	if err := verifyWindowsAncestors(root, components[:len(components)-1]); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func windowsBeneathComponents(relative string) ([]string, error) {
	if relative == "" || filepath.IsAbs(relative) || strings.Contains(relative, "\\") {
		return nil, fmt.Errorf("relative path is unsafe")
	}
	components := strings.Split(relative, "/")
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			return nil, fmt.Errorf("relative path is unsafe")
		}
	}
	return components, nil
}

func canonicalWindowsComponents(path string) (string, string, []string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil || !strings.EqualFold(filepath.Clean(path), absolute) {
		return "", "", nil, fmt.Errorf("path is not canonical")
	}
	volume := filepath.VolumeName(absolute)
	if volume == "" {
		return "", "", nil, fmt.Errorf("path has no volume")
	}
	root := volume + string(filepath.Separator)
	relative, err := filepath.Rel(root, absolute)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", nil, fmt.Errorf("path is not beneath its volume root")
	}
	components := strings.Split(relative, string(filepath.Separator))
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			return "", "", nil, fmt.Errorf("path is unsafe")
		}
	}
	return filepath.Clean(absolute), root, components, nil
}

func verifyWindowsAncestors(root string, components []string) error {
	current := root
	paths := append([]string{root}, components...)
	for index, component := range paths {
		if index > 0 {
			current = filepath.Join(current, component)
		}
		name, err := windows.UTF16PtrFromString(current)
		if err != nil {
			return err
		}
		handle, err := windows.CreateFile(name, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if err != nil {
			return err
		}
		var information windows.ByHandleFileInformation
		infoErr := windows.GetFileInformationByHandle(handle, &information)
		_ = windows.CloseHandle(handle)
		if infoErr != nil || !safeWindowsFileAttributes(information.FileAttributes) || information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			return fmt.Errorf("repository ancestor is not a safe directory")
		}
	}
	return nil
}

func finalPathByHandle(handle windows.Handle) (string, error) {
	buffer := make([]uint16, 32768)
	length, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
	if err != nil || length == 0 || int(length) >= len(buffer) {
		return "", fmt.Errorf("resolve opened file handle")
	}
	value := windows.UTF16ToString(buffer[:length])
	if strings.HasPrefix(value, `\\?\UNC\`) {
		value = `\\` + strings.TrimPrefix(value, `\\?\UNC\`)
	} else {
		value = strings.TrimPrefix(value, `\\?\`)
	}
	return value, nil
}
