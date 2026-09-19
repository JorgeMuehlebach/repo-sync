//go:build windows

package securefile

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsFileBasicInfo struct {
	CreationTime   int64
	LastAccessTime int64
	LastWriteTime  int64
	ChangeTime     int64
	FileAttributes uint32
	_              uint32
}

// SyncRegularNoFollow opens the final component as a reparse point and requests
// write access because FlushFileBuffers rejects a read-only Windows handle.
// The function never writes file content.
func SyncRegularNoFollow(path string) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attributeHandle, err := windows.CreateFile(
		name,
		windows.FILE_READ_ATTRIBUTES|windows.FILE_WRITE_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(attributeHandle)
	var expected windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(attributeHandle, &expected); err != nil || !safeWindowsFileAttributes(expected.FileAttributes) || expected.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return fmt.Errorf("path is not a regular file")
	}
	var original windowsFileBasicInfo
	if err := windows.GetFileInformationByHandleEx(attributeHandle, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&original)), uint32(unsafe.Sizeof(original))); err != nil {
		return err
	}
	changedReadOnly := original.FileAttributes&windows.FILE_ATTRIBUTE_READONLY != 0
	if changedReadOnly {
		writable := original
		writable.FileAttributes &^= windows.FILE_ATTRIBUTE_READONLY
		if err := windows.SetFileInformationByHandle(attributeHandle, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&writable)), uint32(unsafe.Sizeof(writable))); err != nil {
			return err
		}
	}

	writeHandle, writeErr := windows.CreateFile(
		name,
		windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if writeErr == nil {
		var actual windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(writeHandle, &actual); err != nil || !sameWindowsFileIdentity(expected, actual) || !safeWindowsFileAttributes(actual.FileAttributes) || actual.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
			writeErr = fmt.Errorf("file changed identity before durability flush")
		} else {
			writeErr = windows.FlushFileBuffers(writeHandle)
		}
		if closeErr := windows.CloseHandle(writeHandle); writeErr == nil {
			writeErr = closeErr
		}
	}
	if changedReadOnly {
		if restoreErr := windows.SetFileInformationByHandle(attributeHandle, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&original)), uint32(unsafe.Sizeof(original))); restoreErr != nil {
			return fmt.Errorf("restore read-only file metadata: %w", restoreErr)
		}
	}
	return writeErr
}

func sameWindowsFileIdentity(left, right windows.ByHandleFileInformation) bool {
	return left.VolumeSerialNumber == right.VolumeSerialNumber && left.FileIndexHigh == right.FileIndexHigh && left.FileIndexLow == right.FileIndexLow
}
