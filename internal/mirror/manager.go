package mirror

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sofriendly/mayberry/internal/config"
)

// Candidate is one row from Town Square's /api/branches/mirror-candidates.
type Candidate struct {
	BookID         string `json:"book_id"`
	ContentSHA256  string `json:"content_sha256"`
	SizeBytes      int64  `json:"size_bytes"`
	SourceBranchID string `json:"source_branch_id"`
	DownloadURL    string `json:"download_url"`
	HolderCount    int    `json:"holder_count"`
}

// Manager runs the mirror background loop. One per branch process.
// Stopping the context passed to Start() terminates the loop cleanly.
type Manager struct {
	cfg           *config.BranchConfig
	branchID      string
	libraryRoot   string // {LibraryPath}/_mirror, resolved at Start
	audiobookRoot string // {AudiobookPath}/_mirror, resolved at Start (may be "")
	townsquare    string
	httpClient    *http.Client
	libIndex      *Index             // index of the library mirror root
	abIndex       *Index             // index of the audiobook mirror root (may be nil)
	fetchHolders  HolderCountFetcher // injected for tests
	capBytes      int64              // parsed mirror_size cap, set on Start

	events    *eventBuffer
	audit     *AuditLog
	blacklist *Blacklist

	// reportCounters accumulates per-source reject counts and reasons
	// since the last submission to Town Square. The submitter goroutine
	// snapshots and clears every hour.
	reportMu       sync.Mutex
	reportCounters map[string]*sourceReport

	// purgeMu is read-locked during a normal tick and write-locked during
	// Purge(), so a user-initiated purge blocks until any in-flight
	// download finishes and then proceeds atomically.
	purgeMu sync.RWMutex
}

type sourceReport struct {
	count   int
	reasons map[string]int // reason → count, top reasons surfaced
}

// NewManager constructs a Manager from config. Returns nil if mirroring
// is disabled — the caller should treat that as "nothing to start."
func NewManager(cfg *config.BranchConfig, branchID string) *Manager {
	if cfg == nil || !cfg.MirrorNetwork {
		return nil
	}
	return &Manager{
		cfg:            cfg,
		branchID:       branchID,
		townsquare:     cfg.ServerURL,
		httpClient:     &http.Client{Timeout: 30 * time.Second},
		events:         newEventBuffer(20),
		audit:          NewAuditLog(DefaultAuditPath()),
		blacklist:      NewBlacklist(),
		reportCounters: make(map[string]*sourceReport),
	}
}

// Start launches the background goroutine. Returns immediately. The
// goroutine runs until ctx is canceled.
func (m *Manager) Start(ctx context.Context) {
	go m.run(ctx)
}

