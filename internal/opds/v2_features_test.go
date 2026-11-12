package opds

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWantsV2(t *testing.T) {
	cases := []struct {
		accept string
		want   bool
	}{
		{"", false},
		{"application/atom+xml", false},
		{"application/opds+json", true},
		{"application/json", true},
		{"*/*", false},
		// Ties and lower q-values go to Atom.
		{"application/opds+json, application/atom+xml", false},
		{"application/opds+json;q=0.5, application/atom+xml", false},
		// JSON preferred only when strictly higher.
		{"application/atom+xml;q=0.8, application/opds+json", true},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/opds", nil)
		if c.accept != "" {
			r.Header.Set("Accept", c.accept)
		}
		if got := WantsV2(r); got != c.want {
			t.Errorf("WantsV2(%q) = %v, want %v", c.accept, got, c.want)
		}
	}
}

// v2 test doubles for unmarshalling emitted feeds.
type tLink struct {
	Rel       string `json:"rel"`
	Href      string `json:"href"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Templated bool   `json:"templated"`
}
type tPub struct {
	Metadata map[string]any `json:"metadata"`
	Links    []tLink        `json:"links"`
	Images   []tLink        `json:"images"`
}
type tFeed struct {
	Metadata struct {
		Title        string `json:"title"`
		ItemsPerPage int    `json:"itemsPerPage"`
		CurrentPage  int    `json:"currentPage"`
	} `json:"metadata"`
	Links      []tLink `json:"links"`
	Navigation []tLink `json:"navigation"`
	Facets     []struct {
		Metadata struct {
			Title string `json:"title"`
		} `json:"metadata"`
		Links []tLink `json:"links"`
	} `json:"facets"`
	Groups []struct {
		Metadata struct {
			Title string `json:"title"`
		} `json:"metadata"`
		Links        []tLink `json:"links"`
		Publications []tPub  `json:"publications"`
	} `json:"groups"`
	Publications []tPub `json:"publications"`
}

func findRel(links []tLink, rel string) *tLink {
	for i := range links {
		if links[i].Rel == rel {
			return &links[i]
		}
	}
	return nil
}

func TestFeedV2SearchPaginationFacetsCovers(t *testing.T) {
	data, err := FeedV2(FeedV2Options{
		Self:       "/opds/new?media=audiobook",
		Title:      "New Arrivals",
		SearchHref: SearchTemplate,
		Page:       2,
		PageSize:   25,
		Entries: []Entry{
			{ID: "MB1", Title: "Book", AcqHref: "/download/MB1", CoverHref: "/covers/MB1.jpg", CoverType: "image/png", Updated: time.Now()},
			{ID: "MB2", Title: "No links at all"}, // must be dropped, not an invalid publication
		},
		Nav: []NavLink{
			{Rel: "first", Href: "/opds/new"},
			{Rel: "previous", Href: "/opds/new"},
			{Rel: "next", Href: "/opds/new?page=3"},
		},
		Facets: []Facet{{
			Title: "Format",
			Links: []FacetLink{
				{Title: "All formats", Href: "/opds/new"},
				{Title: "Audiobooks", Href: "/opds/new?media=audiobook", Active: true},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var feed tFeed
	if err := json.Unmarshal(data, &feed); err != nil {
		t.Fatal(err)
	}

	search := findRel(feed.Links, "search")
	if search == nil || !search.Templated || search.Href != "/opds/search{?query}" {
		t.Fatalf("bad search link: %+v", search)
	}
	if feed.Metadata.CurrentPage != 2 || feed.Metadata.ItemsPerPage != 25 {
		t.Fatalf("pagination metadata: %+v", feed.Metadata)
	}
	if findRel(feed.Links, "first") == nil {
		t.Fatal("first link missing")
	}
	if len(feed.Publications) != 1 {
		t.Fatalf("want 1 publication (link-less dropped), got %d", len(feed.Publications))
	}
	if img := feed.Publications[0].Images[0]; img.Type != "image/png" {
		t.Fatalf("cover type not carried through: %+v", img)
	}
	if len(feed.Facets) != 1 || feed.Facets[0].Metadata.Title != "Format" {
		t.Fatalf("facets: %+v", feed.Facets)
	}
	active := findRel(feed.Facets[0].Links, "self")
	if active == nil || active.Title != "Audiobooks" {
		t.Fatalf("active facet not marked with rel=self: %+v", feed.Facets[0].Links)
	}
}

func TestFeedV2CoverTypeOmittedWhenUnknown(t *testing.T) {
	data, err := FeedV2(FeedV2Options{
		Self:  "/opds",
		Title: "Feed",
		Entries: []Entry{
			{ID: "MB1", Title: "Book", AcqHref: "/opds/download/MB1", CoverHref: "/covers/MB1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var feed tFeed
	if err := json.Unmarshal(data, &feed); err != nil {
		t.Fatal(err)
	}
	if img := feed.Publications[0].Images[0]; img.Type != "" {
		t.Fatalf("cover type should be omitted when unknown, got %q", img.Type)
	}
}

func TestNavigationFeedV2NoticeAndGroups(t *testing.T) {
	notice := &Entry{Title: "No libraries shared with you yet", Summary: "ask a friend", NavHref: "/opds"}
	groups := []Group{
		{Title: "New Arrivals", Href: "/opds/new", Entries: []Entry{
			{ID: "MB1", Title: "Book", AcqHref: "/download/MB1"},
		}},
		// A group with no publications must be dropped entirely.
		{Title: "Empty", Href: "/opds/popular"},
	}
	data, err := NavigationFeedV2(notice, groups)
	if err != nil {
		t.Fatal(err)
	}
	var feed tFeed
	if err := json.Unmarshal(data, &feed); err != nil {
		t.Fatal(err)
	}
	if len(feed.Navigation) == 0 || feed.Navigation[0].Title != notice.Title {
		t.Fatalf("notice not first in navigation: %+v", feed.Navigation)
	}
	if len(feed.Groups) != 1 {
		t.Fatalf("want 1 group (empty dropped), got %d", len(feed.Groups))
	}
	g := feed.Groups[0]
	if g.Metadata.Title != "New Arrivals" || len(g.Publications) != 1 {
		t.Fatalf("group content: %+v", g)
	}
	if self := findRel(g.Links, "self"); self == nil || self.Href != "/opds/new" {
		t.Fatalf("group self link: %+v", g.Links)
	}
	if search := findRel(feed.Links, "search"); search == nil || !search.Templated {
		t.Fatalf("root search link: %+v", feed.Links)
	}
}

func TestNavigationOnlyV2SearchLink(t *testing.T) {
	items := SubsectionEntries([]Entry{{Title: "Fantasy", NavHref: "/opds/genres/Fantasy"}})
	data, err := NavigationOnlyV2("/opds/genres", "Browse by Genre", SearchTemplate, items)
	if err != nil {
		t.Fatal(err)
	}
	var feed tFeed
	if err := json.Unmarshal(data, &feed); err != nil {
		t.Fatal(err)
	}
	if search := findRel(feed.Links, "search"); search == nil || !search.Templated || search.Href != SearchTemplate {
		t.Fatalf("search link missing from navigation feed: %+v", feed.Links)
	}
	if len(feed.Navigation) != 1 || feed.Navigation[0].Href != "/opds/genres/Fantasy" {
		t.Fatalf("navigation items: %+v", feed.Navigation)
	}
}
