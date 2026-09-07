package branchhttp

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sofriendly/mayberry/internal/auth"
)

func TestDownloadBranchBinding(t *testing.T) {
	keys, err := auth.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	const isbn = "9780123456789"
	file := filepath.Join(t.TempDir(), "book.epub")
	if err := os.WriteFile(file, []byte("book contents"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &Server{branchID: "owner", publicKey: keys.Public, holdings: map[string]string{isbn: file}}
	for _, tc := range []struct {
		name, branch, book string
		status             int
	}{
		{"owner", "owner", isbn, 200}, {"other branch", "other", isbn, 403},
		{"missing branch", "", isbn, 403}, {"other book", "owner", "9780987654321", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token, err := auth.IssueDownloadToken(keys, tc.branch, tc.book)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest("GET", "/download/"+isbn+"?token="+token, nil)
			w := httptest.NewRecorder()
			s.handleDownload(w, r)
			if w.Code != tc.status {
				t.Fatalf("got %d want %d", w.Code, tc.status)
			}
			if tc.status != 200 && w.Body.String() == "book contents" {
				t.Fatal("unauthorized file content")
			}
		})
	}
	s.SetBranchID("recovered")
	token, _ := auth.IssueDownloadToken(keys, "recovered", isbn)
	r := httptest.NewRequest("GET", "/download/"+isbn+"?token="+token, nil)
	r.Header.Set("X-Mayberry-Mirror", "1")
	s.mirrorServeSlots = make(chan struct{}, 1)
	w := httptest.NewRecorder()
	s.handleDownload(w, r)
	if w.Code != 200 || w.Body.String() != "book contents" {
		t.Fatal("recovered branch mirror download failed", w.Code)
	}
}