func (m *Manager) run(ctx context.Context) {
	// Resolve mirror roots upfront. A failure here disables mirroring
	// for this run; the user fixes their config and restarts.
	libRoot, err := EnsureMirrorRoot(m.cfg.LibraryPath)
	if err != nil {
		log.Printf("mirror: disabling — %v", err)
		return
	}
	m.libraryRoot = libRoot
	idx, err := LoadIndex(libRoot)
	if err != nil {
		log.Printf("mirror: index load failed, starting empty: %v", err)
		idx = &Index{path: filepath.Join(libRoot, IndexFileName), entries: map[string]IndexEntry{}}
	}
	m.libIndex = idx
	if m.cfg.AudiobookPath != "" {
		abRoot, err := EnsureMirrorRoot(m.cfg.AudiobookPath)
		if err != nil {
			log.Printf("mirror: audiobook root unusable, audiobook mirroring off — %v", err)
		} else {
			m.audiobookRoot = abRoot
			if abIdx, err := LoadIndex(abRoot); err == nil {
				m.abIndex = abIdx
			}
		}
	}

	// Parse mirror_size now so per-source quota and eviction agree on a
	// single number for this run.
	if n, err := config.ParseSize(m.cfg.MirrorSize); err == nil {
		m.capBytes = n
	} else {
		// Fall back to default — the settings validator should have
		// caught this earlier, but we don't want a typo to disable the
		// safety cap entirely.
		m.capBytes, _ = config.ParseSize(config.DefaultMirrorSize)
	}

	if m.fetchHolders == nil {
		m.fetchHolders = FetchHolderCounts(m.townsquare, m.httpClient)
	}

	preset := ratePreset(m.cfg.MirrorRate)
	bandwidth := preset.bandwidthBps

	// Spawn the periodic report submitter — it sends accumulated reject
	// counts per source to Town Square once an hour. Independent of the
	// download tick rate so reports go out even during quiet periods.
	go m.runReportSubmitter(ctx)

	// Startup jitter: 5–30 minutes, so a fleet-wide restart (e.g. after
	// auto-update) doesn't all hit Town Square at once.
	jitter := time.Duration(5+rand.Intn(26)) * time.Minute
	log.Printf("mirror: starting after %s (rate=%s, lib=%s)", jitter, m.cfg.MirrorRate, m.libraryRoot)
	select {
	case <-ctx.Done():
		return
	case <-time.After(jitter):
	}

	for {
		if ctx.Err() != nil {
			return
		}
		if err := m.tick(ctx, bandwidth); err != nil {
			log.Printf("mirror: tick: %v", err)
		}
		// Sleep a randomized interval inside the preset's range before
		// the next attempt. Randomization defeats sync-up across branches.
		wait := preset.randomInterval()
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// tick fetches up to N candidates and processes one. Processing one at
// a time (rather than draining the list) matches the design intent:
// slow and steady, with rate intervals enforced between every download.
func (m *Manager) tick(ctx context.Context, bandwidth int64) error {
	// Hold a read lock for the entire tick so Purge() (a write lock)
	// waits for in-flight downloads to complete before wiping files.
	m.purgeMu.RLock()
	defer m.purgeMu.RUnlock()

	cands, err := m.fetchCandidates(ctx)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	if len(cands) == 0 {
		return nil
	}
	// Walk candidates until we find one not blacklisted. Pollution
	// attackers might dominate the rarest-first list with junk; the
	// blacklist filter lets us skip them and reach a legitimate
	// candidate further down.
	var picked *Candidate
	for i := range cands {
		if m.blacklist != nil && m.blacklist.IsBlocked(cands[i].SourceBranchID) {
			continue
		}
		picked = &cands[i]
		break
	}
	if picked == nil {
		return nil
	}
	cand := *picked
	if err := m.processCandidate(ctx, cand, bandwidth); err != nil {
		m.recordReject(cand, err.Error())
		return err
	}
	m.recordAccept(cand)
	return nil
}

// recordReject fans the rejection out to dashboard events, the audit
// log file, the blacklist counter, and the periodic report aggregator.
// One call site, one place to keep these in sync.
func (m *Manager) recordReject(c Candidate, reason string) {
	ev := Event{
		At:             LogTime(),
		Kind:           "rejected",
		BookID:         c.BookID,
		SourceBranchID: c.SourceBranchID,
		Reason:         reason,
	}
	m.events.push(ev)
	if m.audit != nil {
		_ = m.audit.Write(ev)
	}
	if m.blacklist != nil && m.blacklist.RecordReject(c.SourceBranchID) {
		log.Printf("mirror: blacklisting source %s for %s after %d consecutive rejects",
			c.SourceBranchID, BlacklistDuration, BlacklistThreshold)
	}
	// Accumulate for the next Town Square report.
	m.reportMu.Lock()
	r, ok := m.reportCounters[c.SourceBranchID]
	if !ok {
		r = &sourceReport{reasons: make(map[string]int)}
		m.reportCounters[c.SourceBranchID] = r
	}
	r.count++
	if reason != "" {
		r.reasons[reason]++
	}
	m.reportMu.Unlock()
}

// recordAccept mirrors recordReject for the success path. Resets the
// per-source consecutive-reject count and logs to the audit file.
func (m *Manager) recordAccept(c Candidate) {
	ev := Event{
		At:             LogTime(),
		Kind:           "accepted",
		BookID:         c.BookID,
		SourceBranchID: c.SourceBranchID,
	}
	// The dashboard event is already pushed by processCandidate at the
	// success path; the audit log captures the persistent record.
	if m.audit != nil {
		_ = m.audit.Write(ev)
	}
	if m.blacklist != nil {
		m.blacklist.RecordAccept(c.SourceBranchID)
	}
}

// fetchCandidates calls Town Square's /api/branches/mirror-candidates
// with the branch's allow/ignore preferences.
func (m *Manager) fetchCandidates(ctx context.Context) ([]Candidate, error) {
	q := url.Values{}
	if m.branchID != "" {
		q.Set("branch_id", m.branchID)
	}
	if len(m.cfg.MirrorOnly) > 0 {
		q.Set("only", strings.Join(m.cfg.MirrorOnly, ","))
	}
	if len(m.cfg.MirrorIgnore) > 0 {
		q.Set("ignore", strings.Join(m.cfg.MirrorIgnore, ","))
	}
	q.Set("limit", "20") // small batch — we only act on one per tick anyway
	u := strings.TrimRight(m.townsquare, "/") + "/api/branches/mirror-candidates?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var body struct {
		Candidates []Candidate `json:"candidates"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return body.Candidates, nil
}

// processCandidate runs the full quarantine → validate → promote
// pipeline for one candidate. Any error here is logged but does not
// stop the manager; the next tick tries a different candidate.
func (m *Manager) processCandidate(ctx context.Context, c Candidate, bandwidth int64) error {
	// 1. Size pre-check against the largest cap we might enforce. We
	//    don't know the kind yet — that's determined by magic bytes
	//    after download — so use the audiobook cap as the upper bound
	//    and let per-kind logic re-check after sniffing.
	if c.SizeBytes <= 0 {
		return fmt.Errorf("reject %s: zero size advertised", c.BookID)
	}
	if c.SizeBytes > MaxAudiobookBytes {
		return fmt.Errorf("reject %s: advertised size %d exceeds caps", c.BookID, c.SizeBytes)
	}

	// 1a. Per-source quota: no single source may occupy more than
	//     PerSourceFraction of the mirror cap. Defends against a
	//     pollution attacker spinning up one branch with thousands of
	//     rare-looking books and monopolizing the eviction queue.
	if m.libIndex != nil && m.capBytes > 0 {
		sourceUsed := m.libIndex.SourceSize(c.SourceBranchID)
		if m.abIndex != nil {
			sourceUsed += m.abIndex.SourceSize(c.SourceBranchID)
		}
		quota := int64(float64(m.capBytes) * PerSourceFraction)
		if sourceUsed+c.SizeBytes > quota {
			return fmt.Errorf("reject %s: source %s over quota (%d+%d > %d)",
				c.BookID, c.SourceBranchID, sourceUsed, c.SizeBytes, quota)
		}
	}

	// 1b. Mirror-size cap: if we're already over, run eviction first
	//     and bail if it didn't make enough room. Eviction can fail to
	//     free space if everything is recently-served — that's normal
	//     transient backpressure; we just skip this tick.
	if m.libIndex != nil && m.capBytes > 0 {
		total := m.libIndex.TotalSize()
		if m.abIndex != nil {
			total += m.abIndex.TotalSize()
		}
		if total+c.SizeBytes > m.capBytes {
			_, _ = Evict(ctx, m.libIndex, m.libraryRoot, m.capBytes-c.SizeBytes, m.fetchHolders)
			total = m.libIndex.TotalSize()
			if m.abIndex != nil {
				total += m.abIndex.TotalSize()
			}
			if total+c.SizeBytes > m.capBytes {
				return fmt.Errorf("reject %s: at cap and eviction couldn't free enough", c.BookID)
			}
		}
	}

	// 2. Open staging. Cleanup runs on every exit path so failed
	//    downloads never leak bytes into the user's library tree.
	f, stagingPath, cleanup, err := StageFile(m.libraryRoot)
	if err != nil {
		return fmt.Errorf("stage: %w", err)
	}
	defer cleanup()

	// 3. Stream the download, computing hash + enforcing cap. We use
	//    the audiobook cap here too; the per-kind cap is rechecked
	//    against the actual sniffed kind below.
	result, err := Download(ctx, c.DownloadURL, MaxAudiobookBytes, bandwidth, f)
	closeErr := f.Close()
	if err != nil {
		return fmt.Errorf("download %s: %w", c.BookID, err)
	}
	if closeErr != nil {
		return fmt.Errorf("staging close: %w", closeErr)
	}

	// 4. Hash verification: the SHA-256 we computed locally must match
	//    what Town Square advertised. This catches in-flight tampering
	//    AND a source that lied about its own hash via the sync API.
	if !strings.EqualFold(result.SHA256, c.ContentSHA256) {
		return fmt.Errorf("reject %s: hash mismatch (got=%s announced=%s)", c.BookID, result.SHA256, c.ContentSHA256)
	}

	// 5. Sniff magic bytes to identify the format. This is the ONLY
	//    thing that decides where the file lands and which validator
	//    runs — we never trust any source-supplied extension.
	kind, err := SniffKind(stagingPath)
	if err != nil {
		return fmt.Errorf("sniff %s: %w", c.BookID, err)
	}
	if kind == KindUnknown {
		return fmt.Errorf("reject %s: unknown file kind (no magic-byte match)", c.BookID)
	}

	// 6. Per-kind size cap enforcement. The download cap above was
	//    permissive; here we enforce the tighter cap appropriate to
	//    the actual sniffed format.
	maxBytes := perKindCap(kind)
	if result.Size > maxBytes {
		return fmt.Errorf("reject %s: %s size %d > cap %d", c.BookID, kind, result.Size, maxBytes)
	}

	// 7. Per-format structural validation. Catches zip bombs,
	//    path-traversal entries, malformed XML, etc. before we ever
	//    expose the bytes to the scanner or downstream readers.
	if err := Validate(stagingPath, kind, maxBytes); err != nil {
		return fmt.Errorf("reject %s: %w", c.BookID, err)
	}

	// 8. Pick the destination root by sniffed format, not by source
	//    claim. EPUBs go to libraryRoot; M4Bs would go to audiobookRoot
	//    (when M4B validation lands).
	dstRoot := m.libraryRoot
	if kind == KindM4B {
		if m.audiobookRoot == "" {
			return fmt.Errorf("reject %s: no audiobook dir configured", c.BookID)
		}
		dstRoot = m.audiobookRoot
	}

	finalPath, err := MirrorPath(dstRoot, result.SHA256, kind.Ext())
	if err != nil {
		return fmt.Errorf("path %s: %w", c.BookID, err)
	}
	if err := Promote(dstRoot, stagingPath, finalPath); err != nil {
		return fmt.Errorf("promote %s: %w", c.BookID, err)
	}

	// Record in the index so eviction can rank by served-recency and
	// per-source quota math sees this file.
	idx := m.libIndex
	if kind == KindM4B {
		idx = m.abIndex
	}
	if idx != nil {
		idx.Add(result.SHA256, IndexEntry{
			SizeBytes:      result.Size,
			SourceBranchID: c.SourceBranchID,
			BookID:         c.BookID,
		})
		if err := idx.Save(); err != nil {
			log.Printf("mirror: index save after accept: %v", err)
		}
	}

	// Dashboard status panel: record the accept event.
	m.events.push(Event{
		At:             time.Now().UTC(),
		Kind:           "accepted",
		BookID:         c.BookID,
		SourceBranchID: c.SourceBranchID,
	})

	log.Printf("mirror: accepted %s (%s, %d bytes) → %s", c.BookID, kind, result.Size, finalPath)
	return nil
}

// Stats returns a snapshot of the mirror's current state for the
// dashboard. Safe to call from any goroutine; never blocks on the
// download loop.
func (m *Manager) Stats() Stats {
	s := Stats{
		Enabled:             m.cfg != nil && m.cfg.MirrorNetwork,
		SizeCapBytes:        m.capBytes,
		LibraryMirrorPath:   m.libraryRoot,
		AudiobookMirrorPath: m.audiobookRoot,
	}
	if m.events != nil {
		s.RecentEvents = m.events.snapshot()
	}
	sources := make(map[string]bool)
	collect := func(idx *Index) {
		if idx == nil {
			return
		}
		snap := idx.Snapshot()
		s.FilesCount += len(snap)
		for _, e := range snap {
			s.SizeUsedBytes += e.SizeBytes
			if e.SourceBranchID != "" {
				sources[e.SourceBranchID] = true
			}
			if e.AddedAt.After(s.LastDownloadAt) {
				s.LastDownloadAt = e.AddedAt
				s.LastDownloadBook = e.BookID
			}
		}
	}
	collect(m.libIndex)
	collect(m.abIndex)
	s.SourcesCount = len(sources)
	return s
}

// Purge wipes every mirrored file under both mirror roots and clears the
// indexes. Blocks until any in-flight download completes (via purgeMu),
// then runs atomically. Returns first error encountered but continues
// best-effort so a partial failure doesn't leave half-cleaned roots.
func (m *Manager) Purge() error {
	m.purgeMu.Lock()
	defer m.purgeMu.Unlock()

	var firstErr error
	if m.libraryRoot != "" {
		if err := purgeRoot(m.libraryRoot); err != nil && firstErr == nil {
			firstErr = err
		}
		if m.libIndex != nil {
			m.libIndex.Clear()
			if err := m.libIndex.Save(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	if m.audiobookRoot != "" {
		if err := purgeRoot(m.audiobookRoot); err != nil && firstErr == nil {
			firstErr = err
		}
		if m.abIndex != nil {
			m.abIndex.Clear()
			if err := m.abIndex.Save(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	// Record the purge in the event ring so it shows up in the
	// dashboard's recent activity log.
	if m.events != nil {
		m.events.push(Event{
			At:     time.Now().UTC(),
			Kind:   "rejected",
			Reason: "manual purge",
		})
	}
	return firstErr
}

// purgeRoot removes every fanout dir and the staging dir under
// mirrorRoot, but leaves the mirror root itself plus the index file
// behind (the index file is overwritten with empty contents by the
// caller's idx.Save()).
func purgeRoot(mirrorRoot string) error {
	entries, err := os.ReadDir(mirrorRoot)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if name == IndexFileName {
			continue
		}
		if err := os.RemoveAll(filepath.Join(mirrorRoot, name)); err != nil {
			return err
		}
	}
	return nil
}

// runReportSubmitter periodically POSTs accumulated reject counts to
// Town Square. The data feeds the cross-network admin view that flags
// pollution attacks in progress. Reports are best-effort; a failed
// submission keeps the counters so the next attempt can carry them.
func (m *Manager) runReportSubmitter(ctx context.Context) {
	const interval = 1 * time.Hour
	// Initial delay so a fleet-wide restart doesn't dogpile TS at boot.
	first := time.Duration(10+rand.Intn(50)) * time.Minute
	select {
	case <-ctx.Done():
		return
	case <-time.After(first):
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := m.submitReport(ctx); err != nil {
			log.Printf("mirror: report submit failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// submitReport snapshots the reject counters and POSTs them. On
// success the counters are cleared; on failure they're retained so the
// next interval can ship them.
func (m *Manager) submitReport(ctx context.Context) error {
	m.reportMu.Lock()
	if len(m.reportCounters) == 0 {
		m.reportMu.Unlock()
		return nil
	}
	type sourceItem struct {
		SourceBranchID string   `json:"source_branch_id"`
		RejectCount    int      `json:"reject_count"`
		Reasons        []string `json:"reasons"`
	}
	items := make([]sourceItem, 0, len(m.reportCounters))
	for src, r := range m.reportCounters {
		// Pick the top few reasons (arbitrary cap so the payload stays
		// small even if a source reject-storms us with diverse errors).
		reasons := make([]string, 0, len(r.reasons))
		for reason := range r.reasons {
			reasons = append(reasons, reason)
		}
		if len(reasons) > 5 {
			reasons = reasons[:5]
		}
		items = append(items, sourceItem{
			SourceBranchID: src,
			RejectCount:    r.count,
			Reasons:        reasons,
		})
	}
	m.reportMu.Unlock()

	body, err := json.Marshal(map[string]any{
		"reporter_branch_id": m.branchID,
		"sources":            items,
	})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	u := strings.TrimRight(m.townsquare, "/") + "/api/branches/mirror-reports"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("report http %d", resp.StatusCode)
	}
	// Ship succeeded — clear the counters. New rejects after this point
	// accumulate into the next interval's report.
	m.reportMu.Lock()
	m.reportCounters = make(map[string]*sourceReport)
	m.reportMu.Unlock()
	return nil
}

// OnServe is the callback the branchhttp server invokes when serving a
// file out of either mirror root. We update last_served_at in whichever
// index contains the sha — eviction uses this to skip recently-served
// files and prefer dropping cold ones.
func (m *Manager) OnServe(sha string) {
	if m.libIndex != nil {
		if _, ok := m.libIndex.Get(sha); ok {
			m.libIndex.Touch(sha)
			return
		}
	}
	if m.abIndex != nil {
		if _, ok := m.abIndex.Get(sha); ok {
			m.abIndex.Touch(sha)
		}
	}
}

// perKindCap returns the per-file size limit for a given kind. These
// constants live in download.go.
func perKindCap(k Kind) int64 {
	switch k {
	case KindEPUB:
		return MaxEPUBBytes
	case KindM4B:
		return MaxAudiobookBytes
	}
	return 0
}

// ratePreset is the resolved interval+bandwidth for a named preset.
type rateSettings struct {
	minInterval  time.Duration
	maxInterval  time.Duration
	bandwidthBps int64
}

func (r rateSettings) randomInterval() time.Duration {
	span := r.maxInterval - r.minInterval
	if span <= 0 {
		return r.minInterval
	}
	return r.minInterval + time.Duration(rand.Int63n(int64(span)))
}

func ratePreset(name string) rateSettings {
	switch name {
	case "fast":
		return rateSettings{1 * time.Minute, 3 * time.Minute, 5 * 1024 * 1024}
	case "normal":
		return rateSettings{5 * time.Minute, 10 * time.Minute, 1024 * 1024}
	}
	// slow (and any unknown value) — matches MIRROR.md defaults.
	return rateSettings{10 * time.Minute, 15 * time.Minute, 500 * 1024}
}

