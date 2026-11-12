package branchhttp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sofriendly/mayberry/internal/config"
)

func TestRelFolder(t *testing.T) {
	lib := "/books"
	audio := "/audio"
	s := NewServer("b", lib)
	s.SetConfig(&config.BranchConfig{LibraryPath: lib, AudiobookPath: audio})

	cases := []struct {
		path        string
		isAudiobook bool
		want        string
	}{
		{"/books/Dune.epub", false, ""},                          // root
		{"/books/Fiction/Dune.epub", false, "Fiction"},           // one level
		{"/books/Fiction/SciFi/Dune.epub", false, "Fiction/SciFi"}, // nested
		{"/audio/Series/Book.m4b", true, "Series"},               // audiobook root
		{filepath.Join(lib, "_mirror", "ab", "cd", "x.epub"), false, ""}, // mirror → root
	}
	for _, c := range cases {
		if got := s.relFolder(c.path, c.isAudiobook); got != c.want {
			t.Errorf("relFolder(%q, audiobook=%v) = %q, want %q", c.path, c.isAudiobook, got, c.want)
		}
	}

	// No library root configured → empty, never a panic.
	empty := NewServer("b", "")
	empty.SetConfig(&config.BranchConfig{})
	if got := empty.relFolder("/somewhere/x.epub", false); got != "" {
		t.Errorf("unconfigured relFolder = %q, want empty", got)
	}
}

// A file cached before the folder field existed (Book.Folder == "") must
// still report its current folder: relFolder is recomputed on cache hit.
func TestScanCacheRecomputesFolder(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "Fiction")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(sub, "book.epub")
	if err := os.WriteFile(f, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(f)

	s := NewServer("b", dir)
	s.SetConfig(&config.BranchConfig{LibraryPath: dir})
	// Simulate a stale cache entry (pre-folder): Folder empty.
	s.scans.Put(f, info.Size(), info.ModTime(), scanFileResult{
		ok: true, hasTitle: true,
		entry: CatalogEntry{Path: f, ID: "9780000000009", Title: "T"},
		book:  BookMeta{ISBN: "9780000000009", Title: "T"},
	})
	res := s.processScanFile(f)
	if res.book.Folder != "Fiction" {
		t.Fatalf("cache hit did not recompute folder: got %q", res.book.Folder)
	}
}
