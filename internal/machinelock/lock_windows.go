//go:build windows

package machinelock

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// Acquire denies delete sharing while held so the stable lock path cannot be
// replaced with a second inode during an active operation.
func Acquire(path string, timeout time.Duration, purpose string) (*os.File, func(), error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s lock: %w", purpose, err)
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s lock: %w", purpose, err)
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil || information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		_ = windows.CloseHandle(handle)
		return nil, nil, fmt.Errorf("%s lock is not a regular file", purpose)
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, nil, fmt.Errorf("open %s lock", purpose)
	}
	var overlapped windows.Overlapped
	deadline := time.Now().Add(timeout)
	for {
		err = windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
		if err == nil {
			return file, func() {
				_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped)
				_ = file.Close()
			}, nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			_ = file.Close()
			return nil, nil, fmt.Errorf("lock %s: %w", purpose, err)
		}
		if timeout <= 0 || time.Now().After(deadline) {
			_ = file.Close()
			return nil, nil, fmt.Errorf("%s is busy; try again", purpose)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
