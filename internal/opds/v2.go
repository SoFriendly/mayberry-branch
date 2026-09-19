package opds

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// OPDS 2.0 — Readium Web Pub Manifest based JSON format.
// Spec: https://drafts.opds.io/opds-2.0
// Content type: application/opds+json

// SearchTemplate is the RFC 6570 URI template advertised on OPDS 2.0 search
// links. The spec's search convention expands the `query` variable, unlike
// OpenSearch's {searchTerms} used by the 1.2 Atom feeds — handlers accept
// both parameter names.
const SearchTemplate = "/opds/search{?query}"

// WantsV2 returns true if the client prefers OPDS 2.0 JSON over OPDS 1.2
// Atom XML, honoring q-values per RFC 7231 §5.3.2. A tie (or no explicit
// preference) defaults to Atom — the legacy format remains the default so
// existing clients aren't surprised.
func WantsV2(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if accept == "" {
		return false
	}
	var jsonQ, atomQ float64
	for _, part := range strings.Split(accept, ",") {
		mt, q := parseAcceptPart(part)
		switch {
		case strings.Contains(mt, "opds+json"), mt == "application/json":
			if q > jsonQ {
				jsonQ = q
			}
		case strings.Contains(mt, "atom+xml"):
			if q > atomQ {
				atomQ = q
			}
		}
	}
	// No explicit JSON ask → fall back to Atom.
	if jsonQ == 0 {
		return false
	}
	// JSON only wins if strictly greater than Atom; ties go to Atom.
	return jsonQ > atomQ
}

// parseAcceptPart returns the lowercase media type and q-value (default 1.0)
// for one comma-separated chunk of an Accept header.
func parseAcceptPart(part string) (string, float64) {
	segments := strings.Split(strings.TrimSpace(part), ";")
	mt := strings.ToLower(strings.TrimSpace(segments[0]))
	q := 1.0
	for _, seg := range segments[1:] {
		seg = strings.TrimSpace(seg)
		if strings.HasPrefix(seg, "q=") {
			if v, err := strconv.ParseFloat(strings.TrimSpace(seg[2:]), 64); err == nil {
				q = v
			}
		}
	}
	return mt, q
}

// Facet is one OPDS 2.0 facet group (e.g. "Format") whose links re-request
// the current feed with a filter applied. The active link is marked with
// rel=self so clients can highlight the current selection.
type Facet struct {
	Title string
	Links []FacetLink
}

type FacetLink struct {
	Title  string
	Href   string
	Active bool
}

// Group is one OPDS 2.0 group: a titled lane of publications with a link to
// the full feed it previews. Used on the catalog root.
type Group struct {
	Title   string
	Href    string // full feed this group previews
	Entries []Entry
}

// FeedV2Options names the inputs to FeedV2. Zero values omit the
// corresponding output: no SearchHref → no search link, Page/PageSize/Total
// 0 → no pagination metadata, and so on.
type FeedV2Options struct {
	Self       string
	Title      string
	Subtitle   string
	SearchHref string // RFC 6570 template; usually SearchTemplate
	Page       int    // 1-based currentPage
	PageSize   int    // itemsPerPage
	Total      int    // numberOfItems
	Entries    []Entry
	Nav        []NavLink
	Facets     []Facet
	Groups     []Group
}

type v2Feed struct {
	Metadata     v2FeedMetadata  `json:"metadata"`
	Links        []v2Link        `json:"links"`
	Navigation   []v2NavItem     `json:"navigation,omitempty"`
	Publications []v2Publication `json:"publications,omitempty"`
	Facets       []v2Facet       `json:"facets,omitempty"`
	Groups       []v2Group       `json:"groups,omitempty"`
}

type v2FeedMetadata struct {
	Title       string `json:"title"`
	Subtitle    string `json:"subtitle,omitempty"`
	Description string `json:"description,omitempty"`
	Modified    string `json:"modified,omitempty"`

	// Pagination
	NumberOfItems int `json:"numberOfItems,omitempty"`
	ItemsPerPage  int `json:"itemsPerPage,omitempty"`
	CurrentPage   int `json:"currentPage,omitempty"`
}

type v2Link struct {
	Rel       string `json:"rel,omitempty"`
	Href      string `json:"href"`
	Type      string `json:"type,omitempty"`
	Title     string `json:"title,omitempty"`
	Templated bool   `json:"templated,omitempty"`
}

type v2NavItem struct {
	Href        string `json:"href"`
	Title       string `json:"title"`
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
}

