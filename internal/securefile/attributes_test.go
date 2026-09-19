package securefile

import "testing"

func TestSafeWindowsFileAttributesRejectsEveryReparsePoint(t *testing.T) {
	if !safeWindowsFileAttributes(0x20) {
		t.Fatal("ordinary archive attributes were rejected")
	}
	if safeWindowsFileAttributes(windowsFileAttributeReparsePoint) || safeWindowsFileAttributes(windowsFileAttributeReparsePoint|0x20) {
		t.Fatal("reparse-point attributes were accepted")
	}
}
