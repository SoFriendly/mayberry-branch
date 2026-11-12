package opds

import "strings"

// QueryMatcher returns a predicate reporting whether an item's text fields
// match the search query. Every space-separated word of the query must appear
// in at least one field, compared case-insensitively against the field with
// its spaces removed — so "nightvale" still finds "Night Vale", and word
// order doesn't matter. A space-free word that occurs in the original text
// always occurs in the stripped text too, so the stripped comparison alone
// preserves plain substring matches.
func QueryMatcher(query string) func(fields ...string) bool {
	words := strings.Fields(strings.ToLower(query))
	if len(words) == 0 {
		// Whitespace-only queries match nothing rather than everything.
		return func(...string) bool { return false }
	}
	return func(fields ...string) bool {
		stripped := make([]string, len(fields))
		for i, f := range fields {
			stripped[i] = strings.ReplaceAll(strings.ToLower(f), " ", "")
		}
	next:
		for _, w := range words {
			for _, f := range stripped {
				if strings.Contains(f, w) {
					continue next
				}
			}
			return false
		}
		return true
	}
}