type v2Facet struct {
	Metadata v2SubMetadata `json:"metadata"`
	Links    []v2Link      `json:"links"`
}

type v2Group struct {
	Metadata     v2SubMetadata   `json:"metadata"`
	Links        []v2Link        `json:"links,omitempty"`
	Publications []v2Publication `json:"publications,omitempty"`
}

// v2SubMetadata is the metadata object of a facet or group collection.
type v2SubMetadata struct {
	Title         string `json:"title"`
	NumberOfItems int    `json:"numberOfItems,omitempty"`
}

type v2Publication struct {
	Metadata v2PubMetadata `json:"metadata"`
	Links    []v2Link      `json:"links"`
	Images   []v2Link      `json:"images,omitempty"`
}

type v2PubMetadata struct {
	Type        string   `json:"@type,omitempty"`
	Identifier  string   `json:"identifier,omitempty"`
	Title       string   `json:"title"`
	Author      string   `json:"author,omitempty"`
	Narrator    string   `json:"narrator,omitempty"`
	Subject     []string `json:"subject,omitempty"`
	Language    string   `json:"language,omitempty"`
	Description string   `json:"description,omitempty"`
	Modified    string   `json:"modified,omitempty"`
	Duration    int      `json:"duration,omitempty"` // seconds
}

// FeedV2 emits an OPDS 2.0 JSON feed.
func FeedV2(o FeedV2Options) ([]byte, error) {
	feed := v2Feed{
		Metadata: v2FeedMetadata{
			Title:    o.Title,
			Subtitle: o.Subtitle,
			Modified: time.Now().UTC().Format(time.RFC3339),
		},
		Links: []v2Link{
			{Rel: "self", Href: o.Self, Type: "application/opds+json"},
		},
	}
	if o.SearchHref != "" {
		feed.Links = append(feed.Links, v2Link{
			Rel: "search", Href: o.SearchHref, Type: "application/opds+json", Templated: true,
		})
	}
	if o.Page > 0 {
		feed.Metadata.CurrentPage = o.Page
	}
	if o.PageSize > 0 {
		feed.Metadata.ItemsPerPage = o.PageSize
	}
	if o.Total > 0 {
		feed.Metadata.NumberOfItems = o.Total
	}

	for _, n := range o.Nav {
		feed.Links = append(feed.Links, v2Link{
			Rel:  n.Rel,
			Href: n.Href,
			Type: "application/opds+json",
		})
	}

	feed.Navigation, feed.Publications = splitEntries(o.Entries)

	for _, f := range o.Facets {
		vf := v2Facet{Metadata: v2SubMetadata{Title: f.Title}}
		for _, l := range f.Links {
			link := v2Link{Href: l.Href, Title: l.Title, Type: "application/opds+json"}
			if l.Active {
				link.Rel = "self"
			}
			vf.Links = append(vf.Links, link)
		}
		feed.Facets = append(feed.Facets, vf)
	}

	for _, g := range o.Groups {
		vg := v2Group{Metadata: v2SubMetadata{Title: g.Title}}
		if g.Href != "" {
			vg.Links = append(vg.Links, v2Link{Rel: "self", Href: g.Href, Type: "application/opds+json"})
		}
		_, vg.Publications = splitEntries(g.Entries)
		feed.Groups = append(feed.Groups, vg)
	}

	return json.MarshalIndent(feed, "", "  ")
}

// splitEntries routes entries into the navigation and publications
// collections. Subsection entries (folders, hints) have a NavHref instead of
// an acquisition link; a publication without links is a dead end for
// clients, so those go in navigation — and an entry with no links at all is
// dropped rather than emitted as an invalid publication.
func splitEntries(entries []Entry) (nav []v2NavItem, pubs []v2Publication) {
	for _, e := range entries {
		if e.NavHref != "" && e.AcqHref == "" {
			nav = append(nav, v2NavItem{
				Href:        e.NavHref,
				Title:       e.Title,
				Type:        "application/opds+json",
				Description: e.Summary,
			})
			continue
		}
		if e.AcqHref == "" {
			continue
		}
		pubs = append(pubs, entryToPublication(e))
	}
	return nav, pubs
}

