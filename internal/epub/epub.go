package epub

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
)

// Metadata holds the extracted metadata from an EPUB file.
type Metadata struct {
	Title         string
	Author        string
	ISBN          string   // ISBN-13 if found
	PublishedDate string   // dc:date (YYYY-MM-DD or full ISO)
	Subjects      []string // dc:subject tags (genres/categories)
	CoverData     []byte   // raw cover image bytes (JPEG/PNG)
	CoverType     string   // MIME type (image/jpeg, image/png)
}

type container struct {
	Rootfiles []rootfile `xml:"rootfiles>rootfile"`
}

type rootfile struct {
	FullPath string `xml:"full-path,attr"`
}

type opfPackage struct {
	Metadata opfMetadata `xml:"metadata"`
}

type opfMetadata struct {
	Titles      []string        `xml:"title"`
	Creators    []opfCreator    `xml:"creator"`
	Dates       []string        `xml:"date"`
	Subjects    []string        `xml:"subject"`
	Identifiers []opfIdentifier `xml:"identifier"`
	Metas       []opfMeta       `xml:"meta"`
}

type opfCreator struct {
	Value string `xml:",chardata"`
}

type opfIdentifier struct {
	ID     string `xml:"id,attr"`
	Scheme string `xml:"scheme,attr"`
	Value  string `xml:",chardata"`
}

type opfMeta struct {
	Name    string `xml:"name,attr"`
	Content string `xml:"content,attr"`
	Value   string `xml:",chardata"`
}

var isbn13Re = regexp.MustCompile(`(?:^|[^0-9])(97[89]\d{10})(?:[^0-9]|$)`)

// ExtractMetadata opens an EPUB file and extracts title, author, and ISBN-13.
func ExtractMetadata(filepath string) (Metadata, error) {
	r, err := zip.OpenReader(filepath)
	if err != nil {
		return Metadata{}, fmt.Errorf("open epub: %w", err)
	}
	defer r.Close()

	opfPath, err := findOPFPath(r)
	if err != nil {
		return Metadata{}, err
	}

	opfFile, err := findInZip(r, opfPath)
	if err != nil {
		return Metadata{}, fmt.Errorf("find OPF file %q: %w", opfPath, err)
	}
	rc, err := opfFile.Open()
	if err != nil {
		return Metadata{}, fmt.Errorf("open OPF file: %w", err)
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return Metadata{}, fmt.Errorf("read OPF file: %w", err)
	}

	var pkg opfPackage
	if err := xml.Unmarshal(data, &pkg); err != nil {
		return Metadata{}, fmt.Errorf("parse OPF: %w", err)
	}

	m := Metadata{}
	if len(pkg.Metadata.Titles) > 0 {
		m.Title = strings.TrimSpace(pkg.Metadata.Titles[0])
	}
	if len(pkg.Metadata.Creators) > 0 {
		m.Author = strings.TrimSpace(pkg.Metadata.Creators[0].Value)
	}
	m.ISBN = findISBN13(pkg.Metadata)

	// Extract publication date (take first 10 chars for YYYY-MM-DD).
	if len(pkg.Metadata.Dates) > 0 {
		d := strings.TrimSpace(pkg.Metadata.Dates[0])
		if len(d) >= 10 {
			d = d[:10]
		}
		m.PublishedDate = d
	}

	// Extract subjects/genres — normalize and filter junk.
	for _, s := range pkg.Metadata.Subjects {
		s = normalizeGenre(s)
		if s != "" {
			m.Subjects = append(m.Subjects, s)
		}
	}

	// Fallback: if no ISBN in metadata, scan copyright/title pages.
	if m.ISBN == "" {
		m.ISBN = scanContentForISBN(r, opfPath, pkg)
	}

	// Extract cover image.
	m.CoverData, m.CoverType = extractCover(r, opfPath, data)

	return m, nil
}

