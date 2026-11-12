package opds

import "testing"

func TestQueryMatcher(t *testing.T) {
	title, author := "Welcome to Night Vale", "Joseph Fink"
	cases := []struct {
		query string
		want  bool
	}{
		{"night vale", true},
		{"nightvale", true},            // space-stripped match
		{"welcome to nightvale", true}, // mixed words and joined words
		{"NIGHT", true},                // case-insensitive
		{"vale fink", true},            // words may match different fields
		{"fink", true},
		{"welcometonightvale", true},
		{"night dale", false}, // every word must match
		{"valenight", false},  // joined words must keep their order
		{"", false},
		{"   ", false},
	}
	for _, c := range cases {
		m := QueryMatcher(c.query)
		if got := m(title, author); got != c.want {
			t.Errorf("QueryMatcher(%q)(%q, %q) = %v, want %v", c.query, title, author, got, c.want)
		}
	}
}
