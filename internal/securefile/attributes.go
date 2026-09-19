package securefile

const windowsFileAttributeReparsePoint = 0x00000400

func safeWindowsFileAttributes(attributes uint32) bool {
	return attributes&windowsFileAttributeReparsePoint == 0
}
