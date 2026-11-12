package mirror

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// permanentReject marks a rejection that is deterministic for the exact
// announcement (book, source, announced hash): the same candidate will
// fail the same way every time, so retrying is pure waste until the
// source re-syncs a different hash. Observed in the field as the same
// hash-mismatched file being re-downloaded every tick for months —
// the per-source blacklist never tripped because accepts from the same
// source kept resetting its consecutive-reject counter.
type permanentReject struct{ msg string }

func (e permanentReject) Error() string { return e.msg }

// permrejectf builds a permanentReject the way fmt.Errorf would.
func permrejectf(format string, a ...any) error {
	return permanentReject{msg: fmt.Sprintf(format, a...)}
}

// rejectRetentionDays bounds the reject memory. An entry only matters
// while the source keeps announcing the identical hash; after this long
// a single retry per window is a fine price for self-healing.
const rejectRetentionDays = 90

// RejectMemory persistently remembers permanently-rejected candidates,
// keyed by (book, source, announced hash). Keying on the announced hash
// means a source that fixes itself — re-syncing the true hash of the
// file it serves — becomes eligible again automatically, with no timer.
type RejectMemory struct {
	mu      sync.Mutex
	entries map[string]time.Time // "book|source|sha" → when rejected
	path    string               // disk location; empty disables persistence
}

func rejectKey(c Candidate) string {
	return c.BookID + "|" + c.SourceBranchID + "|" + c.ContentSHA256
}

// NewRejectMemory loads the persisted memory at path (empty path = memory
// only). Corrupt or missing files start empty; entries past retention are
// dropped on load.
func NewRejectMemory(path string) *RejectMemory {
	m := &RejectMemory{entries: make(map[string]time.Time), path: path}
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			var entries map[string]time.Time
			if json.Unmarshal(data, &entries) == nil {
				cutoff := time.Now().AddDate(0, 0, -rejectRetentionDays)
				for k, t := range entries {
					if t.After(cutoff) {
						m.entries[k] = t
					}
				}
			}
		}
	}
	return m
}

// Skip reports whether this exact candidate was already permanently
// rejected and should not be retried.
func (m *RejectMemory) Skip(c Candidate) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.entries[rejectKey(c)]
	return ok
}

// Record remembers a permanent reject and persists the memory. Failures
// to persist are ignored — worst case the daemon retries once after a
// restart.
func (m *RejectMemory) Record(c Candidate) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[rejectKey(c)] = time.Now()
	if m.path == "" {
		return
	}
	data, err := json.Marshal(m.entries)
	if err != nil {
		return
	}
	_ = os.WriteFile(m.path, data, 0600)
}

// Len reports how many candidates are currently remembered (for the
// dashboard/stats).
func (m *RejectMemory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}

// DefaultRejectsPath returns the canonical reject-memory location,
// alongside the branch config and audit log.
func DefaultRejectsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "mayberry-mirror-rejects.json"
	}
	return filepath.Join(home, ".mayberry", "mirror-rejects.json")
}
