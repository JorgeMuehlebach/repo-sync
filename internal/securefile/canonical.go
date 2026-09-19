package securefile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// OpenCanonicalRegular opens an absolute, lexically canonical path while
// refusing symlink/reparse traversal in every ancestor.
func OpenCanonicalRegular(path string) (*os.File, error) {
	absolute, err := filepath.Abs(path)
	if err != nil || filepath.Clean(path) != absolute {
		return nil, fmt.Errorf("path is not canonical")
	}
	volume := filepath.VolumeName(absolute)
	root := string(filepath.Separator)
	if volume != "" {
		root = volume + string(filepath.Separator)
	}
	relative, err := filepath.Rel(root, absolute)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("path is not beneath its volume root")
	}
	return OpenRegularBeneath(root, filepath.ToSlash(relative))
}