func findOPFPath(r *zip.ReadCloser) (string, error) {
	cf, err := findInZip(r, "META-INF/container.xml")
	if err != nil {
		return "", fmt.Errorf("find container.xml: %w", err)
	}
	rc, err := cf.Open()
	if err != nil {
		return "", fmt.Errorf("open container.xml: %w", err)
	}
	defer rc.Close()

	var c container
	if err := xml.NewDecoder(rc).Decode(&c); err != nil {
		return "", fmt.Errorf("parse container.xml: %w", err)
	}
	if len(c.Rootfiles) == 0 {
		return "", fmt.Errorf("no rootfile in container.xml")
	}
	return c.Rootfiles[0].FullPath, nil
}

func findInZip(r *zip.ReadCloser, name string) (*zip.File, error) {
	for _, f := range r.File {
		if f.Name == name {
			return f, nil
		}
	}
	for _, f := range r.File {
		if strings.EqualFold(f.Name, name) {
			return f, nil
		}
	}
	base := path.Base(name)
	for _, f := range r.File {
		if strings.EqualFold(path.Base(f.Name), base) && strings.HasSuffix(strings.ToLower(f.Name), strings.ToLower(name)) {
			return f, nil
		}
	}
	return nil, fmt.Errorf("file %q not found in archive", name)
}

func findISBN13(md opfMetadata) string {
	for _, id := range md.Identifiers {
		if isbn := extractISBN13(id.Value); isbn != "" {
			return isbn
		}
		if isbn := extractISBN13(id.Scheme); isbn != "" {
			return isbn
		}
	}
	for _, meta := range md.Metas {
		if strings.Contains(strings.ToLower(meta.Name), "isbn") ||
			strings.Contains(strings.ToLower(meta.Content), "isbn") {
			if isbn := extractISBN13(meta.Content); isbn != "" {
				return isbn
			}
			if isbn := extractISBN13(meta.Value); isbn != "" {
				return isbn
			}
		}
	}
	return ""
}

