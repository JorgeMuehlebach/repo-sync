//go:build windows

package gitops

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"github.com/JorgeMuehlebach/repo-sync/internal/securefile"
	"golang.org/x/sys/windows"
)

type systemMirrorPointerBackend struct{}

type windowsFileRenameInfo struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

func (systemMirrorPointerBackend) Resolve(exposed string) (string, error) {
	if err := requireCanonicalMirrorPointerPath(exposed); err != nil {
		return "", err
	}
	info, err := os.Lstat(exposed)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return "", fmt.Errorf("mirror pointer is not a junction or symbolic link")
	}
	target, err := os.Readlink(exposed)
	if err != nil {
		return "", err
	}
	target = normalizeWindowsJunctionTarget(target)
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(exposed), target)
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return "", err
	}
	target = filepath.Clean(target)
	targetInfo, err := os.Stat(target)
	if err != nil || !targetInfo.IsDir() {
		return "", fmt.Errorf("mirror pointer target is not a directory")
	}
	return target, nil
}

func (backend systemMirrorPointerBackend) Create(exposed, target string) error {
	if err := requireCanonicalMirrorPointerPath(exposed); err != nil {
		return err
	}
	if err := requireCanonicalMirrorGenerationPath(target); err != nil {
		return err
	}
	if err := createWindowsJunction(exposed, target); err != nil {
		return err
	}
	if err := syncMirrorDirectory(filepath.Dir(exposed)); err != nil {
		_ = os.Remove(exposed)
		return err
	}
	resolved, err := backend.Resolve(exposed)
	if err != nil || !sameMirrorPath(resolved, target) {
		_ = os.Remove(exposed)
		return fmt.Errorf("created mirror pointer has the wrong target")
	}
	return nil
}

func (backend systemMirrorPointerBackend) Replace(exposed, expected, next string) error {
	if err := requireCanonicalMirrorPointerPath(exposed); err != nil {
		return &mirrorAtomicPointerError{err: err}
	}
	if err := requireCanonicalMirrorGenerationPath(expected); err != nil {
		return &mirrorAtomicPointerError{err: err}
	}
	if err := requireCanonicalMirrorGenerationPath(next); err != nil {
		return &mirrorAtomicPointerError{err: err}
	}
	resolved, err := backend.Resolve(exposed)
	if err != nil || !sameMirrorPath(resolved, expected) {
		return &mirrorAtomicPointerError{err: fmt.Errorf("mirror pointer changed before replacement")}
	}
	before, err := os.Lstat(exposed)
	if err != nil || before.Mode()&os.ModeSymlink == 0 {
		return &mirrorAtomicPointerError{err: fmt.Errorf("mirror pointer identity is invalid")}
	}
	id, err := newMirrorTransactionID()
	if err != nil {
		return &mirrorAtomicPointerError{err: err}
	}
	temporary := filepath.Join(filepath.Dir(exposed), ".repo-sync-pointer-"+id)
	if err := createWindowsJunction(temporary, next); err != nil {
		return &mirrorAtomicPointerError{err: err}
	}
	defer os.Remove(temporary)
	after, err := os.Lstat(exposed)
	if err != nil || !os.SameFile(before, after) {
		return &mirrorAtomicPointerError{err: fmt.Errorf("mirror pointer changed during replacement")}
	}
	resolved, err = backend.Resolve(exposed)
	if err != nil || !sameMirrorPath(resolved, expected) {
		return &mirrorAtomicPointerError{err: fmt.Errorf("mirror pointer target changed during replacement")}
	}
	if err := replaceWindowsPointer(temporary, exposed); err != nil {
		return &mirrorAtomicPointerError{err: err}
	}
	if err := syncMirrorDirectory(filepath.Dir(exposed)); err != nil {
		return &mirrorAtomicPointerError{err: err}
	}
	resolved, err = backend.Resolve(exposed)
	if err != nil || !sameMirrorPath(resolved, next) {
		return &mirrorAtomicPointerError{err: fmt.Errorf("atomic replacement did not expose the next generation")}
	}
	return nil
}