// NavigationFeedV2 emits the OPDS 2.0 root feed: top-level navigation, an
// optional sharing-explainer notice, and preview groups (lanes) of the main
// feeds for clients that render them.
func NavigationFeedV2(notice *Entry, groups []Group) ([]byte, error) {
	feed := v2Feed{
		Metadata: v2FeedMetadata{
			Title:    "Mayberry Library Catalog",
			Subtitle: "A federated EPUB & audiobook library. Share your own collection at joinmayberry.com",
			Modified: time.Now().UTC().Format(time.RFC3339),
		},
		Links: []v2Link{
			{Rel: "self", Href: "/opds", Type: "application/opds+json"},
			{Rel: "search", Href: SearchTemplate, Type: "application/opds+json", Templated: true},
		},
		Navigation: []v2NavItem{
			{Href: "/opds/releases", Title: "New Releases", Type: "application/opds+json"},
			{Href: "/opds/new", Title: "New Arrivals", Type: "application/opds+json"},
			{Href: "/opds/popular", Title: "Top Reads", Type: "application/opds+json"},
			{Href: "/opds/genres", Title: "Browse by Genre", Type: "application/opds+json"},
			{Href: "/opds/branches", Title: "Branches", Type: "application/opds+json"},
		},
	}
	if notice != nil {
		feed.Navigation = append([]v2NavItem{{
			Href:        notice.NavHref,
			Title:       notice.Title,
			Type:        "application/opds+json",
			Description: notice.Summary,
		}}, feed.Navigation...)
	}
	for _, g := range groups {
		vg := v2Group{Metadata: v2SubMetadata{Title: g.Title}}
		if g.Href != "" {
			vg.Links = append(vg.Links, v2Link{Rel: "self", Href: g.Href, Type: "application/opds+json"})
		}
		_, vg.Publications = splitEntries(g.Entries)
		if len(vg.Publications) > 0 {
			feed.Groups = append(feed.Groups, vg)
		}
	}
	return json.MarshalIndent(feed, "", "  ")
}

func entryToPublication(e Entry) v2Publication {
	schemaType := "https://schema.org/Book"
	acqType := e.AcqType
	if acqType == "" {
		acqType = "application/epub+zip"
	}
	if e.MediaType == "audiobook" {
		schemaType = "https://schema.org/Audiobook"
		if e.AcqType == "" {
			acqType = "audio/mp4"
		}
	}

	identifier := e.ID
	if identifier == "" && e.ISBN != "" {
		identifier = "urn:isbn:" + e.ISBN
	}

	pub := v2Publication{
		Metadata: v2PubMetadata{
			Type:        schemaType,
			Identifier:  identifier,
			Title:       e.Title,
			Author:      e.Author,
			Narrator:    e.Narrator,
			Subject:     e.Categories,
			Language:    e.Language,
			Description: e.Summary,
			Duration:    e.DurationSeconds,
		},
	}
	if !e.Updated.IsZero() {
		pub.Metadata.Modified = e.Updated.UTC().Format(time.RFC3339)
	}
	if e.AcqHref != "" {
		pub.Links = append(pub.Links, v2Link{
			Rel:  "http://opds-spec.org/acquisition",
			Href: e.AcqHref,
			Type: acqType,
		})
	}
	if e.CoverHref != "" {
		// Type omitted when unknown (valid per RWPM); clients sniff.
		pub.Images = append(pub.Images, v2Link{
			Href: e.CoverHref,
			Type: e.CoverType,
		})
	}
	return pub
}

// NavigationOnlyV2 emits a feed of nav items (used when entries are subsections, not publications).
func NavigationOnlyV2(self, title, searchHref string, navItems []v2NavItem) ([]byte, error) {
	feed := v2Feed{
		Metadata: v2FeedMetadata{
			Title:    title,
			Modified: time.Now().UTC().Format(time.RFC3339),
		},
		Links: []v2Link{
			{Rel: "self", Href: self, Type: "application/opds+json"},
		},
		Navigation: navItems,
	}
	if searchHref != "" {
		feed.Links = append(feed.Links, v2Link{
			Rel: "search", Href: searchHref, Type: "application/opds+json", Templated: true,
		})
	}
	return json.MarshalIndent(feed, "", "  ")
}

// SubsectionEntries converts Entry slices whose NavHref/Title represent subsections
// (genres, branches) into OPDS 2.0 navigation items.
func SubsectionEntries(entries []Entry) []v2NavItem {
	out := make([]v2NavItem, 0, len(entries))
	for _, e := range entries {
		if e.NavHref == "" {
			continue
		}
		out = append(out, v2NavItem{
			Href:        e.NavHref,
			Title:       e.Title,
			Type:        "application/opds+json",
			Description: e.Summary,
		})
	}
	return out
}