// scanContentForISBN looks for ISBN-13 in the text content of copyright/title pages.
func scanContentForISBN(r *zip.ReadCloser, opfPath string, pkg opfPackage) string {
	opfDir := path.Dir(opfPath)

	// Look for likely copyright/title pages by filename.
	candidates := []string{}
	for _, f := range r.File {
		lower := strings.ToLower(f.Name)
		if strings.HasSuffix(lower, ".xhtml") || strings.HasSuffix(lower, ".html") {
			base := strings.ToLower(path.Base(lower))
			if strings.Contains(base, "copyright") || strings.Contains(base, "colophon") || strings.Contains(base, "title") || strings.Contains(base, "imprint") {
				candidates = append(candidates, f.Name)
			}
		}
	}

	// Also check the first few spine items (copyright is often near the front).
	type manifestItem struct {
		ID   string `xml:"id,attr"`
		Href string `xml:"href,attr"`
	}
	type spineItem struct {
		IDRef string `xml:"idref,attr"`
	}
	// Re-parse to get manifest/spine (our opfPackage struct doesn't include them).
	type fullPkg struct {
		Manifest []manifestItem `xml:"manifest>item"`
		Spine    []spineItem    `xml:"spine>itemref"`
	}
	opfFile, err := findInZip(r, opfPath)
	if err != nil {
		return ""
	}
	rc, err := opfFile.Open()
	if err != nil {
		return ""
	}
	data, _ := io.ReadAll(rc)
	rc.Close()

	var fp fullPkg
	xml.Unmarshal(data, &fp)

	hrefByID := make(map[string]string)
	for _, item := range fp.Manifest {
		hrefByID[item.ID] = item.Href
	}

	// Add first 6 spine items as candidates.
	limit := 6
	if len(fp.Spine) < limit {
		limit = len(fp.Spine)
	}
	for i := 0; i < limit; i++ {
		if href, ok := hrefByID[fp.Spine[i].IDRef]; ok {
			full := href
			if opfDir != "." {
				full = opfDir + "/" + href
			}
			candidates = append(candidates, full)
		}
	}

	// Deduplicate and scan each candidate.
	seen := make(map[string]bool)
	for _, name := range candidates {
		if seen[name] {
			continue
		}
		seen[name] = true

		f, err := findInZip(r, name)
		if err != nil {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		content, _ := io.ReadAll(rc)
		rc.Close()

		// Strip HTML tags to get plain text.
		text := stripTags(string(content))
		if isbn := extractISBN13(text); isbn != "" {
			return isbn
		}
	}
	return ""
}

// stripTags removes HTML/XML tags from a string.
func stripTags(s string) string {
	var b strings.Builder
	inTag := false
	for _, r := range s {
		if r == '<' {
			inTag = true
		} else if r == '>' {
			inTag = false
		} else if !inTag {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// extractCover finds and reads the cover image from the EPUB.
// It looks for: 1) properties="cover-image" (EPUB3), 2) <meta name="cover"> (EPUB2).
func extractCover(r *zip.ReadCloser, opfPath string, opfData []byte) ([]byte, string) {
	type manifestItem struct {
		ID         string `xml:"id,attr"`
		Href       string `xml:"href,attr"`
		MediaType  string `xml:"media-type,attr"`
		Properties string `xml:"properties,attr"`
	}
	type metaItem struct {
		Name    string `xml:"name,attr"`
		Content string `xml:"content,attr"`
	}
	type coverPkg struct {
		Manifest []manifestItem `xml:"manifest>item"`
		Metas    []metaItem     `xml:"metadata>meta"`
	}

	var pkg coverPkg
	xml.Unmarshal(opfData, &pkg)

	opfDir := path.Dir(opfPath)

	// Strategy 1: EPUB3 — find item with properties="cover-image"
	for _, item := range pkg.Manifest {
		if strings.Contains(item.Properties, "cover-image") {
			href := item.Href
			if opfDir != "." {
				href = opfDir + "/" + href
			}
			if data, err := readZipFile(r, href); err == nil {
				return data, item.MediaType
			}
		}
	}

	// Strategy 2: EPUB2 — <meta name="cover" content="item-id"/>
	for _, meta := range pkg.Metas {
		if strings.ToLower(meta.Name) == "cover" && meta.Content != "" {
			for _, item := range pkg.Manifest {
				if item.ID == meta.Content && strings.HasPrefix(item.MediaType, "image/") {
					href := item.Href
					if opfDir != "." {
						href = opfDir + "/" + href
					}
					if data, err := readZipFile(r, href); err == nil {
						return data, item.MediaType
					}
				}
			}
		}
	}

	return nil, ""
}

const maxCoverSize = 5 << 20 // 5MB limit for cover images

func readZipFile(r *zip.ReadCloser, name string) ([]byte, error) {
	f, err := findInZip(r, name)
	if err != nil {
		return nil, err
	}
	if f.UncompressedSize64 > maxCoverSize {
		return nil, fmt.Errorf("file too large: %d bytes", f.UncompressedSize64)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, maxCoverSize))
}

// normalizeGenre cleans up a dc:subject value into a proper genre name.
// Returns empty string for junk entries that should be filtered out.
func normalizeGenre(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	lower := strings.ToLower(s)

	// Filter out junk subjects.
	junk := []string{
		"unknown", "general", "lost", "scifi",
		"goodreads", "calibre", "permuted",
	}
	for _, j := range junk {
		if strings.Contains(lower, j) {
			return ""
		}
	}

	// Filter single-word entries that are too vague or are tags, not genres.
	if !strings.Contains(s, " ") && !strings.Contains(s, "&") && len(s) < 4 {
		return ""
	}

	// Sentence case: capitalize first letter of each word.
	words := strings.Fields(s)
	for i, w := range words {
		// Keep short connectors lowercase (unless first word).
		lower := strings.ToLower(w)
		if i > 0 && (lower == "and" || lower == "of" || lower == "the" || lower == "in" || lower == "for") {
			words[i] = lower
			continue
		}
		if lower == "&" {
			words[i] = "&"
			continue
		}
		words[i] = strings.ToUpper(w[:1]) + strings.ToLower(w[1:])
	}
	return strings.Join(words, " ")
}

func extractISBN13(s string) string {
	cleaned := strings.ReplaceAll(s, "-", "")
	cleaned = strings.ReplaceAll(cleaned, " ", "")
	if m := isbn13Re.FindStringSubmatch(cleaned); len(m) > 1 {
		return m[1]
	}
	return ""
}
