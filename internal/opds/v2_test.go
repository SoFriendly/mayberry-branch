package opds

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFeedV2FolderEntriesAreNavigation(t *testing.T) {
	entries := []Entry{
		{
			ID:      "urn:mayberry:branch:potatofarm:folder:cloudberg",
			Title:   "📁 cloudberg",
			Summary: "62 books",
			NavHref: "/opds/branch/potatofarm?folder=cloudberg",
		},
		{
			ID:      "urn:isbn:MB1",
			Title:   "A Book",
			Author:  "Someone",
			AcqHref: "/download/MB1",
		},
	}
	data, err := FeedV2(FeedV2Options{Self: "/opds/branch/potatofarm", Title: "PotatoFarm", Page: 1, Entries: entries})
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `"navigation"`) {
		t.Fatalf("folder entry missing from navigation: %s", s)
	}
	if !strings.Contains(s, `"href": "/opds/branch/potatofarm?folder=cloudberg"`) {
		t.Fatalf("folder href missing: %s", s)
	}
	if strings.Contains(s, `"links": null`) {
		t.Fatalf("publication with null links leaked through: %s", s)
	}
	if !strings.Contains(s, `"/download/MB1"`) {
		t.Fatalf("book publication lost: %s", s)
	}
}

func TestPublicationSelfLinkInFeed(t *testing.T) {
	data, err := FeedV2(FeedV2Options{Self: "/opds/new", Title: "New", Entries: []Entry{
		{ISBN: "9780593820315", Title: "A Book", AcqHref: "/download/9780593820315"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `"href": "/opds/publications/9780593820315"`) {
		t.Fatalf("publication self link missing: %s", s)
	}
	// Exact media type — clients match with strcmp, no parameters allowed.
	if !strings.Contains(s, `"type": "application/opds-publication+json"`) {
		t.Fatalf("publication self link type wrong: %s", s)
	}
}

func TestPublicationV2Document(t *testing.T) {
	data, err := PublicationV2(Entry{
		ID:        "MBed88618795e1",
		Title:     "A Book",
		Author:    "Someone",
		Publisher: "Pub House",
		Summary:   "<p>Great &amp; <b>bold</b> story.</p>",
		AcqHref:   "/opds/download/MBed88618795e1",
		CoverHref: "/covers/MBed88618795e1",
		CoverType: "image/jpeg",
	})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Metadata struct {
			Title       string `json:"title"`
			Author      string `json:"author"`
			Publisher   string `json:"publisher"`
			Description string `json:"description"`
		} `json:"metadata"`
		Links  []map[string]any `json:"links"`
		Images []map[string]any `json:"images"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Metadata.Title != "A Book" || doc.Metadata.Author != "Someone" || doc.Metadata.Publisher != "Pub House" {
		t.Fatalf("metadata wrong: %+v", doc.Metadata)
	}
	// HTML stripped, entities unescaped: JSON clients don't strip tags.
	if doc.Metadata.Description != "Great & bold story." {
		t.Fatalf("description not plain text: %q", doc.Metadata.Description)
	}
	if len(doc.Images) != 1 || doc.Images[0]["href"] != "/covers/MBed88618795e1" {
		t.Fatalf("images wrong: %+v", doc.Images)
	}
	rels := map[string]string{}
	for _, l := range doc.Links {
		rel, _ := l["rel"].(string)
		href, _ := l["href"].(string)
		rels[rel] = href
	}
	if rels["http://opds-spec.org/acquisition"] != "/opds/download/MBed88618795e1" {
		t.Fatalf("acquisition link wrong: %+v", doc.Links)
	}
	if rels["self"] != "/opds/publications/MBed88618795e1" {
		t.Fatalf("self link wrong: %+v", doc.Links)
	}
}

func TestPlainText(t *testing.T) {
	for in, want := range map[string]string{
		"plain already":            "plain already",
		"<p>Hi<br/>there</p>":      "Hithere",
		"a &lt;tag&gt; &amp; more": "a <tag> & more",
		"<div class=\"x\">y</div>": "y",
	} {
		if got := plainText(in); got != want {
			t.Errorf("plainText(%q) = %q, want %q", in, got, want)
		}
	}
}
