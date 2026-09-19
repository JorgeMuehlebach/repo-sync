//go:build windows

package notify

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func platformLookPath(name string) (string, error) {
	if !strings.EqualFold(name, "powershell") && !strings.EqualFold(name, "powershell.exe") {
		return "", fmt.Errorf("unsupported notification executable")
	}
	systemDirectory, err := windows.GetSystemDirectory()
	if err != nil {
		return "", err
	}
	executable := filepath.Join(systemDirectory, "WindowsPowerShell", "v1.0", "powershell.exe")
	info, err := os.Stat(executable)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("system PowerShell is unavailable")
	}
	return executable, nil
}

func platformRunCommand(ctx context.Context, executable string, args ...string) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(job)
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return err
	}
	executablePointer, err := windows.UTF16PtrFromString(executable)
	if err != nil {
		return err
	}
	commandLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{executable}, args...)))
	if err != nil {
		return err
	}
	startup := windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{}))}
	process := windows.ProcessInformation{}
	flags := uint32(windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW)
	if err := windows.CreateProcess(executablePointer, commandLine, nil, nil, false, flags, nil, nil, &startup, &process); err != nil {
		return err
	}
	defer windows.CloseHandle(process.Process)
	defer windows.CloseHandle(process.Thread)
	if err := windows.AssignProcessToJobObject(job, process.Process); err != nil {
		_ = windows.TerminateProcess(process.Process, 1)
		return err
	}
	if _, err := windows.ResumeThread(process.Thread); err != nil {
		_ = windows.TerminateJobObject(job, 1)
		return err
	}
	waitDone := make(chan error, 1)
	go func() {
		result, waitErr := windows.WaitForSingleObject(process.Process, windows.INFINITE)
		if waitErr == nil && result != windows.WAIT_OBJECT_0 {
			waitErr = fmt.Errorf("unexpected notification wait result %d", result)
		}
		waitDone <- waitErr
	}()
	select {
	case err = <-waitDone:
	case <-ctx.Done():
		_ = windows.TerminateJobObject(job, 1)
		select {
		case err = <-waitDone:
			if err == nil {
				err = ctx.Err()
			}
		case <-time.After(5 * time.Second):
			return ctx.Err()
		}
	}
	if err != nil {
		return err
	}
	var exitCode uint32
	if err := windows.GetExitCodeProcess(process.Process, &exitCode); err != nil {
		return err
	}
	if exitCode != 0 {
		return fmt.Errorf("notification process exited with status %d", exitCode)
	}
	return nil
}
