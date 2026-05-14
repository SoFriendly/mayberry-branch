package mirror

import (
	"sync"
	"time"
)

// Stats is the snapshot the dashboard renders. JSON tags match the
// /api/mirror/status response shape.
type Stats struct {
	Enabled            bool      `json:"enabled"`
	SizeUsedBytes      int64     `json:"size_used_bytes"`
	SizeCapBytes       int64     `json:"size_cap_bytes"`
	FilesCount         int       `json:"files_count"`
	SourcesCount       int       `json:"sources_count"`
	LastDownloadAt     time.Time `json:"last_download_at,omitempty"`
	LastDownloadBook   string    `json:"last_download_book,omitempty"`
	RecentEvents       []Event   `json:"recent_events"`
	LibraryMirrorPath  string    `json:"library_mirror_path,omitempty"`
	AudiobookMirrorPath string   `json:"audiobook_mirror_path,omitempty"`
}

// Event is one row of the manager's recent-activity ring buffer. Drives
// the "what is the mirror doing right now" status panel.
type Event struct {
	At             time.Time `json:"at"`
	Kind           string    `json:"kind"` // "accepted" or "rejected"
	BookID         string    `json:"book_id,omitempty"`
	SourceBranchID string    `json:"source_branch_id,omitempty"`
	Reason         string    `json:"reason,omitempty"` // for rejected
}

// eventBuffer is a small thread-safe ring of recent events. The size is
// deliberately small — the dashboard shows the last few, not the full
// history; the full history is Phase 7's audit log file.
type eventBuffer struct {
	mu     sync.Mutex
	events []Event
	max    int
}

func newEventBuffer(max int) *eventBuffer {
	return &eventBuffer{max: max}
}

func (b *eventBuffer) push(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, e)
	if len(b.events) > b.max {
		b.events = b.events[len(b.events)-b.max:]
	}
}

// snapshot returns the events in most-recent-first order.
func (b *eventBuffer) snapshot() []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Event, len(b.events))
	for i, e := range b.events {
		out[len(b.events)-1-i] = e
	}
	return out
}
