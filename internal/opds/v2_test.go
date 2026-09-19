package opds

import (
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
