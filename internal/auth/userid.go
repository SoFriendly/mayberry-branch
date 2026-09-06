package auth

import (
	"crypto/rand"
	"fmt"
	"regexp"
	"strings"
)

// User IDs are the whole account system: a random, unique, unchangeable
// token assigned by Town Square when a branch first registers. The owner
// gives it to friends (who add it to their branch's shared list) and also
// signs into OPDS readers with it — entered as both the Basic-auth
// username and password, since readers like Crosspoint require both
// fields to be non-empty before they send the header.

// Kept short (9 chars) so it's painless to type on an e-reader's
// on-screen keyboard; 36^9 ≈ 46 bits is still far beyond guessable.
const userIDLen = 9

const userIDAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// userIDRe accepts our generated form plus room for future variants.
// Deliberately loose on length so a format change doesn't strand old IDs.
var userIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,63}$`)

// GenerateUserID returns a new random user ID like "x7k2m9qp4".
func GenerateUserID() (string, error) {
	buf := make([]byte, userIDLen)
	// Rejection-sample so every alphabet character is equally likely:
	// 252 is the largest multiple of 36 that fits in a byte.
	const limit = byte(252)
	for i := 0; i < userIDLen; {
		var b [1]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", fmt.Errorf("generate user id: %w", err)
		}
		if b[0] >= limit {
			continue
		}
		buf[i] = userIDAlphabet[int(b[0])%len(userIDAlphabet)]
		i++
	}
	return string(buf), nil
}

// ValidUserID reports whether s looks like a user ID. Used to filter
// junk out of shared lists and auth attempts before they reach the DB.
func ValidUserID(s string) bool {
	return userIDRe.MatchString(s)
}

// NormalizeUserIDs lowercases, trims, dedupes, and drops invalid entries
// from a user-supplied ID list, capping it at 500 entries.
func NormalizeUserIDs(ids []string) []string {
	const maxShared = 500
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.ToLower(strings.TrimSpace(id))
		if !ValidUserID(id) || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
		if len(out) >= maxShared {
			break
		}
	}
	return out
}
