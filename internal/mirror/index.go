package mirror

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// IndexFileName is the JSON file the mirror manager uses to remember
// per-file metadata that isn't recoverable from the filesystem alone:
// where it came from, when we first got it, when it was last served.
// Lives inside the mirror root and is dot-prefixed so the scanner
// ignores it.
const IndexFileName = ".index.json"

// IndexEntry is the persisted metadata for one mirrored file. Size is
// duplicated from the filesystem so eviction can compute totals without
// stat'ing every file.
type IndexEntry struct {
	SizeBytes      int64     `json:"size_bytes"`
	SourceBranchID string    `json:"source_branch_id"`
	BookID         string    `json:"book_id"`
	AddedAt        time.Time `json:"added_at"`
	LastServedAt   time.Time `json:"last_served_at"`
}

// Index is the in-memory + on-disk index for one mirror root.
type Index struct {
	path string

	mu      sync.Mutex
	entries map[string]IndexEntry // key: sha256 hex (the file's canonical name)
}

// LoadIndex reads the index file at mirrorRoot/.index.json. A missing
// file yields an empty index; corrupted JSON returns an error so we
// don't silently start fresh and orphan all existing tracking data.
func LoadIndex(mirrorRoot string) (*Index, error) {
	i := &Index{
		path:    filepath.Join(mirrorRoot, IndexFileName),
		entries: make(map[string]IndexEntry),
	}
	data, err := os.ReadFile(i.path)
	if err != nil {
		if os.IsNotExist(err) {
			return i, nil
		}
		return nil, fmt.Errorf("mirror index: read: %w", err)
	}
	if len(data) == 0 {
		return i, nil
	}
	if err := json.Unmarshal(data, &i.entries); err != nil {
		return nil, fmt.Errorf("mirror index: decode: %w", err)
	}
	return i, nil
}

// Save writes the index atomically: write to a sibling temp file, then
// rename. A crash mid-write leaves either the old file or the new file
// intact, never a partial write.
func (i *Index) Save() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.saveLocked()
}

func (i *Index) saveLocked() error {
	data, err := json.MarshalIndent(i.entries, "", "  ")
	if err != nil {
		return fmt.Errorf("mirror index: marshal: %w", err)
	}
	tmp := i.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("mirror index: write tmp: %w", err)
	}
	if err := os.Rename(tmp, i.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("mirror index: rename: %w", err)
	}
	return nil
}

// Add records a new entry. AddedAt and LastServedAt are both set to
// now so a freshly-mirrored file isn't instantly evictable. Caller
// should Save() after a successful batch of changes.
func (i *Index) Add(sha string, e IndexEntry) {
	i.mu.Lock()
	defer i.mu.Unlock()
	now := time.Now().UTC()
	if e.AddedAt.IsZero() {
		e.AddedAt = now
	}
	if e.LastServedAt.IsZero() {
		e.LastServedAt = now
	}
	i.entries[sha] = e
}

// Remove deletes the index record (does not touch the file itself —
// caller is responsible for that, in the right order).
func (i *Index) Remove(sha string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.entries, sha)
}

// Clear drops every entry. Caller should Save() afterward so the disk
// matches memory. Used by Manager.Purge().
func (i *Index) Clear() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.entries = make(map[string]IndexEntry)
}

// Get returns a copy of the entry, or zero + false if absent.
func (i *Index) Get(sha string) (IndexEntry, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	e, ok := i.entries[sha]
	return e, ok
}

// Touch updates LastServedAt to now. No-op for unknown sha — we don't
// want a renamed/scanner-discovered file to ghost-create index entries
// (those should always come through Add at download time).
func (i *Index) Touch(sha string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	e, ok := i.entries[sha]
	if !ok {
		return
	}
	e.LastServedAt = time.Now().UTC()
	i.entries[sha] = e
}

// Snapshot returns a copy of all entries. Safe to read without locking.
func (i *Index) Snapshot() map[string]IndexEntry {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := make(map[string]IndexEntry, len(i.entries))
	for k, v := range i.entries {
		out[k] = v
	}
	return out
}

// TotalSize sums SizeBytes across all entries. Used to check against
// the configured mirror_size cap before pulling new candidates.
func (i *Index) TotalSize() int64 {
	i.mu.Lock()
	defer i.mu.Unlock()
	var total int64
	for _, e := range i.entries {
		total += e.SizeBytes
	}
	return total
}

// SourceSize sums SizeBytes for entries from a specific source branch.
// Used by the per-source quota check (no single source > 10% of mirror).
func (i *Index) SourceSize(branchID string) int64 {
	i.mu.Lock()
	defer i.mu.Unlock()
	var total int64
	for _, e := range i.entries {
		if e.SourceBranchID == branchID {
			total += e.SizeBytes
		}
	}
	return total
}
