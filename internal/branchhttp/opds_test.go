package branchhttp

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofriendly/mayberry/internal/config"
)

func TestBranchOPDS(t *testing.T) {
	dir := t.TempDir()
	// One real book file to stream, plus a catalog big enough to paginate.
	epubPath := filepath.Join(dir, "dune.epub")
	if err := os.WriteFile(epubPath, []byte("PK\x03\x04 pretend epub"), 0644); err != nil {
		t.Fatal(err)
	}

	s := NewServer("branch-1", dir)
	s.SetConfig(&config.BranchConfig{DisplayName: "Merry Vale", UserID: "123456789"})
	s.mu.Lock()
	s.holdings = map[string]string{"9780441172719": epubPath}
	s.catalog = []CatalogEntry{
		{ID: "9780441172719", ISBN: "9780441172719", Title: "Dune", Author: "Frank Herbert", Path: epubPath, HasCover: true},
		{ID: "MBaaaaaaaaaaaa", Title: "Zephyr Tales", Author: "A. Writer", Path: filepath.Join(dir, "z.epub")},
	}
	for i := 0; i < 60; i++ { // force a second page
		s.catalog = append(s.catalog, CatalogEntry{ID: fmt.Sprintf("MB%012d", i), Title: fmt.Sprintf("Filler %02d", i), Path: "x.epub"})
	}
	s.mu.Unlock()

	// Helper: request with optional tunnel header + basic auth.
	call := func(path string, tunnel bool, user string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		if tunnel {
			r.Header.Set("X-Mayberry-Via-Tunnel", "true")
		}
		if user != "" {
			r.SetBasicAuth(user, user)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}

	// Local request (no tunnel header) is exempt from auth.
	w := call("/opds", false, "")
	if w.Code != 200 {
		t.Fatalf("local /opds: %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "<title>Merry Vale</title>") {
		t.Fatal("feed title missing branch name")
	}
	if !strings.Contains(body, "Dune") || !strings.Contains(body, "/opds/download/9780441172719") {
		t.Fatal("feed missing book or acquisition link")
	}
	if !strings.Contains(body, "/covers/9780441172719") {
		t.Fatal("feed missing cover link for book with cover")
	}
	// 62 books, 50/page → page 0 has a next link.
	if !strings.Contains(body, `rel="next"`) {
		t.Fatal("expected pagination next link on page 0")
	}
	if strings.Contains(body, `rel="previous"`) {
		t.Fatal("page 0 should not have a previous link")
	}
	if p2 := call("/opds?page=1", false, "").Body.String(); !strings.Contains(p2, `rel="previous"`) {
		t.Fatal("page 1 missing previous link")
	}

	// Tunnel request without credentials is rejected; owner card passes.
	if w := call("/opds", true, ""); w.Code != 401 {
		t.Fatalf("tunnel /opds without auth: %d, want 401", w.Code)
	}
	if w := call("/opds", true, "123456789"); w.Code != 200 {
		t.Fatalf("tunnel /opds as owner: %d, want 200", w.Code)
	}

	// Search filters by title/author.
	sr := call("/opds/search?q=herbert", false, "").Body.String()
	if !strings.Contains(sr, "Dune") || strings.Contains(sr, "Zephyr Tales") {
		t.Fatal("search did not filter by author")
	}
	if call("/opds/search", false, "").Code != 400 {
		t.Fatal("empty search query should be 400")
	}

	// Download streams the actual file bytes.
	d := call("/opds/download/9780441172719", false, "")
	if d.Code != 200 || !strings.Contains(d.Body.String(), "pretend epub") {
		t.Fatalf("download failed: %d %q", d.Code, d.Body.String())
	}
	if d.Header().Get("Content-Type") != "application/epub+zip" {
		t.Fatalf("wrong content-type: %s", d.Header().Get("Content-Type"))
	}
	// Unknown book → 404; tunnel download without auth → 401.
	if call("/opds/download/9999999999999", false, "").Code != 404 {
		t.Fatal("unknown download should 404")
	}
	if call("/opds/download/9780441172719", true, "").Code != 401 {
		t.Fatal("tunnel download without auth should 401")
	}

	// OpenSearch descriptor is public.
	if os := call("/opds/opensearch.xml", true, ""); os.Code != 200 || !strings.Contains(os.Body.String(), "OpenSearchDescription") {
		t.Fatalf("opensearch: %d", os.Code)
	}
}

func TestBranchOPDSFolders(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"Fiction/SciFi", "Nonfiction"} {
		os.MkdirAll(filepath.Join(dir, d), 0755)
	}
	mk := func(rel string) string {
		p := filepath.Join(dir, rel)
		os.WriteFile(p, []byte("PK\x03\x04"), 0644)
		return p
	}
	s := NewServer("b", dir)
	s.SetConfig(&config.BranchConfig{DisplayName: "Vale", LibraryPath: dir})
	s.mu.Lock()
	s.catalog = []CatalogEntry{
		{ID: "root1", Title: "Root Book", Path: mk("root.epub")},
		{ID: "fic1", Title: "Fiction One", Path: mk("Fiction/f1.epub")},
		{ID: "sci1", Title: "SciFi Deep", Path: mk("Fiction/SciFi/s1.epub")},
		{ID: "non1", Title: "Nonfiction One", Path: mk("Nonfiction/n1.epub")},
	}
	s.mu.Unlock()

	get := func(q string) string {
		r := httptest.NewRequest("GET", "/opds"+q, nil)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("/opds%s: %d", q, w.Code)
		}
		return w.Body.String()
	}
	has := func(b, sub string) bool { return strings.Contains(b, sub) }

	root := get("")
	if !has(root, "📁 Fiction") || !has(root, "📁 Nonfiction") {
		t.Fatal("root missing subfolders")
	}
	if !has(root, "/opds/download/root1") { // root book present
		t.Fatal("root book missing")
	}
	if has(root, "/opds/download/fic1") || has(root, "/opds/download/sci1") {
		t.Fatal("root leaked nested books as acquisitions")
	}
	if !has(root, "2 books") { // Fiction has f1 + s1 beneath it
		t.Fatal("Fiction count wrong")
	}

	fic := get("?folder=Fiction")
	if !has(fic, "📁 SciFi") || !has(fic, "/opds/download/fic1") {
		t.Fatal("Fiction folder missing subfolder or its book")
	}
	if has(fic, "/opds/download/root1") || has(fic, "/opds/download/sci1") {
		t.Fatal("Fiction folder leaked non-direct books")
	}

	deep := get("?folder=Fiction/SciFi")
	if !has(deep, "/opds/download/sci1") || has(deep, "📁 ") {
		t.Fatalf("SciFi folder wrong")
	}

	if !has(get("?folder=../../etc"), "📁 Fiction") {
		t.Fatal("traversal did not fall back to root")
	}
}

