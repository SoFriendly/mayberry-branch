package branchhttp

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sofriendly/mayberry/internal/opds"
)

// Self-contained OPDS for a branch. A reader points their catalog app at
// {name}.branch.pub/opds, signs in once with a library card (the same
// tunnelAuth the dashboard uses — owner exempt on the local network), and
// browses and downloads only this branch's books. Everything is served from
// the branch's own in-memory catalog and files; no Town Square round-trip
// and no download token — the acquisition links stream straight back through
// the tunnel under the same Basic-auth session.
//
// Metadata is what the branch has locally (title, author, cover); richer
// fields (descriptions, genres) live in the mayberry.pub catalog.

const opdsPageSize = 50

// handleOPDS emits folder navigation mirroring the on-disk library layout,
// matching the mayberry.pub branch feed: the immediate subfolders of the
// current folder (via ?folder=<path>) as subsection entries, then the books
// filed directly in it. A flat library (everything in the root) shows just
// the book list. Folder for each book is derived from its path (relFolder),
// so no extra stored state is needed.
func (s *Server) handleOPDS(w http.ResponseWriter, r *http.Request) {
	// Compat shim: OpenSearch-era readers that can't expand the OPDS 2.0
	// search template (/opds/search{?query}) fetch the link target
	// literally — it lands here via the /opds/ prefix route — expecting an
	// OpenSearch description document. Serve the description so their
	// search still works.
	if strings.HasPrefix(r.URL.Path, "/opds/search{") {
		s.handleOpenSearch(w, r)
		return
	}
	folder := cleanBranchFolder(r.URL.Query().Get("folder"))
	page := opdsParsePage(r)

	s.mu.RLock()
	branchName := ""
	if s.cfg != nil {
		branchName = s.cfg.DisplayName
	}
	// Immediate subfolder book-counts, and the books filed directly here.
	childCounts := map[string]int{}
	var books []CatalogEntry
	for _, e := range s.catalog {
		g := s.relFolder(e.Path, strings.EqualFold(filepath.Ext(e.Path), ".m4b"))
		switch {
		case g == folder:
			books = append(books, e)
		case folder == "" && g != "":
			childCounts[firstSegment(g)]++
		case folder != "" && strings.HasPrefix(g, folder+"/"):
			childCounts[firstSegment(strings.TrimPrefix(g, folder+"/"))]++
		}
	}
	s.mu.RUnlock()
	if branchName == "" {
		branchName = "My Library"
	}

	subs := make([]string, 0, len(childCounts))
	for name := range childCounts {
		subs = append(subs, name)
	}
	sort.Strings(subs)
	sort.Slice(books, func(i, j int) bool {
		ti, tj := strings.ToLower(books[i].Title), strings.ToLower(books[j].Title)
		if ti == tj {
			return books[i].ID < books[j].ID
		}
		return ti < tj
	})

	basePath := "/opds"
	feedID := "urn:mayberry:branch:opds"
	title := branchName
	if folder != "" {
		basePath = "/opds?folder=" + urlQueryEscape(folder)
		feedID += ":folder:" + folder
		title = branchName + " / " + folder
	}

	// Subfolders lead the first page; later pages continue this folder's books.
	start := page * opdsPageSize
	if start > len(books) {
		start = len(books)
	}
	end := start + opdsPageSize
	if end > len(books) {
		end = len(books)
	}
	hasNext := end < len(books)

	var entries []opds.Entry
	if page == 0 {
		for _, name := range subs {
			child := name
			if folder != "" {
				child = folder + "/" + name
			}
			entries = append(entries, opds.Entry{
				ID:      feedID + ":" + child,
				Title:   "📁 " + name,
				Summary: fmt.Sprintf("%d book%s", childCounts[name], plural(childCounts[name])),
				NavHref: "/opds?folder=" + urlQueryEscape(child),
			})
		}
	}
	for _, e := range books[start:end] {
		entries = append(entries, catalogEntryToOPDS(e))
	}
	s.writeOPDS(w, r, basePath, feedID, title, entries, page, hasNext)
}

// cleanBranchFolder normalizes a ?folder= value: forward-slashed, no
// traversal. Anything suspicious collapses to "" (root).
func cleanBranchFolder(raw string) string {
	f := strings.Trim(strings.TrimSpace(raw), "/")
	if f == "" {
		return ""
	}
	for _, p := range strings.Split(f, "/") {
		if strings.TrimSpace(p) == "" || p == "." || p == ".." {
			return ""
		}
	}
	return f
}

// firstSegment returns the leading path component of a forward-slashed path.
func firstSegment(p string) string {
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return p
}

func opdsParsePage(r *http.Request) int {
	page := 0
	if p := r.URL.Query().Get("page"); p != "" {
		fmt.Sscanf(p, "%d", &page)
	}
	if page < 0 {
		page = 0
	}
	return page
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// handleOPDSSearch filters the catalog by a case-insensitive substring of
// title or author (OpenSearch q parameter, or the OPDS 2.0 search
// template's query parameter).
func (s *Server) handleOPDSSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		q = strings.TrimSpace(r.URL.Query().Get("query"))
	}
	if q == "" {
		http.Error(w, "Missing search query", http.StatusBadRequest)
		return
	}
	entries, page, hasNext, title := s.opdsPage(r, q)
	base := "/opds/search?q=" + urlQueryEscape(q)
	s.writeOPDS(w, r, base, "urn:mayberry:branch:opds:search", "Search: "+title, entries, page, hasNext)
}

// handleOpenSearch advertises the branch's search endpoint.
func (s *Server) handleOpenSearch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/opensearchdescription+xml")
	w.Write(opds.OpenSearchDescription(""))
}

