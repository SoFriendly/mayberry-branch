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

// ScanDirectory walks the given path and returns all supported book/audiobook paths.
func ScanDirectory(dir string) ([]string, error) {
	var paths []string
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && IsSupportedFile(p) {
			paths = append(paths, p)
		}
		return nil
	})
	return paths, err
}
