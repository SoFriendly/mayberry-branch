package storage

import (
	"os"
	"path/filepath"
	"strings"
)

// ScanDirectory walks the given path and returns all .epub file paths.
func ScanDirectory(dir string) ([]string, error) {
	var epubs []string
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.EqualFold(filepath.Ext(p), ".epub") {
			epubs = append(epubs, p)
		}
		return nil
	})
	return epubs, err
}
