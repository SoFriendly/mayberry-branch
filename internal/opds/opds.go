package opds

import (
	"encoding/xml"
	"fmt"
	"time"
)

// NavLink is an extra navigation link (next/previous page).
type NavLink struct {
	Rel  string
	Href string
}

// Entry represents a single book in an OPDS catalog.
type Entry struct {
	ID         string
	Title      string
	Author     string
	ISBN       string
	Summary    string
	Categories []string
	Updated    time.Time
	AcqHref    string // Acquisition link URL
	NavHref    string // Navigation subsection link URL
	CoverHref  string // Cover image URL (optional)
	CoverType  string // Cover MIME type; "" when unknown

	// Fields used by OPDS 2.0 emission (ignored by 1.2 Atom output).
	MediaType       string // "ebook" or "audiobook"
	AcqType         string // MIME type for the acquisition link
	Language        string
	Narrator        string
	DurationSeconds int
}

// Feed generates an OPDS 1.2 Atom XML feed.
func Feed(id, title string, entries []Entry) ([]byte, error) {
	return FeedWithNav(id, title, entries, nil)
}

// FeedWithNav generates an OPDS feed with optional navigation links (next/previous).
func FeedWithNav(id, title string, entries []Entry, navLinks []NavLink) ([]byte, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	feed := atomFeed{
		XMLNS:     "http://www.w3.org/2005/Atom",
		XMLNSOPDS: "http://opds-spec.org/2010/catalog",
		ID:        id,
		Title:     title,
		Updated:   now,
	}

	// OpenSearch link
	feed.Links = append(feed.Links, atomLink{
		Rel:  "search",
		Href: "/opds/search?q={searchTerms}",
		Type: "application/opensearchdescription+xml",
	})
	feed.Links = append(feed.Links, atomLink{
		Rel:  "self",
		Href: "/opds",
		Type: "application/atom+xml;profile=opds-catalog;kind=navigation",
	})
	for _, nl := range navLinks {
		feed.Links = append(feed.Links, atomLink{
			Rel:  nl.Rel,
			Href: nl.Href,
			Type: "application/atom+xml;profile=opds-catalog",
		})
	}

	for _, e := range entries {
		entryID := e.ID
		if entryID == "" && e.ISBN != "" {
			entryID = "urn:isbn:" + e.ISBN
		}
		if entryID == "" {
			entryID = fmt.Sprintf("urn:mayberry:book:%s", e.Title)
		}

		updated := now
		if !e.Updated.IsZero() {
			updated = e.Updated.UTC().Format(time.RFC3339)
		}

		ae := atomEntry{
			ID:      entryID,
			Title:   e.Title,
			Updated: updated,
		}
		if e.Author != "" {
			ae.Author = &atomAuthor{Name: e.Author}
		}
		if e.Summary != "" {
			ae.Summary = &atomSummary{Type: "text", Value: e.Summary}
		}
		for _, cat := range e.Categories {
			ae.Categories = append(ae.Categories, atomCategory{Term: cat, Label: cat})
		}
		if e.NavHref != "" {
			ae.Links = append(ae.Links, atomLink{
				Rel:  "subsection",
				Href: e.NavHref,
				Type: "application/atom+xml;profile=opds-catalog;kind=acquisition",
			})
		}
		if e.AcqHref != "" {
			acqType := e.AcqType
			if acqType == "" {
				acqType = "application/epub+zip"
			}
			ae.Links = append(ae.Links, atomLink{
				Rel:  "http://opds-spec.org/acquisition",
				Href: e.AcqHref,
				Type: acqType,
			})
		}
		if e.CoverHref != "" {
			coverType := e.CoverType
			if coverType == "" {
				coverType = "image/jpeg"
			}
			ae.Links = append(ae.Links, atomLink{
				Rel:  "http://opds-spec.org/image",
				Href: e.CoverHref,
				Type: coverType,
			})
		}

		feed.Entries = append(feed.Entries, ae)
	}

	output, err := xml.MarshalIndent(feed, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal OPDS feed: %w", err)
	}
	return append([]byte(xml.Header), output...), nil
}

