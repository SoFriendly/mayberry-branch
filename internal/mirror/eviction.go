package mirror

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// PerSourceFraction caps any single source's footprint inside the mirror.
// 10% means a 100 GB mirror can hold up to 10 GB from any one source,
// protecting against a single pollution-attacker monopolizing the cap.
const PerSourceFraction = 0.10

// EvictionProtectionWindow is how recently a file must have been served
// before we refuse to evict it. Prevents pulling the rug out from under
// an in-flight serve (especially on Windows where open files can't be
// deleted at all).
const EvictionProtectionWindow = 60 * time.Second

// HolderCountFetcher fetches current online-holder counts for a list of
// book IDs from Town Square. The eviction logic uses these to prefer
// dropping books that have become common.
type HolderCountFetcher func(ctx context.Context, bookIDs []string) (map[string]int, error)

// Evict deletes files from the mirror until total size is under capBytes.
// Ranking: prefer to drop books that are (a) no longer rare and
// (b) haven't been served recently. Returns bytes freed.
//
// Files served within EvictionProtectionWindow are skipped — they're
// likely being served right now and yanking them mid-stream produces
// truncated reader downloads.
//
// The index, the filesystem, and Town Square's holder counts can drift
// apart between invocations; this function reconciles them defensively:
// missing-from-disk entries are dropped from the index, missing-from-
// index files on disk are left alone (someone else's job to clean up).
func Evict(ctx context.Context, idx *Index, mirrorRoot string, capBytes int64, fetch HolderCountFetcher) (int64, error) {
	snap := idx.Snapshot()
	if len(snap) == 0 {
		return 0, nil
	}

	// Compute current total. If we're already under cap, no work to do.
	var total int64
	for _, e := range snap {
		total += e.SizeBytes
	}
	if total <= capBytes {
		return 0, nil
	}

	// Fetch fresh holder counts for everything we hold. A book with
	// N holders is "more abundant" than one with 1 holder; dropping
	// the abundant one degrades the network less.
	bookIDs := make([]string, 0, len(snap))
	for _, e := range snap {
		if e.BookID != "" {
			bookIDs = append(bookIDs, e.BookID)
		}
	}
	counts := map[string]int{}
	if fetch != nil && len(bookIDs) > 0 {
		c, err := fetch(ctx, bookIDs)
		if err != nil {
			log.Printf("mirror: holder-counts fetch failed, evicting blind: %v", err)
		} else {
			counts = c
		}
	}

	// Build a sortable list. The least-valuable file goes first: most
	// abundant on the network and longest-since-served.
	type candidate struct {
		sha    string
		entry  IndexEntry
		count  int
	}
	now := time.Now().UTC()
	cands := make([]candidate, 0, len(snap))
	for sha, e := range snap {
		// Protect recently-served files from eviction so an in-flight
		// download isn't yanked out from under us.
		if now.Sub(e.LastServedAt) < EvictionProtectionWindow {
			continue
		}
		cands = append(cands, candidate{sha: sha, entry: e, count: counts[e.BookID]})
	}
	sort.Slice(cands, func(i, j int) bool {
		// Higher holder count = more droppable. Ties broken by older
		// LastServedAt = more droppable.
		if cands[i].count != cands[j].count {
			return cands[i].count > cands[j].count
		}
		return cands[i].entry.LastServedAt.Before(cands[j].entry.LastServedAt)
	})

	var freed int64
	for _, c := range cands {
		if total-freed <= capBytes {
			break
		}
		// Try to locate the file on disk by sniffing for known extensions.
		// We don't know the extension from the index because we may add
		// other formats over time, but we DO know the fanout pattern.
		path, found := findFanoutFile(mirrorRoot, c.sha)
		if !found {
			// File already gone from disk — index is stale. Drop it.
			idx.Remove(c.sha)
			continue
		}
		if err := os.Remove(path); err != nil {
			log.Printf("mirror: evict %s: %v", c.sha, err)
			continue
		}
		idx.Remove(c.sha)
		freed += c.entry.SizeBytes
		log.Printf("mirror: evicted %s (book=%s, holders=%d, size=%d)", c.sha, c.entry.BookID, c.count, c.entry.SizeBytes)
	}
	if err := idx.Save(); err != nil {
		// Index save failure is annoying but not fatal — next call
		// will re-evict whatever's stale.
		log.Printf("mirror: index save after evict: %v", err)
	}
	return freed, nil
}

// findFanoutFile returns the on-disk path for a hash by scanning the
// expected fanout directory for any extension. Returns the first match.
func findFanoutFile(mirrorRoot, sha string) (string, bool) {
	if len(sha) < 4 {
		return "", false
	}
	dir := filepath.Join(mirrorRoot, sha[0:2], sha[2:4])
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// The filename is "<sha>.<ext>" — match prefix to allow any ext.
		if strings.HasPrefix(name, sha+".") {
			return filepath.Join(dir, name), true
		}
	}
	return "", false
}

// FetchHolderCounts returns a HolderCountFetcher that calls Town Square's
// POST /api/books/holder-counts. Used by the manager; isolated here so
// tests can swap in a stub.
func FetchHolderCounts(townsquareURL string, client *http.Client) HolderCountFetcher {
	return func(ctx context.Context, bookIDs []string) (map[string]int, error) {
		body, err := json.Marshal(map[string]any{"book_ids": bookIDs})
		if err != nil {
			return nil, err
		}
		u := strings.TrimRight(townsquareURL, "/") + "/api/books/holder-counts"
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(string(body)))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("holder-counts http %d", resp.StatusCode)
		}
		var out struct {
			Counts map[string]int `json:"counts"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
			return nil, err
		}
		return out.Counts, nil
	}
}
