//go:build windows

package processstate

import (
	"errors"
	"syscall"
	"unsafe"
)

const (
	processQueryLimitedInformation = 0x1000
	stillActive                    = 259
	errorInvalidParameter          = syscall.Errno(87)
)

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	getExitCodeProcessProc = kernel32.NewProc("GetExitCodeProcess")
)

// Alive reports whether pid still names a running process. Access denied is
// treated as alive so a lock is never evicted merely because it is owned by a
// more privileged process.
func Alive(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		if errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
			return true, nil
		}
		if errors.Is(err, errorInvalidParameter) {
			return false, nil
		}
		return false, err
	}
	defer syscall.CloseHandle(handle)
	var exitCode uint32
	result, _, callErr := getExitCodeProcessProc.Call(uintptr(handle), uintptr(unsafe.Pointer(&exitCode)))
	if result == 0 {
		return false, callErr
	}
	return exitCode == stillActive, nil
}
