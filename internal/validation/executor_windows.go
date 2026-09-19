//go:build windows

package validation

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

func runIsolatedProcess(ctx context.Context, executable string, args, environment []string, stdout, stderr io.Writer) (int, error) {
	job, err := createKillOnCloseJob()
	if err != nil {
		return -1, err
	}
	defer func() {
		if job != 0 {
			_ = windows.CloseHandle(job)
		}
	}()

	stdoutRead, stdoutWrite, err := childOutputPipe()
	if err != nil {
		return -1, err
	}
	defer func() {
		if stdoutRead != 0 {
			_ = windows.CloseHandle(stdoutRead)
		}
		if stdoutWrite != 0 {
			_ = windows.CloseHandle(stdoutWrite)
		}
	}()
	stderrRead, stderrWrite, err := childOutputPipe()
	if err != nil {
		return -1, err
	}
	defer func() {
		if stderrRead != 0 {
			_ = windows.CloseHandle(stderrRead)
		}
		if stderrWrite != 0 {
			_ = windows.CloseHandle(stderrWrite)
		}
	}()
	stdinRead, stdinWrite, err := childInputPipe()
	if err != nil {
		return -1, err
	}
	defer func() {
		if stdinRead != 0 {
			_ = windows.CloseHandle(stdinRead)
		}
		if stdinWrite != 0 {
			_ = windows.CloseHandle(stdinWrite)
		}
	}()
	_ = windows.CloseHandle(stdinWrite)
	stdinWrite = 0

	executablePointer, err := windows.UTF16PtrFromString(executable)
	if err != nil {
		return -1, fmt.Errorf("encode validator executable path: %w", err)
	}
	commandLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{executable}, args...)))
	if err != nil {
		return -1, fmt.Errorf("encode validator command line: %w", err)
	}
	environmentBlock, err := windowsEnvironmentBlock(environment)
	if err != nil {
		return -1, err
	}
	startup := windows.StartupInfo{
		Cb:        uint32(unsafe.Sizeof(windows.StartupInfo{})),
		Flags:     windows.STARTF_USESTDHANDLES,
		StdInput:  stdinRead,
		StdOutput: stdoutWrite,
		StdErr:    stderrWrite,
	}
	process := windows.ProcessInformation{}
	creationFlags := uint32(windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_UNICODE_ENVIRONMENT)
	if err := windows.CreateProcess(executablePointer, commandLine, nil, nil, true, creationFlags, &environmentBlock[0], nil, &startup, &process); err != nil {
		return -1, fmt.Errorf("start validator process: %w", err)
	}
	defer windows.CloseHandle(process.Process)
	defer windows.CloseHandle(process.Thread)
	if err := windows.AssignProcessToJobObject(job, process.Process); err != nil {
		_ = windows.TerminateProcess(process.Process, 1)
		return -1, fmt.Errorf("assign suspended validator to job object: %w", err)
	}
	if _, err := windows.ResumeThread(process.Thread); err != nil {
		_ = windows.TerminateJobObject(job, 1)
		return -1, fmt.Errorf("resume validator process: %w", err)
	}
	_ = windows.CloseHandle(stdinRead)
	stdinRead = 0
	_ = windows.CloseHandle(stdoutWrite)
	stdoutWrite = 0
	_ = windows.CloseHandle(stderrWrite)
	stderrWrite = 0

	stdoutFile := os.NewFile(uintptr(stdoutRead), "contextctl-stdout")
	stderrFile := os.NewFile(uintptr(stderrRead), "contextctl-stderr")
	if stdoutFile == nil || stderrFile == nil {
		_ = windows.TerminateJobObject(job, 1)
		return -1, fmt.Errorf("open validator output pipes")
	}
	stdoutRead, stderrRead = 0, 0
	copyDone := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(stdout, stdoutFile); _ = stdoutFile.Close(); copyDone <- struct{}{} }()
	go func() { _, _ = io.Copy(stderr, stderrFile); _ = stderrFile.Close(); copyDone <- struct{}{} }()
	waitDone := make(chan error, 1)
	go func() {
		result, waitErr := windows.WaitForSingleObject(process.Process, windows.INFINITE)
		if waitErr == nil && result != windows.WAIT_OBJECT_0 {
			waitErr = fmt.Errorf("unexpected validator wait result %d", result)
		}
		waitDone <- waitErr
	}()
	var waitErr error
	select {
	case waitErr = <-waitDone:
	case <-ctx.Done():
		_ = windows.TerminateJobObject(job, 1)
		select {
		case waitErr = <-waitDone:
			if waitErr == nil {
				waitErr = ctx.Err()
			}
		case <-time.After(5 * time.Second):
			return -1, ctx.Err()
		}
	}
	// Closing the kill-on-close job here terminates any descendant that outlived
	// the validator parent and releases inherited pipe handles.
	_ = windows.CloseHandle(job)
	job = 0
	<-copyDone
	<-copyDone
	if waitErr != nil {
		return -1, waitErr
	}
	var exitCode uint32
	if err := windows.GetExitCodeProcess(process.Process, &exitCode); err != nil {
		return -1, fmt.Errorf("read validator exit code: %w", err)
	}
	if exitCode == 0 {
		return 0, nil
	}
	return int(exitCode), fmt.Errorf("validator exited with status %d", exitCode)
}

func createKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("create validator job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return 0, fmt.Errorf("configure validator job object: %w", err)
	}
	return job, nil
}

func childOutputPipe() (windows.Handle, windows.Handle, error) {
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), InheritHandle: 1}
	var read, write windows.Handle
	if err := windows.CreatePipe(&read, &write, &attributes, 0); err != nil {
		return 0, 0, err
	}
	if err := windows.SetHandleInformation(read, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		windows.CloseHandle(read)
		windows.CloseHandle(write)
		return 0, 0, err
	}
	return read, write, nil
}

func childInputPipe() (windows.Handle, windows.Handle, error) {
	read, write, err := childOutputPipe()
	if err != nil {
		return 0, 0, err
	}
	if err := windows.SetHandleInformation(read, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
		windows.CloseHandle(read)
		windows.CloseHandle(write)
		return 0, 0, err
	}
	if err := windows.SetHandleInformation(write, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		windows.CloseHandle(read)
		windows.CloseHandle(write)
		return 0, 0, err
	}
	return read, write, nil
}

func windowsEnvironmentBlock(environment []string) ([]uint16, error) {
	values := make(map[string]string, len(environment))
	for _, value := range environment {
		name, _, ok := strings.Cut(value, "=")
		if !ok || strings.ContainsRune(value, '\x00') {
			return nil, fmt.Errorf("invalid validator environment")
		}
		values[strings.ToUpper(name)] = value
	}
	environment = environment[:0]
	for _, value := range values {
		environment = append(environment, value)
	}
	sort.Slice(environment, func(i, j int) bool { return strings.ToUpper(environment[i]) < strings.ToUpper(environment[j]) })
	encoded := utf16.Encode([]rune(strings.Join(environment, "\x00") + "\x00\x00"))
	return encoded, nil
}
