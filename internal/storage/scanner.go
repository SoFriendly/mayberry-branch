package storage

import (
	"os"
	"path/filepath"
	"strings"
)

// IsSupportedFile returns true if the path looks like a file we handle
// (EPUB ebook or M4B audiobook). Case-insensitive on extension.
func IsSupportedFile(p string) bool {
	ext := strings.ToLower(filepath.Ext(p))
	return ext == ".epub" || ext == ".m4b"
}

// IsSkippedDir returns true if a directory should be excluded from
// scanning. Tools like Calibre move "deleted" books to a `.caltrash`
// subdirectory inside the library rather than unlinking them — when
// our recursive walk re-discovered those files, the daemon kept
// claiming to hold (and serving!) books the user had clearly removed.
// macOS and Windows trash dirs land here too for the same reason.
func IsSkippedDir(name string) bool {
	switch name {
	case ".caltrash", ".Trash", ".Trashes", "$RECYCLE.BIN", "__MACOSX":
		return true
	}
	return false
}

// ScanDirectory walks the given path and returns all supported book/audiobook paths.
func ScanDirectory(dir string) ([]string, error) {
	var paths []string
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && IsSkippedDir(info.Name()) {
			return filepath.SkipDir
		}
		if !info.IsDir() && IsSupportedFile(p) {
			paths = append(paths, p)
		}
		return nil
	})
	return paths, err
}
