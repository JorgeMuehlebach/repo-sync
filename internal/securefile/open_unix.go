//go:build !windows

package securefile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// OpenRegularNoFollow opens the final path component without following a
// symbolic link and rejects all non-regular files.
func OpenRegularNoFollow(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open regular file")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("path is not a regular file")
	}
	return file, nil
}

func VerifyDirectoryNoReparse(path string) (os.FileInfo, error) {
	absolute, err := filepath.Abs(path)
	if err != nil || filepath.Clean(path) != absolute || absolute == string(filepath.Separator) {
		return nil, fmt.Errorf("path is not a canonical directory")
	}
	components := strings.Split(strings.TrimPrefix(absolute, string(filepath.Separator)), string(filepath.Separator))
	fd, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	for _, component := range components {
		next, openErr := unix.Openat(fd, component, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
		_ = unix.Close(fd)
		if openErr != nil {
			return nil, openErr
		}
		fd = next
	}
	file := os.NewFile(uintptr(fd), absolute)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open safe directory")
	}
	info, err := file.Stat()
	_ = file.Close()
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("path is not a safe directory")
	}
	return info, nil
}

// OpenRegularBeneath walks each path component through held directory file
// descriptors. No component can be replaced by a symlink between inspection
// and the final open.
func OpenRegularBeneath(root, relative string) (*os.File, error) {
	components, err := beneathComponents(relative)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	for _, component := range components[:len(components)-1] {
		next, openErr := unix.Openat(fd, component, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
		_ = unix.Close(fd)
		if openErr != nil {
			return nil, openErr
		}
		fd = next
	}
	finalFD, err := unix.Openat(fd, components[len(components)-1], unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	_ = unix.Close(fd)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(finalFD), filepath.Join(root, filepath.FromSlash(relative)))
	if file == nil {
		_ = unix.Close(finalFD)
		return nil, fmt.Errorf("open regular file beneath root")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("path beneath root is not a regular file")
	}
	return file, nil
}

func beneathComponents(relative string) ([]string, error) {
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
