package mirror

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// AuditMaxBytes is the rotation threshold. When the live log grows past
// this, we rename it to .1 (overwriting any prior .1) and start fresh.
// 5 MB is enough to retain ~50k events at typical line lengths — far more
// than a Phase 7 incident response would need to scroll through.
const AuditMaxBytes = 5 * 1024 * 1024

// AuditLog appends mirror events to an on-disk JSON-lines file. It's
// the persistent record for incident response — when something goes
// wrong, the operator reads this rather than scrolling daemon stdout.
//
// One process owns the file; concurrent goroutines serialize through
// the log's mutex. We open-append-close per write so a kill -9 doesn't
// leave bytes buffered.
type AuditLog struct {
	path string
	mu   sync.Mutex
}

// NewAuditLog opens (or creates) the audit log at path. The directory
// must already exist; LoadIndex etc. will have created ~/.mayberry first.
func NewAuditLog(path string) *AuditLog {
	return &AuditLog{path: path}
}

// Write appends one event. Failures are not propagated — a broken audit
// log shouldn't take down the mirror loop. The error is returned for
// the rare caller that wants to verify (tests, admin endpoints).
func (a *AuditLog) Write(e Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("audit marshal: %w", err)
	}
	line = append(line, '\n')

	// Check size before write; rotate if we would exceed cap. Rotate
	// before write so the brand-new file gets the line and the .1 file
	// holds a clean snapshot.
	if info, err := os.Stat(a.path); err == nil && info.Size()+int64(len(line)) > AuditMaxBytes {
		if err := a.rotateLocked(); err != nil {
			return err
		}
	}

	f, err := os.OpenFile(a.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("audit open: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("audit write: %w", err)
	}
	return nil
}

// rotateLocked moves the current log to <path>.1 (overwriting the prior
// rotation). Caller must hold a.mu.
func (a *AuditLog) rotateLocked() error {
	rotated := a.path + ".1"
	_ = os.Remove(rotated)
	if err := os.Rename(a.path, rotated); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("audit rotate: %w", err)
	}
	return nil
}

// DefaultAuditPath returns the canonical audit log location, alongside
// the branch config. The caller should ensure the parent dir exists.
func DefaultAuditPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "mayberry-mirror-audit.log"
	}
	return filepath.Join(home, ".mayberry", "mirror-audit.log")
}

// LogTime returns time.Now in UTC. Wrapped so tests can substitute.
var LogTime = func() time.Time { return time.Now().UTC() }
