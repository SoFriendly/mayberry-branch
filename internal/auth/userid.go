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

// Kept short (9 digits, leading digit non-zero) so it reads and types
// like a real library card number on an e-reader keyboard. ~10^9
// combinations is plenty while the network is small; lengthen userIDLen
// if it grows — ValidUserID is deliberately loose, so old IDs (including
// pre-numeric alphanumeric ones) stay valid.
const userIDLen = 9

// userIDRe accepts our generated form plus room for future variants.
var userIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,63}$`)

// GenerateUserID returns a new random numeric user ID like "204817396".
func GenerateUserID() (string, error) {
	buf := make([]byte, userIDLen)
	// Rejection-sample so every digit is equally likely: 250 is the
	// largest multiple of 10 that fits in a byte (252 for the 9-way
	// non-zero first digit).
	for i := 0; i < userIDLen; {
		var b [1]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", fmt.Errorf("generate user id: %w", err)
		}
		if i == 0 {
			if b[0] >= 252 {
				continue
			}
			buf[i] = '1' + b[0]%9
		} else {
			if b[0] >= 250 {
				continue
			}
			buf[i] = '0' + b[0]%10
		}
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