// replaceWindowsPointer intentionally uses FileRenameInfoEx rather than
// MoveFileEx. Replacing a directory reparse point is accepted only when the
// filesystem supports POSIX replacement semantics as one atomic operation.
func replaceWindowsPointer(source, destination string) error {
	sourceName, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(
		sourceName,
		windows.DELETE|windows.SYNCHRONIZE|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)

	destinationName, err := windows.UTF16FromString(destination)
	if err != nil {
		return err
	}
	destinationName = destinationName[:len(destinationName)-1]
	var template windowsFileRenameInfo
	nameOffset := int(unsafe.Offsetof(template.FileName))
	buffer := make([]byte, nameOffset+len(destinationName)*2)
	information := (*windowsFileRenameInfo)(unsafe.Pointer(&buffer[0]))
	information.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	information.FileNameLength = uint32(len(destinationName) * 2)
	name := unsafe.Slice(&information.FileName[0], len(destinationName))
	copy(name, destinationName)
	if err := windows.SetFileInformationByHandle(handle, windows.FileRenameInfoEx, &buffer[0], uint32(len(buffer))); err != nil {
		return fmt.Errorf("FileRenameInfoEx POSIX replacement is unavailable: %w", err)
	}
	return windows.FlushFileBuffers(handle)
}

func createWindowsJunction(link, target string) error {
	if err := os.Mkdir(link, 0o700); err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(link)
		}
	}()
	linkName, err := windows.UTF16PtrFromString(link)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(
		linkName,
		windows.GENERIC_WRITE,
		0,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)

	substitute := `\??\` + target
	if strings.HasPrefix(target, `\\`) {
		substitute = `\??\UNC\` + strings.TrimPrefix(target, `\\`)
	}
	substituteUTF16, err := windows.UTF16FromString(substitute)
	if err != nil {
		return err
	}
	printUTF16, err := windows.UTF16FromString(target)
	if err != nil {
		return err
	}
	pathBuffer := append(substituteUTF16, printUTF16...)
	pathBytes := len(pathBuffer) * 2
	if pathBytes+8 > int(^uint16(0)) {
		return fmt.Errorf("junction target is too long")
	}
	buffer := make([]byte, 16+pathBytes)
	binary.LittleEndian.PutUint32(buffer[0:4], windows.IO_REPARSE_TAG_MOUNT_POINT)
	binary.LittleEndian.PutUint16(buffer[4:6], uint16(8+pathBytes))
	binary.LittleEndian.PutUint16(buffer[8:10], 0)
	binary.LittleEndian.PutUint16(buffer[10:12], uint16((len(substituteUTF16)-1)*2))
	binary.LittleEndian.PutUint16(buffer[12:14], uint16(len(substituteUTF16)*2))
	binary.LittleEndian.PutUint16(buffer[14:16], uint16((len(printUTF16)-1)*2))
	for index, value := range pathBuffer {
		binary.LittleEndian.PutUint16(buffer[16+index*2:], value)
	}
	var returned uint32
	if err := windows.DeviceIoControl(handle, windows.FSCTL_SET_REPARSE_POINT, &buffer[0], uint32(len(buffer)), nil, 0, &returned, nil); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func normalizeWindowsJunctionTarget(target string) string {
	if strings.HasPrefix(target, `\\?\UNC\`) {
		return `\\` + strings.TrimPrefix(target, `\\?\UNC\`)
	}
	if strings.HasPrefix(target, `\??\UNC\`) {
		return `\\` + strings.TrimPrefix(target, `\??\UNC\`)
	}
	target = strings.TrimPrefix(target, `\\?\`)
	return strings.TrimPrefix(target, `\??\`)
}

func requireCanonicalMirrorPointerPath(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil || !strings.EqualFold(filepath.Clean(path), absolute) || filepath.Base(path) == "." {
		return fmt.Errorf("mirror pointer path is not absolute and canonical")
	}
	if _, err := securefile.VerifyDirectoryNoReparse(filepath.Dir(path)); err != nil {
		return fmt.Errorf("mirror pointer parent is not a directory")
	}
	return nil
}

func requireCanonicalMirrorGenerationPath(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil || !strings.EqualFold(filepath.Clean(path), absolute) {
		return fmt.Errorf("mirror generation path is not absolute and canonical")
	}
	if _, err := securefile.VerifyDirectoryNoReparse(path); err != nil {
		return fmt.Errorf("mirror generation is not a directory")
	}
	return nil
}

func syncMirrorDirectory(path string) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	return windows.FlushFileBuffers(handle)
}

func mirrorPathCaseInsensitive() bool { return true }

func normalizeMirrorPathForComparison(path string) string {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return filepath.Clean(path)
	}
	buffer := make([]uint16, 32768)
	length, err := windows.GetLongPathName(name, &buffer[0], uint32(len(buffer)))
	if err != nil || length == 0 || int(length) >= len(buffer) {
		return filepath.Clean(path)
	}
	return filepath.Clean(normalizeWindowsJunctionTarget(windows.UTF16ToString(buffer[:length])))
}
