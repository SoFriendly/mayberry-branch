package mirror

import (
	"sync"
	"time"
)

// BlacklistThreshold is the number of consecutive rejects from a single
// source that triggers a local blacklist. Counter resets on any accept
// from that source — transient hiccups don't accumulate, sustained
// badness does.
const BlacklistThreshold = 5

// BlacklistDuration is how long a source stays blacklisted after it
// trips the threshold. Long enough to survive a flap, short enough that
// a recovered source eventually gets retried.
const BlacklistDuration = 24 * time.Hour

// Blacklist tracks per-source consecutive-reject counts and locks out
// sources that cross BlacklistThreshold. Thread-safe.
type Blacklist struct {
	mu        sync.Mutex
	counts    map[string]int          // source_branch_id → consecutive rejects
	blockUntil map[string]time.Time   // source_branch_id → unblock time
}

// NewBlacklist constructs an empty blacklist.
func NewBlacklist() *Blacklist {
	return &Blacklist{
		counts:     make(map[string]int),
		blockUntil: make(map[string]time.Time),
	}
}

// IsBlocked returns true if the source is currently locked out. Expired
// entries are cleaned up lazily on access.
func (b *Blacklist) IsBlocked(source string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	until, ok := b.blockUntil[source]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(b.blockUntil, source)
		delete(b.counts, source) // start counting fresh after expiry
		return false
	}
	return true
}

// RecordReject increments the source's reject counter. Returns true if
// this push tipped the source into blacklisted state (so the caller can
// log it).
func (b *Blacklist) RecordReject(source string) bool {
	if source == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.counts[source]++
	if b.counts[source] >= BlacklistThreshold {
		// Already blacklisted? Don't re-trigger a fresh log line.
		_, was := b.blockUntil[source]
		b.blockUntil[source] = time.Now().Add(BlacklistDuration)
		return !was
	}
	return false
}

// RecordAccept resets the source's reject counter. Successful downloads
// are evidence the source is functioning, regardless of past failures.
func (b *Blacklist) RecordAccept(source string) {
	if source == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.counts, source)
}

// Snapshot returns a copy of currently-blacklisted sources keyed by
// their unblock time. For dashboard/admin display.
func (b *Blacklist) Snapshot() map[string]time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	out := make(map[string]time.Time, len(b.blockUntil))
	for k, v := range b.blockUntil {
		if v.After(now) {
			out[k] = v
		}
	}
	return out
}