// NavigationFeed generates an OPDS navigation feed (top-level catalog).
// A non-nil notice is prepended as an informational entry — used to tell
// users with no shared libraries how sharing works.
func NavigationFeed(baseURL string, notice *Entry) ([]byte, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	feed := atomFeed{
		XMLNS:     "http://www.w3.org/2005/Atom",
		XMLNSOPDS: "http://opds-spec.org/2010/catalog",
		ID:        "urn:mayberry:catalog",
		Title:     "Mayberry Library Catalog",
		Subtitle:  "A federated EPUB library. Share your own collection at joinmayberry.com",
		Author:    &atomAuthor{Name: "Mayberry Network", URI: "https://joinmayberry.com"},
		Updated:   now,
	}
	feed.Links = append(feed.Links, atomLink{
		Rel:  "self",
		Href: "/opds",
		Type: "application/atom+xml;profile=opds-catalog;kind=navigation",
	})
	feed.Links = append(feed.Links, atomLink{
		Rel:  "search",
		Href: "/opds/opensearch.xml",
		Type: "application/opensearchdescription+xml",
	})

	if notice != nil {
		feed.Entries = append(feed.Entries, atomEntry{
			ID:      notice.ID,
			Title:   notice.Title,
			Updated: now,
			Summary: &atomSummary{Type: "text", Value: notice.Summary},
			Links: []atomLink{{
				Rel:  "subsection",
				Href: notice.NavHref,
				Type: "application/atom+xml;profile=opds-catalog;kind=navigation",
			}},
		})
	}

	navEntries := []struct {
		id, title, href string
	}{
		{"urn:mayberry:releases", "New Releases", "/opds/releases"},
		{"urn:mayberry:new", "New Arrivals", "/opds/new"},
		{"urn:mayberry:popular", "Top Reads", "/opds/popular"},
		{"urn:mayberry:genres", "Browse by Genre", "/opds/genres"},
		{"urn:mayberry:branches", "Branches", "/opds/branches"},
	}
	for _, n := range navEntries {
		feed.Entries = append(feed.Entries, atomEntry{
			ID:      n.id,
			Title:   n.title,
			Updated: now,
			Links: []atomLink{{
				Rel:  "subsection",
				Href: n.href,
				Type: "application/atom+xml;profile=opds-catalog;kind=acquisition",
			}},
		})
	}

	output, err := xml.MarshalIndent(feed, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), output...), nil
}

// OpenSearchDescription returns an OpenSearch XML description.
func OpenSearchDescription(baseURL string) []byte {
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<OpenSearchDescription xmlns="http://a9.com/-/spec/opensearch/1.1/">
  <ShortName>Mayberry</ShortName>
  <Description>Search the Mayberry Library Catalog</Description>
  <Url type="application/atom+xml;profile=opds-catalog" template="%s/opds/search?q={searchTerms}"/>
</OpenSearchDescription>`, baseURL))
}

// Atom XML structures

type atomFeed struct {
	XMLName   xml.Name    `xml:"feed"`
	XMLNS     string      `xml:"xmlns,attr"`
	XMLNSOPDS string      `xml:"xmlns:opds,attr,omitempty"`
	ID        string      `xml:"id"`
	Title     string      `xml:"title"`
	Subtitle  string      `xml:"subtitle,omitempty"`
	Author    *atomAuthor `xml:"author,omitempty"`
	Updated   string      `xml:"updated"`
	Links     []atomLink  `xml:"link"`
	Entries   []atomEntry `xml:"entry"`
}

type atomEntry struct {
	ID         string         `xml:"id"`
	Title      string         `xml:"title"`
	Updated    string         `xml:"updated"`
	Author     *atomAuthor    `xml:"author,omitempty"`
	Summary    *atomSummary   `xml:"summary,omitempty"`
	Categories []atomCategory `xml:"category,omitempty"`
	Links      []atomLink     `xml:"link"`
}

type atomAuthor struct {
	Name string `xml:"name"`
	URI  string `xml:"uri,omitempty"`
}

type atomSummary struct {
	Type  string `xml:"type,attr"`
	Value string `xml:",chardata"`
}

type atomCategory struct {
	Term  string `xml:"term,attr"`
	Label string `xml:"label,attr"`
}

type atomLink struct {
	Rel  string `xml:"rel,attr"`
	Href string `xml:"href,attr"`
	Type string `xml:"type,attr,omitempty"`
}