// opdsPage snapshots the catalog (optionally filtered by query), sorts it by
// title, and returns the requested page of OPDS entries plus whether another
// page follows and the branch's display name.
func (s *Server) opdsPage(r *http.Request, query string) (entries []opds.Entry, page int, hasNext bool, branchName string) {
	page = 0
	if p := r.URL.Query().Get("page"); p != "" {
		fmt.Sscanf(p, "%d", &page)
	}
	if page < 0 {
		page = 0
	}

	s.mu.RLock()
	cat := make([]CatalogEntry, 0, len(s.catalog))
	q := strings.ToLower(query)
	for _, e := range s.catalog {
		if q == "" || strings.Contains(strings.ToLower(e.Title), q) || strings.Contains(strings.ToLower(e.Author), q) {
			cat = append(cat, e)
		}
	}
	if s.cfg != nil {
		branchName = s.cfg.DisplayName
	}
	s.mu.RUnlock()
	if branchName == "" {
		branchName = "My Library"
	}

	sort.Slice(cat, func(i, j int) bool {
		ti, tj := strings.ToLower(cat[i].Title), strings.ToLower(cat[j].Title)
		if ti == tj {
			return cat[i].ID < cat[j].ID
		}
		return ti < tj
	})

	start := page * opdsPageSize
	if start > len(cat) {
		start = len(cat)
	}
	end := start + opdsPageSize
	if end > len(cat) {
		end = len(cat)
	}
	hasNext = end < len(cat)
	for _, e := range cat[start:end] {
		entries = append(entries, catalogEntryToOPDS(e))
	}
	return entries, page, hasNext, branchName
}

// catalogEntryToOPDS maps a local catalog row to an OPDS entry with links
// back to the branch's own acquisition and cover endpoints.
func catalogEntryToOPDS(e CatalogEntry) opds.Entry {
	acqType := "application/epub+zip"
	mediaType := "ebook"
	if strings.EqualFold(filepath.Ext(e.Path), ".m4b") {
		acqType = "audio/mp4"
		mediaType = "audiobook"
	}
	entry := opds.Entry{
		ID:        e.ID,
		ISBN:      e.ISBN,
		Title:     e.Title,
		Author:    e.Author,
		AcqHref:   "/opds/download/" + e.ID,
		AcqType:   acqType,
		MediaType: mediaType,
	}
	if e.HasCover {
		entry.CoverHref = "/covers/" + e.ID
	}
	return entry
}

// writeOPDS negotiates OPDS 1.2 Atom vs 2.0 JSON (same rules as Town
// Square: 2.0 only on an explicit Accept preference) and renders an
// acquisition feed with first/previous/next paging.
func (s *Server) writeOPDS(w http.ResponseWriter, r *http.Request, basePath, id, title string, entries []opds.Entry, page int, hasNext bool) {
	var nav []opds.NavLink
	if page > 0 {
		nav = append(nav,
			opds.NavLink{Rel: "first", Href: opdsPageLink(basePath, 0)},
			opds.NavLink{Rel: "previous", Href: opdsPageLink(basePath, page-1)})
	}
	if hasNext {
		nav = append(nav, opds.NavLink{Rel: "next", Href: opdsPageLink(basePath, page+1)})
	}
	w.Header().Set("Vary", "Accept")
	if opds.WantsV2(r) {
		data, err := opds.FeedV2(opds.FeedV2Options{
			Self:       r.URL.RequestURI(),
			Title:      title,
			SearchHref: opds.SearchTemplate,
			Page:       page + 1,
			PageSize:   opdsPageSize,
			Entries:    entries,
			Nav:        nav,
		})
		if err != nil {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/opds+json")
		w.Write(data)
		return
	}
	data, err := opds.FeedWithNav(id, title, entries, nav)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/atom+xml;profile=opds-catalog")
	w.Write(data)
}

func opdsPageLink(basePath string, page int) string {
	sep := "?"
	if strings.Contains(basePath, "?") {
		sep = "&"
	}
	if page <= 0 {
		return basePath
	}
	return fmt.Sprintf("%s%spage=%d", basePath, sep, page)
}

// handleOPDSDownload streams a book from the branch's own files, identified
// by book ID. Authentication is the tunnelAuth Basic-auth session (the
// wrapper runs before this handler), so no download token is needed — this
// is the reader-facing counterpart to the token-gated /download/ path.
func (s *Server) handleOPDSDownload(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/opds/download/")
	id = strings.TrimSuffix(id, "/")
	if id == "" || strings.Contains(id, "/") || strings.Contains(id, "..") {
		http.NotFound(w, r)
		return
	}

	s.mu.RLock()
	filePath, ok := s.holdings[id]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "Book not found on this branch", http.StatusNotFound)
		return
	}

	// Count as a real download so mirror serves defer to it.
	s.realDownloads.Add(1)
	defer s.realDownloads.Add(-1)

	f, err := os.Open(filePath)
	if err != nil {
		http.Error(w, "File not available", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "File not available", http.StatusInternalServerError)
		return
	}

	ext := strings.ToLower(filepath.Ext(filePath))
	contentType := "application/epub+zip"
	if ext == ".m4b" {
		contentType = "audio/mp4"
	} else {
		ext = ".epub"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s%s"`, id, ext))
	http.ServeContent(w, r, id+ext, info.ModTime(), f)
}

// urlQueryEscape percent-escapes a query value for embedding in a feed link.
func urlQueryEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == ' ':
			b.WriteString("%20")
		case r == '&':
			b.WriteString("%26")
		case r == '=':
			b.WriteString("%3D")
		case r == '?':
			b.WriteString("%3F")
		case r == '#':
			b.WriteString("%23")
		case r == '+':
			b.WriteString("%2B")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
