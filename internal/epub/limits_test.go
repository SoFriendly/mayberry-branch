package epub

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMetadataAndBoundedReads(t *testing.T) {
	for _, n := range []int{31, 32, 33} {
		b, err := readBounded(strings.NewReader(strings.Repeat("x", n)), 32)
		if n <= 32 && (err != nil || len(b) != n) {
			t.Fatal("valid boundary rejected")
		}
		if n > 32 && err == nil {
			t.Fatal("oversized entry accepted")
		}
	}
	p := filepath.Join(t.TempDir(), "book.epub")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	z := zip.NewWriter(f)
	c, _ := z.Create("META-INF/container.xml")
	c.Write([]byte(`<container><rootfiles><rootfile full-path="book.opf"/></rootfiles></container>`))
	opf, _ := z.Create("book.opf")
	opf.Write([]byte(`<package><metadata><title>Title &amp; subtitle</title><creator>Author</creator><identifier>9780123456789</identifier></metadata></package>`))
	z.Close()
	f.Close()
	m, err := ExtractMetadata(p)
	if err != nil {
		t.Fatal(err)
	}
	if m.Title != "Title & subtitle" || m.ISBN != "9780123456789" {
		t.Fatal("normal metadata changed")
	}
}