// TestBranchOPDSV2 covers the branch-served catalog's OPDS 2.0 negotiation:
// an explicit Accept preference for opds+json gets the JSON feed with the
// spec's search template; everything else keeps getting Atom.
func TestBranchOPDSV2(t *testing.T) {
	dir := t.TempDir()
	s := NewServer("branch-1", dir)
	s.SetConfig(&config.BranchConfig{DisplayName: "Merry Vale", UserID: "123456789"})
	s.mu.Lock()
	s.catalog = []CatalogEntry{
		{ID: "9780441172719", ISBN: "9780441172719", Title: "Dune", Author: "Frank Herbert", Path: filepath.Join(dir, "dune.epub"), HasCover: true},
		{ID: "MBaaaaaaaaaaaa", Title: "Zephyr Tales", Author: "A. Writer", Path: filepath.Join(dir, "Fiction", "z.epub")},
	}
	s.mu.Unlock()

	get := func(path, accept string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		if accept != "" {
			r.Header.Set("Accept", accept)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}

	// 2.0 ask → JSON with publications, folder navigation, and search template.
	w := get("/opds", "application/opds+json")
	if w.Code != 200 {
		t.Fatalf("/opds v2: %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/opds+json" {
		t.Fatalf("content type: %s", ct)
	}
	if w.Header().Get("Vary") != "Accept" {
		t.Fatal("missing Vary: Accept")
	}
	body := w.Body.String()
	for _, want := range []string{
		`"publications"`, `"Dune"`, `"/opds/download/9780441172719"`,
		`"/opds/search{?query}"`, `"templated": true`,
		`"navigation"`, `"/opds?folder=Fiction"`, // folder subsection
		`"itemsPerPage": 50`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("v2 feed missing %s in: %s", want, body)
		}
	}

	// No stated preference → Atom, unchanged.
	w = get("/opds", "")
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/atom+xml") {
		t.Fatalf("default should stay Atom, got %s", w.Header().Get("Content-Type"))
	}

	// The 2.0 search template's query parameter works alongside q.
	for _, path := range []string{"/opds/search?query=dune", "/opds/search?q=dune"} {
		w = get(path, "application/opds+json")
		if w.Code != 200 || !strings.Contains(w.Body.String(), "Dune") {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
}
