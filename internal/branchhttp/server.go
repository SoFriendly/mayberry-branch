package branchhttp

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sofriendly/mayberry/internal/audiobook"
	"github.com/sofriendly/mayberry/internal/auth"
	"github.com/sofriendly/mayberry/internal/config"
	"github.com/sofriendly/mayberry/internal/epub"
	"github.com/sofriendly/mayberry/internal/mirror"
)

// hashFilenameRe matches a 64-character lowercase hex filename — what the
// mirror manager produces. The audiobook scanner falls back to filename
// for titles when no embedded metadata is present; we MUST NOT surface
// raw SHA-256s as titles, so we skip the fallback when this matches.
var hashFilenameRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// hashCache memoizes file SHA-256 + size, keyed by absolute path and
// invalidated when size or mtime change. Avoids rehashing every scan tick.
type hashCache struct {
	mu      sync.Mutex
	entries map[string]hashEntry
}

type hashEntry struct {
	size  int64
	mtime time.Time
	hash  string
}

func newHashCache() *hashCache {
	return &hashCache{entries: make(map[string]hashEntry)}
}

// GetOrCompute returns the file's size and hex SHA-256. If a cached entry
// matches the current size + mtime, the cached hash is returned without
// re-reading the file.
func (c *hashCache) GetOrCompute(path string) (int64, string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, "", err
	}
	size := info.Size()
	mtime := info.ModTime()

	c.mu.Lock()
	e, ok := c.entries[path]
	c.mu.Unlock()
	if ok && e.size == size && e.mtime.Equal(mtime) {
		return size, e.hash, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return 0, "", err
	}
	hash := hex.EncodeToString(h.Sum(nil))

	c.mu.Lock()
	c.entries[path] = hashEntry{size: size, mtime: mtime, hash: hash}
	c.mu.Unlock()
	return size, hash, nil
}

// SetupCallback is called after the user completes the web setup wizard.
// It receives the updated config so the caller can trigger a rescan.
type SetupCallback func(cfg *config.BranchConfig)

// RestartCallback is called when the user clicks the "Restart" button after
// changing settings. The host process is expected to schedule a daemon restart
// and exit promptly.
type RestartCallback func()

// SyncCallback is called when the user (via dashboard or `mayberry sync`)
// requests an immediate scan + Town Square sync, bypassing the watcher poll.
type SyncCallback func()

// MirrorServeCallback is called after we successfully begin serving a
// file out of our _mirror/ subfolder to a peer mirror client. The sha
// argument is the file's content hash (its filename minus extension).
// Used by the mirror manager to update last_served_at, which informs
// eviction ranking.
type MirrorServeCallback func(sha string)

// MirrorStatsFn lets the dashboard endpoints render mirror status without
// importing the mirror package directly into branchhttp's HTTP code path.
// Returns nil-ish (zero-value Stats) when mirroring is disabled.
type MirrorStatsFn func() mirror.Stats

// MirrorPurgeFn deletes all mirrored content. Blocks until in-flight
// downloads complete.
type MirrorPurgeFn func() error

// Server is the Branch local HTTP server providing the dashboard and download endpoint.
type Server struct {
	mux        *http.ServeMux
	branchID   string
	libraryDir string
	publicKey  ed25519.PublicKey
	coverDir   string // cached cover images
	hashes     *hashCache

	// Mirror-serve throttling: size-1 semaphore so we only serve one
	// mirror request at a time, plus a counter of in-flight real
	// downloads so we can prioritize real users over mirrors.
	mirrorServeSlots chan struct{}
	realDownloads    atomic.Int32

	mu       sync.RWMutex
	catalog  []CatalogEntry // current epub catalog
	holdings map[string]string // isbn -> filepath

	cfg            *config.BranchConfig
	onSetup        SetupCallback
	onRestart      RestartCallback
	onSync         SyncCallback
	onMirrorServe  MirrorServeCallback
	onMirrorStats  MirrorStatsFn
	onMirrorPurge  MirrorPurgeFn
}

// CatalogEntry is a scanned epub with its metadata and path.
type CatalogEntry struct {
	Path     string `json:"path"`
	Title    string `json:"title"`
	Author   string `json:"author"`
	ISBN     string `json:"isbn"`
	ID       string `json:"id"` // bookID — ISBN or MB+hash
	HasCover bool   `json:"has_cover"`
}

// NewServer creates the Branch local HTTP server.
func NewServer(branchID, libraryDir string) *Server {
	home, _ := os.UserHomeDir()
	coverDir := filepath.Join(home, ".mayberry", "covers")
	os.MkdirAll(coverDir, 0755)

	s := &Server{
		mux:              http.NewServeMux(),
		branchID:         branchID,
		libraryDir:       libraryDir,
		holdings:         make(map[string]string),
		coverDir:         coverDir,
		hashes:           newHashCache(),
		mirrorServeSlots: make(chan struct{}, 1),
	}
	s.routes()
	return s
}

// SetConfig attaches the branch config for setup wizard detection.
func (s *Server) SetConfig(cfg *config.BranchConfig) {
	s.cfg = cfg
}

// SetSetupCallback sets the function called when web setup completes.
func (s *Server) SetSetupCallback(cb SetupCallback) {
	s.onSetup = cb
}

// SetRestartCallback sets the function called when the user requests a
// daemon restart from the settings UI.
func (s *Server) SetRestartCallback(cb RestartCallback) {
	s.onRestart = cb
}

// SetSyncCallback sets the function called when the user requests an
// immediate library scan + Town Square sync.
func (s *Server) SetSyncCallback(cb SyncCallback) {
	s.onSync = cb
}

// SetMirrorServeCallback sets the function called when we serve a file
// out of our _mirror/ subfolder.
func (s *Server) SetMirrorServeCallback(cb MirrorServeCallback) {
	s.onMirrorServe = cb
}

// SetMirrorStatsFn registers the function that reports live mirror
// status to the dashboard.
func (s *Server) SetMirrorStatsFn(fn MirrorStatsFn) {
	s.onMirrorStats = fn
}

// SetMirrorPurgeFn registers the function that wipes mirrored content
// when the user clicks "Purge mirror" in the dashboard.
func (s *Server) SetMirrorPurgeFn(fn MirrorPurgeFn) {
	s.onMirrorPurge = fn
}

// SetPublicKey sets the Town Square public key for JWT verification.
// CoverDir returns the directory where extracted cover images are cached.
func (s *Server) CoverDir() string {
	return s.coverDir
}

func (s *Server) SetPublicKey(pk ed25519.PublicKey) {
	s.publicKey = pk
}

// BookMeta contains ISBN and EPUB-extracted metadata for sync.
type BookMeta struct {
	ISBN            string   `json:"isbn"`
	Title           string   `json:"title,omitempty"`
	Author          string   `json:"author,omitempty"`
	PublishedDate   string   `json:"published_date,omitempty"`
	Categories      []string `json:"categories,omitempty"`
	MediaType       string   `json:"media_type,omitempty"`       // "ebook" or "audiobook"
	Narrator        string   `json:"narrator,omitempty"`         // audiobook only
	DurationSeconds int      `json:"duration_seconds,omitempty"` // audiobook only
	FileExt         string   `json:"file_ext,omitempty"`         // ".epub", ".m4b"
	ASIN            string   `json:"asin,omitempty"`             // audiobook only

	// Used by the network mirror feature: content_sha256 lets a mirror branch
	// verify it received untampered bytes; size_bytes lets a mirror client
	// reject oversize files before starting a download.
	ContentSHA256 string `json:"content_sha256,omitempty"`
	SizeBytes     int64  `json:"size_bytes,omitempty"`

	// IsMirror is true when this branch holds the book because it mirrored
	// the file from another branch (rather than the user adding it directly
	// to their library). Town Square uses this to prefer originals during
	// download routing.
	IsMirror bool `json:"is_mirror,omitempty"`
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// bookID returns the ISBN if available, otherwise a content hash of
// title+author (plus media type for audiobooks). Ebook IDs are unchanged for
// backward compatibility — only audiobook IDs get the extra suffix so that a
// user owning the same title as both .epub and .m4b produces two distinct
// catalog rows instead of colliding under one ebook record.
func bookID(isbn, title, author, mediaType string) string {
	if isbn != "" {
		return isbn
	}
	payload := strings.ToLower(title + "\x00" + author)
	if mediaType == "audiobook" {
		payload += "\x00audiobook"
	}
	h := sha256.Sum256([]byte(payload))
	return "MB" + hex.EncodeToString(h[:6]) // e.g. "MB1a2b3c4d5e6f"
}

// UpdateCatalog replaces the current catalog with newly scanned book files.
// Both .epub (ebook) and .m4b (audiobook) paths are accepted. Returns metadata
// for all titles (for sync to Town Square).
func (s *Server) UpdateCatalog(bookPaths []string) []BookMeta {
	var entries []CatalogEntry
	holdings := make(map[string]string)
	var books []BookMeta
	audiobookCount := 0

	for _, p := range bookPaths {
		ext := strings.ToLower(filepath.Ext(p))
		var (
			title, author, isbn, pubDate, coverType string
			categories                              []string
			coverData                               []byte
			narrator, asin                          string
			durationSecs                            int
			mediaType                               = "ebook"
		)

		switch ext {
		case ".epub":
			meta, err := func() (m epub.Metadata, err error) {
				defer func() {
					if r := recover(); r != nil {
						err = fmt.Errorf("panic: %v", r)
					}
				}()
				return epub.ExtractMetadata(p)
			}()
			if err != nil {
				log.Printf("branch: skipping %s: %v", filepath.Base(p), err)
				continue
			}
			title, author, isbn, pubDate = meta.Title, meta.Author, meta.ISBN, meta.PublishedDate
			categories = meta.Subjects
			coverData, coverType = meta.CoverData, meta.CoverType
		case ".m4b":
			meta, err := func() (m audiobook.Metadata, err error) {
				defer func() {
					if r := recover(); r != nil {
						err = fmt.Errorf("panic: %v", r)
					}
				}()
				return audiobook.ExtractMetadata(p)
			}()
			if err != nil {
				log.Printf("branch: skipping %s: %v", filepath.Base(p), err)
				continue
			}
			title, author = meta.Title, meta.Author
			if title == "" {
				base := strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
				// Mirrored audiobooks are named by SHA-256; surfacing that
				// as a title would be ugly and useless. The audiobook gets
				// skipped below because title stays empty.
				if !hashFilenameRe.MatchString(base) {
					title = base
				}
			}
			narrator = meta.Narrator
			pubDate = meta.Year
			categories = meta.Genres
			coverData, coverType = meta.CoverData, meta.CoverType
			durationSecs = meta.DurationSeconds
			asin = meta.ASIN
			mediaType = "audiobook"
			// iTunes ©day is usually a bare year ("2025"); PostgreSQL's ::date
			// cast on Town Square rejects that. Expand to YYYY-01-01 so the
			// sync upsert succeeds.
			if len(pubDate) == 4 && isAllDigits(pubDate) {
				pubDate = pubDate + "-01-01"
			}
			audiobookCount++
		default:
			continue
		}

		id := bookID(isbn, title, author, mediaType)

		hasCover := false
		if len(coverData) > 0 {
			coverExt := ".jpg"
			if strings.Contains(coverType, "png") {
				coverExt = ".png"
			}
			coverPath := filepath.Join(s.coverDir, id+coverExt)
			if err := os.WriteFile(coverPath, coverData, 0644); err == nil {
				hasCover = true
			}
		}

		// Compute (or fetch cached) SHA-256 + size. A failure here is
		// non-fatal: the catalog entry still lands without hash, sync just
		// won't carry mirror-eligibility data until the next successful pass.
		size, sha, err := s.hashes.GetOrCompute(p)
		if err != nil {
			log.Printf("branch: hash failed for %s: %v", filepath.Base(p), err)
		}

		entry := CatalogEntry{
			Path:     p,
			Title:    title,
			Author:   author,
			ISBN:     isbn,
			ID:       id,
			HasCover: hasCover,
		}
		entries = append(entries, entry)
		if title != "" {
			holdings[id] = p
			books = append(books, BookMeta{
				ISBN:            id,
				Title:           title,
				Author:          author,
				PublishedDate:   pubDate,
				Categories:      categories,
				MediaType:       mediaType,
				Narrator:        narrator,
				DurationSeconds: durationSecs,
				FileExt:         ext,
				ASIN:            asin,
				ContentSHA256:   sha,
				SizeBytes:       size,
				IsMirror:        mirror.IsMirrorPath(p),
			})
		}
	}

	s.mu.Lock()
	s.catalog = entries
	s.holdings = holdings
	s.mu.Unlock()

	log.Printf("branch: catalog updated — %d total, %d audiobook(s)", len(entries), audiobookCount)
	return books
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// localOnly wraps a handler to reject requests arriving via the public tunnel.
// The tunnel client sets X-Mayberry-Via-Tunnel on forwarded requests; the proxy
// hub strips this header from external input so it cannot be spoofed.
func localOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Mayberry-Via-Tunnel") != "" {
			http.Error(w, "This endpoint is only available on the local network", 403)
			return
		}
		next(w, r)
	}
}

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.handleDashboard)
	s.mux.HandleFunc("/settings", localOnly(s.handleSettingsPage))
	s.mux.HandleFunc("/api/catalog", localOnly(s.handleCatalog))
	s.mux.HandleFunc("/api/status", localOnly(s.handleStatus))
	s.mux.HandleFunc("/api/setup", localOnly(s.handleSetup))
	s.mux.HandleFunc("/api/restart", localOnly(s.handleRestart))
	s.mux.HandleFunc("/api/sync", localOnly(s.handleSyncNow))
	s.mux.HandleFunc("/api/browse", localOnly(s.handleBrowse))
	s.mux.HandleFunc("/api/mirror/status", localOnly(s.handleMirrorStatus))
	s.mux.HandleFunc("/api/mirror/purge", localOnly(s.handleMirrorPurge))
	s.mux.HandleFunc("/favicon.ico", s.handleFavicon)
	s.mux.HandleFunc("/covers/", s.handleLocalCover)
	s.mux.HandleFunc("/download/", s.handleDownload)
}

// needsSetup returns true if the library path is not configured or invalid.
func (s *Server) needsSetup() bool {
	if s.cfg == nil {
		return s.libraryDir == ""
	}
	path := s.cfg.LibraryPath
	if path == "" {
		return true
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return true
	}
	return false
}

// --- Dashboard ---

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	if s.needsSetup() {
		s.serveSetupWizard(w, r)
		return
	}

	s.serveDashboard(w, r)
}

func (s *Server) serveSetupWizard(w http.ResponseWriter, r *http.Request) {
	displayName := ""
	subdomain := ""
	if s.cfg != nil {
		displayName = s.cfg.DisplayName
		subdomain = s.cfg.Subdomain
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Welcome to Mayberry</title>
<style>
  *, *::before, *::after { margin: 0; padding: 0; box-sizing: border-box; }
  :root {
    --brown-900: #3B2314;
    --brown-800: #4A3728;
    --brown-600: #8A7968;
    --brown-400: #C4A882;
    --brown-300: #D4A574;
    --brown-100: #F5F1EB;
    --brown-50:  #FAF8F5;
    --green-500: #66BB6A;
    --red-500:   #EF5350;
    --shadow-sm: 0 1px 2px rgba(59,35,20,0.06);
    --shadow-md: 0 4px 12px rgba(59,35,20,0.08);
    --shadow-lg: 0 8px 30px rgba(59,35,20,0.12);
    --radius: 12px;
  }
  body {
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
    background: linear-gradient(135deg, var(--brown-100) 0%%, #E8E0D4 100%%);
    color: var(--brown-900);
    min-height: 100vh;
    display: flex;
    align-items: center;
    justify-content: center;
    padding: 2rem;
  }
  .wizard-card {
    background: white;
    border-radius: 16px;
    box-shadow: var(--shadow-lg);
    max-width: 520px;
    width: 100%%;
    padding: 2.5rem;
    animation: fadeUp 0.5s ease-out;
  }
  @keyframes fadeUp {
    from { opacity: 0; transform: translateY(20px); }
    to { opacity: 1; transform: translateY(0); }
  }
  .logo {
    font-size: 2.5rem;
    margin-bottom: 0.25rem;
  }
  .wizard-card h1 {
    font-size: 1.6rem;
    color: var(--brown-800);
    margin-bottom: 0.25rem;
    font-weight: 700;
  }
  .wizard-card .subtitle {
    color: var(--brown-600);
    font-size: 0.95rem;
    margin-bottom: 2rem;
    line-height: 1.5;
  }
  .form-group { margin-bottom: 1.5rem; }
  .form-group label {
    display: block;
    font-weight: 600;
    font-size: 0.85rem;
    color: var(--brown-800);
    margin-bottom: 0.4rem;
    letter-spacing: 0.02em;
  }
  .form-group .hint {
    font-size: 0.78rem;
    color: var(--brown-600);
    margin-bottom: 0.4rem;
  }
  .form-group input[type="text"] {
    width: 100%%;
    padding: 0.7rem 0.9rem;
    border: 1.5px solid #DDD5CA;
    border-radius: 8px;
    font-size: 0.95rem;
    color: var(--brown-900);
    background: var(--brown-50);
    transition: border-color 0.2s, box-shadow 0.2s;
    outline: none;
  }
  .form-group input[type="text"]:focus {
    border-color: var(--brown-400);
    box-shadow: 0 0 0 3px rgba(196,168,130,0.2);
  }
  .btn-primary {
    width: 100%%;
    padding: 0.8rem 1.5rem;
    background: linear-gradient(135deg, var(--brown-800), var(--brown-900));
    color: white;
    border: none;
    border-radius: 8px;
    font-size: 1rem;
    font-weight: 600;
    cursor: pointer;
    transition: transform 0.15s, box-shadow 0.15s;
    letter-spacing: 0.02em;
  }
  .btn-primary:hover { transform: translateY(-1px); box-shadow: var(--shadow-md); }
  .btn-primary:active { transform: translateY(0); }
  .btn-primary:disabled { opacity: 0.6; cursor: not-allowed; transform: none; }
  .alert { padding: 0.75rem 1rem; border-radius: 8px; font-size: 0.85rem; margin-bottom: 1rem; display: none; }
  .alert-error { background: #FFF0F0; color: var(--red-500); border: 1px solid #FFD5D5; }
  .alert-success { background: #F0FFF0; color: #2E7D32; border: 1px solid #C8E6C9; }
  .powered-by { text-align: center; margin-top: 1.5rem; font-size: 0.75rem; color: var(--brown-600); }
</style>
</head>
<body>
<div class="wizard-card">
  <div class="logo">📚</div>
  <h1>Welcome to Mayberry</h1>
  <p class="subtitle">Let's set up your Branch — your personal EPUB library node in the Mayberry network.</p>

  <div id="alert" class="alert alert-error"></div>

  <form id="setup-form" onsubmit="return submitSetup(event)">
    <div class="form-group">
      <label for="display_name">Branch Name</label>
      <div class="hint">A friendly name for your branch (e.g., "Jane's Library"). This determines your public URL.</div>
      <input type="text" id="display_name" name="display_name" placeholder="%s" value="%s">
      <div class="slug" id="slug-preview" style="font-size:0.78rem;color:#C4A882;margin-top:0.3rem;font-family:monospace">%s.branch.pub</div>
    </div>
    <div class="form-group">
      <label>EPUB Library Folder</label>
      <div class="hint">Folder containing your .epub files. Subfolders are scanned recursively.</div>
      <div id="library_path-selected" class="picker-selected" style="display:none"></div>
      <input type="hidden" id="library_path" name="library_path">
      <div id="library_path-picker" class="picker"></div>
    </div>
    <div class="form-group">
      <label>Audiobook Folder <span style="font-weight:400;color:var(--brown-600);">(optional)</span></label>
      <div class="hint">Folder containing your .m4b audiobooks. Leave blank to skip — you can add this later in Settings.</div>
      <div id="audiobook_path-selected" class="picker-selected" style="display:none"></div>
      <input type="hidden" id="audiobook_path" name="audiobook_path">
      <div id="audiobook_path-picker" class="picker"></div>
    </div>
    <button type="submit" class="btn-primary" id="submit-btn">Set Up My Branch</button>
  </form>
  <div class="powered-by">Powered by the Mayberry Network</div>
</div>
<style>
  .picker { border:1.5px solid #DDD5CA; border-radius:8px; background:var(--brown-50); max-height:260px; overflow-y:auto; }
  .picker-row { display:flex; align-items:center; padding:0.45rem 0.75rem; cursor:pointer; border-bottom:1px solid #F0EBE3; font-size:0.88rem; gap:0.5rem; }
  .picker-row:last-child { border-bottom:none; }
  .picker-row:hover { background:#EDE8DF; }
  .picker-row .icon { flex-shrink:0; width:20px; text-align:center; }
  .picker-row .name { flex:1; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; color:var(--brown-900); }
  .picker-current { padding:0.5rem 0.75rem; font-size:0.78rem; color:var(--brown-600); border-bottom:1.5px solid #DDD5CA; font-family:monospace; display:flex; align-items:center; justify-content:space-between; }
  .picker-current .select-btn { background:var(--brown-800); color:white; border:none; border-radius:6px; padding:0.3rem 0.75rem; font-size:0.75rem; cursor:pointer; font-weight:600; }
  .picker-current .select-btn:hover { background:var(--brown-900); }
  .picker-selected { padding:0.6rem 0.9rem; background:var(--brown-50); border:1.5px solid #C4A882; border-radius:8px; font-family:monospace; font-size:0.88rem; color:var(--brown-800); margin-bottom:0.5rem; display:flex; align-items:center; justify-content:space-between; }
  .picker-selected .change-btn { background:none; border:1px solid var(--brown-400); border-radius:6px; padding:0.2rem 0.6rem; font-size:0.75rem; cursor:pointer; color:var(--brown-600); }
</style>
<script>
async function loadDir(field, path) {
  var url = '/api/browse' + (path ? '?path=' + encodeURIComponent(path) : '');
  var resp = await fetch(url);
  var data = await resp.json();
  var picker = document.getElementById(field + '-picker');
  picker.innerHTML = '';
  var header = document.createElement('div');
  header.className = 'picker-current';
  var safe = data.current.replace(/\\/g, '\\\\').replace(/'/g, "\\'");
  header.innerHTML = '<span>' + data.current + '</span><button class="select-btn" onclick="selectFolder(\'' + field + '\', \'' + safe + '\')">Select This Folder</button>';
  picker.appendChild(header);
  (data.entries || []).forEach(function(e) {
    if (!e.is_dir) return;
    var row = document.createElement('div');
    row.className = 'picker-row';
    row.innerHTML = '<span class="icon">' + (e.name === '..' ? '⬆' : '📁') + '</span><span class="name">' + e.name + '</span>';
    row.onclick = function() { loadDir(field, e.path); };
    picker.appendChild(row);
  });
}
function selectFolder(field, path) {
  document.getElementById(field).value = path;
  document.getElementById(field + '-picker').style.display = 'none';
  var sel = document.getElementById(field + '-selected');
  sel.style.display = 'flex';
  var clearBtn = field === 'audiobook_path' ? '<button type="button" class="change-btn" style="margin-left:0.4rem;" onclick="clearFolder(\'' + field + '\')">Clear</button>' : '';
  sel.innerHTML = '<span>' + path + '</span><button type="button" class="change-btn" onclick="changeFolder(\'' + field + '\')">Change</button>' + clearBtn;
}
function changeFolder(field) {
  document.getElementById(field + '-picker').style.display = '';
  document.getElementById(field + '-selected').style.display = 'none';
  document.getElementById(field).value = '';
  loadDir(field, '');
}
function clearFolder(field) {
  document.getElementById(field).value = '';
  document.getElementById(field + '-picker').style.display = 'none';
  document.getElementById(field + '-selected').style.display = 'none';
}
loadDir('library_path', '');
loadDir('audiobook_path', '');

async function submitSetup(e) {
  e.preventDefault();
  var btn = document.getElementById('submit-btn');
  var alert = document.getElementById('alert');
  if (!document.getElementById('library_path').value) { alert.className='alert alert-error'; alert.textContent='Please select an EPUB library folder.'; alert.style.display='block'; return; }
  btn.disabled = true; btn.textContent = 'Setting up...'; alert.style.display = 'none';
  var body = {
    library_path: document.getElementById('library_path').value.trim(),
    audiobook_path: document.getElementById('audiobook_path').value.trim(),
    display_name: document.getElementById('display_name').value.trim()
  };
  try {
    var resp = await fetch('/api/setup', { method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(body) });
    var data = await resp.json();
    if (!resp.ok) throw new Error(data.error || 'Setup failed');
    alert.className = 'alert alert-success'; alert.textContent = 'Setup complete! Redirecting to your dashboard...'; alert.style.display = 'block';
    setTimeout(function() { window.location.reload(); }, 1500);
  } catch (err) {
    alert.className = 'alert alert-error'; alert.textContent = err.message; alert.style.display = 'block';
    btn.disabled = false; btn.textContent = 'Set Up My Branch';
  }
}
document.getElementById('display_name').addEventListener('input', function() {
  var slug = this.value.trim().toLowerCase().replace(/[^a-z0-9-]/g, '-').replace(/-+/g, '-').replace(/^-|-$/g, '');
  document.getElementById('slug-preview').textContent = (slug || '...') + '.branch.pub';
});
</script>
</body>
</html>`, displayName, displayName, subdomain)
}

func (s *Server) serveDashboard(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	bookCount := len(s.catalog)
	isbnCount := len(s.holdings)
	s.mu.RUnlock()

	branchName := s.branchID
	subdomain := ""
	if s.cfg != nil {
		branchName = s.cfg.DisplayName
		subdomain = s.cfg.Subdomain + ".branch.pub"
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s — Mayberry Branch</title>
<style>
  *, *::before, *::after { margin: 0; padding: 0; box-sizing: border-box; }
  :root {
    --brown-900: #3B2314;
    --brown-800: #4A3728;
    --brown-700: #5C4A38;
    --brown-600: #8A7968;
    --brown-400: #C4A882;
    --brown-300: #D4A574;
    --brown-200: #E8D5BC;
    --brown-100: #F5F1EB;
    --brown-50:  #FAF8F5;
    --green-500: #66BB6A;
    --shadow-sm: 0 1px 2px rgba(59,35,20,0.06);
    --shadow-md: 0 4px 12px rgba(59,35,20,0.08);
    --shadow-lg: 0 8px 30px rgba(59,35,20,0.12);
    --radius: 12px;
  }
  body {
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
    background: linear-gradient(180deg, var(--brown-100) 0%%, var(--brown-50) 100%%);
    color: var(--brown-900);
    min-height: 100vh;
  }
  .header {
    background: linear-gradient(135deg, var(--brown-800), var(--brown-900));
    color: white;
    padding: 1.5rem 2rem;
    box-shadow: var(--shadow-lg);
  }
  .header-inner {
    max-width: 960px;
    margin: 0 auto;
    display: flex;
    align-items: center;
    justify-content: space-between;
  }
  .header h1 {
    font-size: 1.4rem;
    font-weight: 700;
    display: flex;
    align-items: center;
    gap: 0.5rem;
  }
  .header .subdomain {
    font-size: 0.8rem;
    opacity: 0.7;
    font-weight: 400;
  }
  .badge {
    display: inline-flex;
    align-items: center;
    gap: 0.35rem;
    background: rgba(255,255,255,0.15);
    padding: 0.3rem 0.7rem;
    border-radius: 20px;
    font-size: 0.75rem;
    font-weight: 500;
  }
  .badge .dot { width: 8px; height: 8px; border-radius: 50%%; background: var(--green-500); animation: pulse 2s infinite; }
  @keyframes pulse { 0%%,100%% { opacity: 1; } 50%% { opacity: 0.5; } }
  .container { max-width: 960px; margin: 0 auto; padding: 1.5rem 2rem; }
  .stats-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
    gap: 1rem;
    margin-bottom: 1.5rem;
  }
  .stat-card {
    background: white;
    border-radius: var(--radius);
    padding: 1.25rem 1.5rem;
    box-shadow: var(--shadow-sm);
    border: 1px solid rgba(196,168,130,0.2);
    transition: transform 0.15s, box-shadow 0.15s;
  }
  .stat-card:hover { transform: translateY(-2px); box-shadow: var(--shadow-md); }
  .stat-card .stat-num {
    font-size: 2rem;
    font-weight: 800;
    color: var(--brown-800);
    line-height: 1;
  }
  .stat-card .stat-label {
    font-size: 0.78rem;
    color: var(--brown-600);
    font-weight: 500;
    margin-top: 0.25rem;
    text-transform: uppercase;
    letter-spacing: 0.05em;
  }
  .section { margin-bottom: 1.5rem; }
  .section-title {
    font-size: 0.85rem;
    font-weight: 700;
    color: var(--brown-600);
    text-transform: uppercase;
    letter-spacing: 0.08em;
    margin-bottom: 0.75rem;
    padding-bottom: 0.5rem;
    border-bottom: 2px solid var(--brown-200);
  }
  .book-list { list-style: none; }
  .book-item {
    background: white;
    border-radius: 10px;
    padding: 0.9rem 1.2rem;
    margin-bottom: 0.5rem;
    box-shadow: var(--shadow-sm);
    border: 1px solid rgba(196,168,130,0.15);
    display: flex;
    align-items: center;
    gap: 1rem;
    transition: transform 0.1s, box-shadow 0.1s;
  }
  .book-item:hover { transform: translateX(4px); box-shadow: var(--shadow-md); }
  .book-icon {
    width: 40px;
    height: 56px;
    background: var(--brown-100);
    border-radius: 4px;
    display: flex;
    align-items: center;
    justify-content: center;
    font-size: 1.1rem;
    flex-shrink: 0;
  }
  .book-cover {
    width: 40px;
    height: 56px;
    object-fit: cover;
    border-radius: 4px;
    flex-shrink: 0;
    box-shadow: 0 1px 3px rgba(59,35,20,0.15);
  }
  .book-info { flex: 1; min-width: 0; }
  .book-title { font-weight: 600; font-size: 0.95rem; color: var(--brown-900); }
  .book-meta { color: var(--brown-600); font-size: 0.8rem; margin-top: 0.15rem; }
  .isbn-badge {
    font-size: 0.7rem;
    background: var(--brown-100);
    color: var(--brown-700);
    padding: 0.15rem 0.5rem;
    border-radius: 4px;
    font-family: "SF Mono", "Fira Code", monospace;
    flex-shrink: 0;
  }
  .empty-state {
    text-align: center;
    padding: 3rem 1rem;
    color: var(--brown-600);
  }
  .empty-state .icon { font-size: 2.5rem; margin-bottom: 0.75rem; }
  .empty-state p { font-size: 0.9rem; }
  .footer {
    text-align: center;
    padding: 1.5rem;
    color: var(--brown-600);
    font-size: 0.75rem;
  }
</style>
</head>
<body>
<div class="header">
  <div class="header-inner">
    <div>
      <h1>📚 %s</h1>
      <div class="subdomain">%s</div>
    </div>
    <div style="display:flex;align-items:center;gap:0.75rem">
      <div class="badge"><span class="dot"></span> Online</div>
      <a href="/settings" style="color:rgba(255,255,255,0.6);text-decoration:none;font-size:1.2rem" title="Settings">&#9881;</a>
    </div>
  </div>
</div>
<div class="container">
  <div class="stats-grid">
    <div class="stat-card">
      <div class="stat-num">%d</div>
      <div class="stat-label">EPUBs Scanned</div>
    </div>
    <div class="stat-card">
      <div class="stat-num">%d</div>
      <div class="stat-label">With ISBN</div>
    </div>
  </div>
  <div class="section">
    <div class="section-title">Catalog</div>
    <ul class="book-list" id="catalog"></ul>
  </div>
</div>
<div class="footer">Powered by the Mayberry Network</div>
<script>
fetch('/api/catalog').then(r=>r.json()).then(books=>{
  const ul=document.getElementById('catalog');
  if(!books||books.length===0){
    ul.innerHTML='<div class="empty-state"><div class="icon">📖</div><p>No EPUBs found yet. Add .epub files to your library folder.</p></div>';
    return;
  }
  books.forEach(b=>{
    const li=document.createElement('li');
    li.className='book-item';
    const isbn=b.isbn?'<span class="isbn-badge">'+b.isbn+'</span>':'';
    var coverKey=b.id||b.isbn||encodeURIComponent(b.path.split('/').pop());
    var img=b.has_cover?'<img src="/covers/'+coverKey+'" class="book-cover" onerror="this.style.display=\'none\';this.nextSibling.style.display=\'flex\'"><div class="book-icon" style="display:none">📕</div>':'<div class="book-icon">📕</div>';
    li.innerHTML=img+'<div class="book-info"><div class="book-title">'+(b.title||'Unknown Title')+'</div><div class="book-meta">by '+(b.author||'Unknown')+'</div></div>'+isbn;
    ul.appendChild(li);
  });
});
</script>
</body>
</html>`, branchName, branchName, subdomain, bookCount, isbnCount)
}

// --- Setup API ---

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}

	var req struct {
		LibraryPath   string `json:"library_path"`
		AudiobookPath string `json:"audiobook_path"`
		DisplayName   string `json:"display_name"`

		// Optional mirror settings — only applied when present in the request.
		// The pointer types let us distinguish "field omitted" from "field set to zero value."
		MirrorNetwork   *bool     `json:"mirror_network,omitempty"`
		MirrorSize      *string   `json:"mirror_size,omitempty"`
		MirrorOnly      *[]string `json:"mirror_only,omitempty"`
		MirrorIgnore    *[]string `json:"mirror_ignore,omitempty"`
		MirrorRate      *string   `json:"mirror_rate,omitempty"`
		MirrorServeRate *string   `json:"mirror_serve_rate,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	req.LibraryPath = strings.TrimSpace(req.LibraryPath)
	req.AudiobookPath = strings.TrimSpace(req.AudiobookPath)
	if req.LibraryPath == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Library path is required"})
		return
	}

	// Validate mirror fields up front before touching cfg.
	if req.MirrorSize != nil {
		if _, err := config.ParseSize(*req.MirrorSize); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid mirror_size: " + err.Error()})
			return
		}
	}
	if req.MirrorRate != nil && !config.IsValidMirrorRate(*req.MirrorRate) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid mirror_rate (expected: slow|normal|fast)"})
		return
	}
	if req.MirrorServeRate != nil {
		if _, err := config.ParseSize(*req.MirrorServeRate); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid mirror_serve_rate: " + err.Error()})
			return
		}
	}

	// Validate the path exists and is a directory.
	info, err := os.Stat(req.LibraryPath)
	if err != nil || !info.IsDir() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Library path does not exist or is not a directory"})
		return
	}

	// Audiobook path is optional; validate only if provided.
	if req.AudiobookPath != "" {
		ai, err := os.Stat(req.AudiobookPath)
		if err != nil || !ai.IsDir() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "Audiobook path does not exist or is not a directory"})
			return
		}
	}

	// Update config.
	if s.cfg == nil {
		s.cfg = &config.BranchConfig{Port: 1950, ServerURL: config.DefaultServerURL}
	}
	s.cfg.LibraryPath = req.LibraryPath
	s.cfg.AudiobookPath = req.AudiobookPath
	s.libraryDir = req.LibraryPath

	if req.DisplayName != "" {
		s.cfg.DisplayName = req.DisplayName
		s.cfg.Subdomain = config.Sanitize(req.DisplayName)
	}

	// Apply mirror settings (validated above).
	if req.MirrorNetwork != nil {
		s.cfg.MirrorNetwork = *req.MirrorNetwork
	}
	if req.MirrorSize != nil {
		s.cfg.MirrorSize = *req.MirrorSize
	}
	if req.MirrorOnly != nil {
		s.cfg.MirrorOnly = *req.MirrorOnly
	}
	if req.MirrorIgnore != nil {
		s.cfg.MirrorIgnore = *req.MirrorIgnore
	}
	if req.MirrorRate != nil {
		s.cfg.MirrorRate = *req.MirrorRate
	}
	if req.MirrorServeRate != nil {
		s.cfg.MirrorServeRate = *req.MirrorServeRate
	}

	if err := config.SaveBranch(s.cfg); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		log.Printf("branch: save config: %v", err)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to save configuration"})
		return
	}

	log.Printf("branch: setup complete — library=%s, name=%s", s.cfg.LibraryPath, s.cfg.DisplayName)

	// Trigger rescan callback.
	if s.onSetup != nil {
		go s.onSetup(s.cfg)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":       "ok",
		"library_path": s.cfg.LibraryPath,
		"display_name": s.cfg.DisplayName,
	})
}

// handleRestart asks the host process to restart the daemon. The HTTP
// response is sent before the restart begins, so the browser sees a clean
// 200 before the connection drops.
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	if s.onRestart == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(501)
		json.NewEncoder(w).Encode(map[string]string{"error": "Restart not supported on this platform"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "restarting"})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go s.onRestart()
}

// handleMirrorStatus returns a JSON snapshot of mirror state for the
// dashboard. Always returns 200 with a status payload — the "enabled"
// field tells the UI whether to render mirror sections at all.
func (s *Server) handleMirrorStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.onMirrorStats == nil {
		// Mirror not wired into this Server (mirror_network=false on
		// boot). Return a sentinel "disabled" object so the dashboard
		// JS can branch cleanly.
		json.NewEncoder(w).Encode(mirror.Stats{Enabled: false})
		return
	}
	json.NewEncoder(w).Encode(s.onMirrorStats())
}

// handleMirrorPurge wipes mirrored content. Blocks until any in-flight
// download finishes (worst case ~10s on slow rate).
func (s *Server) handleMirrorPurge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	if s.onMirrorPurge == nil {
		http.Error(w, "Mirror not enabled", 503)
		return
	}
	if err := s.onMirrorPurge(); err != nil {
		log.Printf("branch: mirror purge: %v", err)
		http.Error(w, "Purge failed: "+err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "purged"})
}

// handleSyncNow triggers an immediate scan + Town Square sync. The actual
// work runs in a goroutine so the HTTP response returns quickly.
func (s *Server) handleSyncNow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	if s.onSync == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		json.NewEncoder(w).Encode(map[string]string{"error": "Daemon not ready"})
		return
	}
	go s.onSync()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "syncing"})
}

// handleBrowse returns a directory listing for the folder picker UI.
func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("path")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "/"
		}
		dir = home
	}

	// Prevent path traversal — resolve to absolute.
	dir = filepath.Clean(dir)

	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		http.Error(w, "Not a directory", 400)
		return
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		http.Error(w, "Cannot read directory", 400)
		return
	}

	type dirEntry struct {
		Name  string `json:"name"`
		Path  string `json:"path"`
		IsDir bool   `json:"is_dir"`
	}

	var dirs []dirEntry
	// Add parent directory entry unless we're at root.
	parent := filepath.Dir(dir)
	if parent != dir {
		dirs = append(dirs, dirEntry{Name: "..", Path: parent, IsDir: true})
	} else if runtime.GOOS == "windows" {
		// At a drive root — show all available drives so users can switch partitions.
		for _, letter := range "ABCDEFGHIJKLMNOPQRSTUVWXYZ" {
			drive := string(letter) + ":\\"
			if drive == dir {
				continue // skip current drive
			}
			if _, err := os.Stat(drive); err == nil {
				dirs = append(dirs, dirEntry{Name: drive, Path: drive, IsDir: true})
			}
		}
	}
	for _, e := range entries {
		// Skip hidden files/dirs.
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if e.IsDir() {
			dirs = append(dirs, dirEntry{
				Name:  e.Name(),
				Path:  filepath.Join(dir, e.Name()),
				IsDir: true,
			})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"current": dir,
		"entries": dirs,
	})
}

func (s *Server) handleSettingsPage(w http.ResponseWriter, r *http.Request) {
	displayName := ""
	libraryPath := ""
	audiobookPath := ""
	subdomain := ""
	var mirrorHTML string
	if s.cfg != nil {
		displayName = s.cfg.DisplayName
		libraryPath = s.cfg.LibraryPath
		audiobookPath = s.cfg.AudiobookPath
		subdomain = s.cfg.Subdomain
		mirrorHTML = mirrorSettingsHTML(s.cfg)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Settings — Mayberry Branch</title>
<style>
  *, *::before, *::after { margin: 0; padding: 0; box-sizing: border-box; }
  :root {
    --brown-900: #3B2314; --brown-800: #4A3728; --brown-600: #8A7968;
    --brown-400: #C4A882; --brown-300: #D4A574; --brown-100: #F5F1EB;
    --brown-50: #FAF8F5; --green-500: #66BB6A; --red-500: #EF5350;
    --shadow-lg: 0 8px 30px rgba(59,35,20,0.12); --radius: 12px;
  }
  body {
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
    background: linear-gradient(135deg, var(--brown-100) 0%%, #E8E0D4 100%%);
    color: var(--brown-900); min-height: 100vh;
    display: flex; align-items: center; justify-content: center; padding: 2rem;
  }
  .card {
    background: white; border-radius: 16px; box-shadow: var(--shadow-lg);
    max-width: 520px; width: 100%%; padding: 2.5rem;
  }
  .card h1 { font-size: 1.6rem; color: var(--brown-800); margin-bottom: 0.25rem; }
  .subtitle { color: var(--brown-600); font-size: 0.95rem; margin-bottom: 2rem; line-height: 1.5; }
  .form-group { margin-bottom: 1.5rem; }
  .form-group label { display: block; font-weight: 600; font-size: 0.85rem; color: var(--brown-800); margin-bottom: 0.4rem; }
  .form-group .hint { font-size: 0.78rem; color: var(--brown-600); margin-bottom: 0.4rem; }
  .form-group .slug { font-size: 0.78rem; color: var(--brown-400); margin-top: 0.3rem; font-family: monospace; }
  .form-group input[type="text"] {
    width: 100%%; padding: 0.7rem 0.9rem; border: 1.5px solid #DDD5CA; border-radius: 8px;
    font-size: 0.95rem; color: var(--brown-900); background: var(--brown-50);
    transition: border-color 0.2s, box-shadow 0.2s; outline: none;
  }
  .form-group input[type="text"]:focus { border-color: var(--brown-400); box-shadow: 0 0 0 3px rgba(196,168,130,0.2); }
  .btn-primary {
    width: 100%%; padding: 0.8rem 1.5rem;
    background: linear-gradient(135deg, var(--brown-800), var(--brown-900));
    color: white; border: none; border-radius: 8px; font-size: 1rem; font-weight: 600;
    cursor: pointer; transition: transform 0.15s, box-shadow 0.15s;
  }
  .btn-primary:hover { transform: translateY(-1px); box-shadow: 0 4px 12px rgba(59,35,20,0.08); }
  .btn-primary:disabled { opacity: 0.6; cursor: not-allowed; transform: none; }
  .alert { padding: 0.75rem 1rem; border-radius: 8px; font-size: 0.85rem; margin-bottom: 1rem; display: none; }
  .alert-error { background: #FFF0F0; color: var(--red-500); border: 1px solid #FFD5D5; }
  .alert-success { background: #F0FFF0; color: #2E7D32; border: 1px solid #C8E6C9; }
  .back { display: inline-block; margin-bottom: 1rem; color: var(--brown-600); text-decoration: none; font-size: 0.85rem; }
  .back:hover { color: var(--brown-800); }
  .picker { border:1.5px solid #DDD5CA; border-radius:8px; background:var(--brown-50); max-height:260px; overflow-y:auto; }
  .picker-row { display:flex; align-items:center; padding:0.45rem 0.75rem; cursor:pointer; border-bottom:1px solid #F0EBE3; font-size:0.88rem; gap:0.5rem; }
  .picker-row:last-child { border-bottom:none; }
  .picker-row:hover { background:#EDE8DF; }
  .picker-row .icon { flex-shrink:0; width:20px; text-align:center; }
  .picker-row .name { flex:1; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; color:var(--brown-900); }
  .picker-current { padding:0.5rem 0.75rem; font-size:0.78rem; color:var(--brown-600); border-bottom:1.5px solid #DDD5CA; font-family:monospace; display:flex; align-items:center; justify-content:space-between; }
  .picker-current .select-btn { background:var(--brown-800); color:white; border:none; border-radius:6px; padding:0.3rem 0.75rem; font-size:0.75rem; cursor:pointer; font-weight:600; }
  .picker-current .select-btn:hover { background:var(--brown-900); }
  .picker-selected { padding:0.6rem 0.9rem; background:var(--brown-50); border:1.5px solid #C4A882; border-radius:8px; font-family:monospace; font-size:0.88rem; color:var(--brown-800); display:flex; align-items:center; justify-content:space-between; }
  .picker-selected .change-btn { background:none; border:1px solid var(--brown-400); border-radius:6px; padding:0.2rem 0.6rem; font-size:0.75rem; cursor:pointer; color:var(--brown-600); }
</style>
</head>
<body>
<div class="card">
  <a href="/" class="back">&larr; Back to Dashboard</a>
  <h1>Branch Settings</h1>
  <p class="subtitle">Update your branch name or library folder. Changes take effect immediately.</p>

  <div id="alert" class="alert alert-error"></div>

  <form id="settings-form" onsubmit="return saveSettings(event)">
    <div class="form-group">
      <label for="display_name">Branch Name</label>
      <div class="hint">A friendly name for your branch. This determines your public URL.</div>
      <input type="text" id="display_name" name="display_name" value="%s" required>
      <div class="slug" id="slug-preview">%s.branch.pub</div>
    </div>
    <div class="form-group">
      <label>EPUB Library Folder</label>
      <div class="hint">Folder containing your .epub files. Subfolders are scanned recursively.</div>
      <div id="library_path-selected" class="picker-selected" style="%s"><span>%s</span><button type="button" class="change-btn" onclick="changeFolder('library_path')">Change</button></div>
      <input type="hidden" id="library_path" name="library_path" value="%s">
      <div id="library_path-picker" class="picker" style="%s"></div>
    </div>
    <div class="form-group">
      <label>Audiobook Folder <span style="font-weight:400;color:var(--brown-600);">(optional)</span></label>
      <div class="hint">Folder containing your .m4b audiobooks. Leave blank to skip audiobook sync. Subfolders are scanned recursively.</div>
      <div id="audiobook_path-selected" class="picker-selected" style="%s"><span>%s</span><button type="button" class="change-btn" onclick="changeFolder('audiobook_path')">Change</button><button type="button" class="change-btn" style="margin-left:0.4rem;" onclick="clearFolder('audiobook_path')">Clear</button></div>
      <input type="hidden" id="audiobook_path" name="audiobook_path" value="%s">
      <div id="audiobook_path-picker" class="picker" style="%s"></div>
    </div>
    %s
    <button type="submit" class="btn-primary" id="submit-btn">Save Settings</button>
    <button type="button" class="btn-primary" id="restart-btn" style="margin-top:0.6rem;display:none;background:linear-gradient(135deg,#C97A4D,#A85D34);" onclick="restartDaemon()">Restart Now to Apply</button>
  </form>
</div>
<script>
async function loadDir(field, path) {
  var url = '/api/browse' + (path ? '?path=' + encodeURIComponent(path) : '');
  var resp = await fetch(url);
  var data = await resp.json();
  var picker = document.getElementById(field + '-picker');
  picker.innerHTML = '';
  var header = document.createElement('div');
  header.className = 'picker-current';
  var safeCurrent = data.current.replace(/\\/g, '\\\\').replace(/'/g, "\\'");
  header.innerHTML = '<span>' + data.current + '</span><button class="select-btn" onclick="selectFolder(\'' + field + '\', \'' + safeCurrent + '\')">Select This Folder</button>';
  picker.appendChild(header);
  (data.entries || []).forEach(function(e) {
    if (!e.is_dir) return;
    var row = document.createElement('div');
    row.className = 'picker-row';
    row.innerHTML = '<span class="icon">' + (e.name === '..' ? '⬆' : '📁') + '</span><span class="name">' + e.name + '</span>';
    row.onclick = function() { loadDir(field, e.path); };
    picker.appendChild(row);
  });
}
function selectFolder(field, path) {
  document.getElementById(field).value = path;
  document.getElementById(field + '-picker').style.display = 'none';
  var sel = document.getElementById(field + '-selected');
  sel.style.display = 'flex';
  var clearBtn = field === 'audiobook_path' ? '<button type="button" class="change-btn" style="margin-left:0.4rem;" onclick="clearFolder(\'' + field + '\')">Clear</button>' : '';
  sel.innerHTML = '<span>' + path + '</span><button type="button" class="change-btn" onclick="changeFolder(\'' + field + '\')">Change</button>' + clearBtn;
}
function changeFolder(field) {
  document.getElementById(field + '-picker').style.display = '';
  document.getElementById(field + '-selected').style.display = 'none';
  document.getElementById(field).value = '';
  loadDir(field, '');
}
function clearFolder(field) {
  document.getElementById(field).value = '';
  document.getElementById(field + '-picker').style.display = 'none';
  var sel = document.getElementById(field + '-selected');
  sel.style.display = 'none';
}
document.getElementById('display_name').addEventListener('input', function() {
  var slug = this.value.trim().toLowerCase().replace(/[^a-z0-9-]/g, '-').replace(/-+/g, '-').replace(/^-|-$/g, '');
  document.getElementById('slug-preview').textContent = (slug || '...') + '.branch.pub';
});
async function saveSettings(e) {
  e.preventDefault();
  var btn = document.getElementById('submit-btn');
  var alert = document.getElementById('alert');
  if (!document.getElementById('library_path').value) { alert.className='alert alert-error'; alert.textContent='Please select an EPUB library folder.'; alert.style.display='block'; return; }
  btn.disabled = true; btn.textContent = 'Saving...'; alert.style.display = 'none';
  function splitCSV(s) { return s.split(',').map(function(x){return x.trim();}).filter(function(x){return x;}); }
  var body = {
    library_path: document.getElementById('library_path').value.trim(),
    audiobook_path: document.getElementById('audiobook_path').value.trim(),
    display_name: document.getElementById('display_name').value.trim(),
    mirror_network: document.getElementById('mirror_network').checked,
    mirror_size: document.getElementById('mirror_size').value.trim(),
    mirror_only: splitCSV(document.getElementById('mirror_only').value),
    mirror_ignore: splitCSV(document.getElementById('mirror_ignore').value),
    mirror_rate: document.getElementById('mirror_rate').value,
    mirror_serve_rate: document.getElementById('mirror_serve_rate').value.trim()
  };
  try {
    var resp = await fetch('/api/setup', { method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(body) });
    var data = await resp.json();
    if (!resp.ok) throw new Error(data.error || 'Save failed');
    alert.className = 'alert alert-success';
    alert.innerHTML = 'Settings saved. Folder changes need a restart to take effect.';
    alert.style.display = 'block';
    btn.disabled = false; btn.textContent = 'Save Settings';
    document.getElementById('restart-btn').style.display = '';
    var slug = body.display_name.toLowerCase().replace(/[^a-z0-9-]/g, '-').replace(/-+/g, '-').replace(/^-|-$/g, '');
    document.getElementById('slug-preview').textContent = slug + '.branch.pub';
  } catch (err) {
    alert.className = 'alert alert-error'; alert.textContent = err.message; alert.style.display = 'block';
    btn.disabled = false; btn.textContent = 'Save Settings';
  }
}
async function restartDaemon() {
  var btn = document.getElementById('restart-btn');
  var alert = document.getElementById('alert');
  btn.disabled = true; btn.textContent = 'Restarting...';
  try {
    var resp = await fetch('/api/restart', { method: 'POST' });
    var data = await resp.json();
    if (!resp.ok) throw new Error(data.error || 'Restart failed');
    alert.className = 'alert alert-success';
    alert.innerHTML = 'Daemon restarting. This page will reconnect shortly...';
    alert.style.display = 'block';
    setTimeout(function() { window.location.reload(); }, 5000);
  } catch (err) {
    alert.className = 'alert alert-error'; alert.textContent = err.message; alert.style.display = 'block';
    btn.disabled = false; btn.textContent = 'Restart Now to Apply';
  }
}
if (!document.getElementById('library_path').value) { loadDir('library_path', ''); }
if (!document.getElementById('audiobook_path').value) { loadDir('audiobook_path', ''); }

// --- Mirror status + purge + disclosure ---
function fmtBytes(b) {
  if (!b) return '0 B';
  var u = ['B','KB','MB','GB','TB']; var i = 0;
  while (b >= 1024 && i < u.length - 1) { b /= 1024; i++; }
  return b.toFixed(b >= 10 || i === 0 ? 0 : 1) + ' ' + u[i];
}
function relTime(iso) {
  if (!iso || iso.indexOf('0001-') === 0) return '';
  var s = (Date.now() - new Date(iso).getTime()) / 1000;
  if (s < 60) return Math.floor(s) + 's ago';
  if (s < 3600) return Math.floor(s/60) + 'm ago';
  if (s < 86400) return Math.floor(s/3600) + 'h ago';
  return Math.floor(s/86400) + 'd ago';
}
async function refreshMirrorStatus() {
  try {
    var r = await fetch('/api/mirror/status');
    if (!r.ok) return;
    var s = await r.json();
    var panel = document.getElementById('mirror-status-panel');
    var btn = document.getElementById('mirror-purge-btn');
    if (!s.enabled) { panel.style.display = 'none'; btn.style.display = 'none'; return; }
    panel.style.display = ''; btn.style.display = '';
    document.getElementById('mirror-usage').textContent =
      fmtBytes(s.size_used_bytes) + ' of ' + fmtBytes(s.size_cap_bytes) + ' used — ' +
      (s.files_count || 0) + ' file(s) from ' + (s.sources_count || 0) + ' source(s)';
    var lastEl = document.getElementById('mirror-last-download');
    if (s.last_download_at && s.last_download_at.indexOf('0001-') !== 0) {
      lastEl.textContent = 'Last download: ' + (s.last_download_book || '—') + ' (' + relTime(s.last_download_at) + ')';
    } else {
      lastEl.textContent = 'No downloads yet';
    }
    var ul = document.getElementById('mirror-events');
    ul.innerHTML = '';
    (s.recent_events || []).slice(0, 5).forEach(function(e) {
      var li = document.createElement('li');
      li.style.padding = '0.2rem 0';
      var color = e.kind === 'accepted' ? '#3F804E' : '#A04040';
      var book = e.book_id ? ' ' + e.book_id : '';
      var reason = e.reason ? ' — ' + e.reason : '';
      li.innerHTML = '<span style="color:' + color + ';font-weight:600">' + e.kind + '</span>' +
                     book + ' <span style="color:#A0937D">' + relTime(e.at) + '</span>' + reason;
      ul.appendChild(li);
    });
  } catch (err) { /* network blips are fine; next interval retries */ }
}
document.getElementById('mirror-purge-btn').addEventListener('click', async function() {
  if (!confirm('Delete all mirrored books? This frees disk space immediately and cannot be undone.')) return;
  var btn = this;
  btn.disabled = true; btn.textContent = 'Purging...';
  try {
    var r = await fetch('/api/mirror/purge', { method: 'POST' });
    if (!r.ok) throw new Error(await r.text());
    btn.textContent = 'Purged ✓';
    setTimeout(refreshMirrorStatus, 400);
  } catch (err) {
    alert('Purge failed: ' + err.message);
  } finally {
    setTimeout(function() { btn.disabled = false; btn.textContent = 'Purge mirror'; }, 2000);
  }
});
document.getElementById('mirror_network').addEventListener('change', function() {
  if (!this.checked) return;
  if (localStorage.getItem('mayberry_mirror_disclosure_seen')) return;
  var ok = confirm('Network mirror notice:\n\nYou will host copies of other branches’ files so the network stays resilient when those branches go offline. Downloads are slow and rate-limited, the mirror respects your size cap, and you can purge everything from this page at any time.\n\nEnable network mirror?');
  if (!ok) { this.checked = false; return; }
  localStorage.setItem('mayberry_mirror_disclosure_seen', '1');
});
refreshMirrorStatus();
setInterval(refreshMirrorStatus, 30000);
</script>
</body>
</html>`, displayName, subdomain,
		pickerSelectedStyle(libraryPath), libraryPath, libraryPath, pickerBrowseStyle(libraryPath),
		pickerSelectedStyle(audiobookPath), audiobookPath, audiobookPath, pickerBrowseStyle(audiobookPath),
		mirrorHTML)
}

// mirrorSettingsHTML renders the Network Mirror form section. Kept separate
// from the main settings template because it has its own conditional logic
// (checked/selected attributes) that would clutter the inline HTML.
func mirrorSettingsHTML(cfg *config.BranchConfig) string {
	checked := ""
	if cfg.MirrorNetwork {
		checked = "checked"
	}
	sel := func(want string) string {
		if cfg.MirrorRate == want {
			return "selected"
		}
		return ""
	}
	only := strings.Join(cfg.MirrorOnly, ", ")
	ignore := strings.Join(cfg.MirrorIgnore, ", ")
	size := cfg.MirrorSize
	if size == "" {
		size = config.DefaultMirrorSize
	}
	serve := cfg.MirrorServeRate
	if serve == "" {
		serve = config.DefaultMirrorServeRate
	}
	return fmt.Sprintf(`
    <div class="form-group" style="margin-top:2rem;padding-top:1.5rem;border-top:1px solid #E8DED0">
      <div style="display:flex;align-items:center;gap:0.5rem;margin-bottom:0.5rem">
        <span style="font-size:0.7rem;background:#FFE4C4;color:#8B4513;padding:0.15rem 0.5rem;border-radius:10px;font-weight:600;letter-spacing:0.05em;text-transform:uppercase">Beta</span>
        <strong style="font-size:0.95rem;color:#4A3728">Network Mirror</strong>
      </div>
      <div class="hint" style="margin-bottom:1rem">Mirror books from other branches so they remain available if those branches go offline. The download client ships in a later update — for now you can configure preferences.</div>
      <label style="display:flex;align-items:center;gap:0.5rem;font-size:0.9rem;font-weight:500;margin-bottom:1rem;cursor:pointer">
        <input type="checkbox" id="mirror_network" %s>
        Enable network mirror
      </label>
      <label for="mirror_size" style="display:block;font-size:0.85rem;font-weight:600;margin-bottom:0.25rem">Mirror size limit</label>
      <div class="hint">Maximum disk space to use, e.g. 100G, 50G, 500M.</div>
      <input type="text" id="mirror_size" value="%s" style="margin-bottom:0.9rem">
      <label for="mirror_only" style="display:block;font-size:0.85rem;font-weight:600;margin-bottom:0.25rem">Only mirror from (optional)</label>
      <div class="hint">Comma-separated branch subdomains. Leave empty for "any branch with rare books".</div>
      <input type="text" id="mirror_only" value="%s" placeholder="janes-library, oak-grove" style="margin-bottom:0.9rem">
      <label for="mirror_ignore" style="display:block;font-size:0.85rem;font-weight:600;margin-bottom:0.25rem">Never mirror from (optional)</label>
      <div class="hint">Comma-separated branch subdomains to skip.</div>
      <input type="text" id="mirror_ignore" value="%s" style="margin-bottom:0.9rem">
      <label for="mirror_rate" style="display:block;font-size:0.85rem;font-weight:600;margin-bottom:0.25rem">Mirror speed</label>
      <div class="hint">Slow: ~6 books/hr at 500 KB/s. Normal: ~12/hr at 1 MB/s. Fast: ~30/hr at 5 MB/s.</div>
      <select id="mirror_rate" style="width:100%%;padding:0.7rem 0.9rem;border:1.5px solid #DDD5CA;border-radius:8px;background:#FAF8F5;font-size:0.95rem;color:#3B2314;margin-bottom:0.9rem">
        <option value="slow" %s>Slow</option>
        <option value="normal" %s>Normal</option>
        <option value="fast" %s>Fast</option>
      </select>
      <label for="mirror_serve_rate" style="display:block;font-size:0.85rem;font-weight:600;margin-bottom:0.25rem">Outbound mirror serve rate</label>
      <div class="hint">Bandwidth cap when others mirror from your branch, e.g. 200K, 1M.</div>
      <input type="text" id="mirror_serve_rate" value="%s">

      <div id="mirror-status-panel" style="margin-top:1.25rem;padding:0.9rem 1rem;background:#FAF6F0;border:1px solid #E8DCC8;border-radius:8px;display:none">
        <div style="font-size:0.8rem;font-weight:700;color:#4A3728;text-transform:uppercase;letter-spacing:0.05em;margin-bottom:0.5rem">Mirror status</div>
        <div id="mirror-usage" style="font-size:0.85rem;color:#5C4A38;margin-bottom:0.4rem"></div>
        <div id="mirror-last-download" style="font-size:0.82rem;color:#8A7968;margin-bottom:0.75rem"></div>
        <div style="font-size:0.75rem;font-weight:700;color:#8A7968;text-transform:uppercase;letter-spacing:0.05em;margin-bottom:0.3rem">Recent activity</div>
        <ul id="mirror-events" style="list-style:none;padding:0;margin:0;font-size:0.78rem;color:#5C4A38"></ul>
      </div>
      <button type="button" id="mirror-purge-btn" style="margin-top:0.6rem;display:none;background:#C97A4D;color:white;border:none;padding:0.55rem 1rem;border-radius:8px;font-size:0.85rem;font-weight:600;cursor:pointer">Purge mirror</button>
    </div>`, checked, size, only, ignore, sel("slow"), sel("normal"), sel("fast"), serve)
}

func pickerSelectedStyle(path string) string {
	if path != "" {
		return ""
	}
	return "display:none"
}

func pickerBrowseStyle(path string) string {
	if path != "" {
		return "display:none"
	}
	return ""
}

func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.catalog)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	json.NewEncoder(w).Encode(map[string]any{
		"branch_id":    s.branchID,
		"book_count":   len(s.catalog),
		"isbn_count":   len(s.holdings),
		"needs_setup":  s.needsSetup(),
	})
}

func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100"><text y="80" font-size="80">📚</text></svg>`))
}

// --- Covers ---

func (s *Server) handleLocalCover(w http.ResponseWriter, r *http.Request) {
	// Path: /covers/{isbn}.jpg or /covers/{filename}.jpg
	name := strings.TrimPrefix(r.URL.Path, "/covers/")
	if name == "" || strings.Contains(name, "..") || strings.Contains(name, "/") {
		http.NotFound(w, r)
		return
	}

	coverPath := filepath.Join(s.coverDir, name)
	if _, err := os.Stat(coverPath); err != nil {
		// Try with .jpg and .png extensions
		if _, err := os.Stat(coverPath + ".jpg"); err == nil {
			coverPath = coverPath + ".jpg"
		} else if _, err := os.Stat(coverPath + ".png"); err == nil {
			coverPath = coverPath + ".png"
		} else {
			http.NotFound(w, r)
			return
		}
	}

	http.ServeFile(w, r, coverPath)
}

// --- Download (Handshake Protocol) ---

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	// Mirror-serve admission: a request with X-Mayberry-Mirror=1 is from
	// another branch's mirror manager, not a real reader. Real users always
	// take priority — we refuse mirror serves while any real download is
	// in flight, and we only serve one mirror at a time. See Phase 4 in
	// MIRROR.md.
	isMirror := r.Header.Get("X-Mayberry-Mirror") == "1"
	if isMirror {
		select {
		case s.mirrorServeSlots <- struct{}{}:
			defer func() { <-s.mirrorServeSlots }()
		default:
			w.Header().Set("Retry-After", "60")
			http.Error(w, "Mirror serve capacity full", http.StatusServiceUnavailable)
			return
		}
		if s.realDownloads.Load() > 0 {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "Serving real downloads — try again later", http.StatusServiceUnavailable)
			return
		}
	} else {
		s.realDownloads.Add(1)
		defer s.realDownloads.Add(-1)
	}

	// Path: /download/{isbn}?token={jwt}
	isbn := strings.TrimPrefix(r.URL.Path, "/download/")
	isbn = strings.TrimSuffix(isbn, "/")
	if isbn == "" {
		http.Error(w, "Missing ISBN", 400)
		return
	}

	tokenStr := r.URL.Query().Get("token")
	if tokenStr == "" {
		http.Error(w, "Missing token", 401)
		return
	}

	if s.publicKey == nil {
		http.Error(w, "Public key not configured", 500)
		return
	}

	claims, err := auth.VerifyToken(s.publicKey, tokenStr)
	if err != nil {
		http.Error(w, "Invalid or expired token", 403)
		return
	}

	if claims.Purpose != "download" {
		http.Error(w, "Invalid token purpose", 403)
		return
	}

	if claims.ISBN != isbn {
		http.Error(w, "Token ISBN mismatch", 403)
		return
	}

	s.mu.RLock()
	filePath, ok := s.holdings[isbn]
	s.mu.RUnlock()

	if !ok {
		http.Error(w, "Book not found on this branch", 404)
		return
	}

	f, err := os.Open(filePath)
	if err != nil {
		http.Error(w, "File not available", 500)
		return
	}
	defer f.Close()

	finfo, err := f.Stat()
	if err != nil {
		http.Error(w, "File not available", 500)
		return
	}

	ext := strings.ToLower(filepath.Ext(filePath))
	contentType := "application/epub+zip"
	if ext == ".m4b" {
		contentType = "audio/mp4"
	} else {
		ext = ".epub"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s%s"`, isbn, ext))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", finfo.Size()))

	if isMirror {
		// Fire the touch callback BEFORE the slow throttled write begins,
		// so eviction's "recently served" window protects the file
		// throughout the transfer rather than only at completion.
		if s.onMirrorServe != nil {
			if sha := shaFromMirrorPath(filePath); sha != "" {
				s.onMirrorServe(sha)
			}
		}
		// Throttle mirror serves so we don't saturate the link the user
		// is reading on. The configured rate is parsed every request so a
		// settings change takes effect immediately without restart.
		rate := serveRate(s.cfg)
		if _, err := throttledCopy(r.Context(), w, f, rate); err != nil {
			// Connection drops mid-mirror are routine — log quietly.
			log.Printf("branch: mirror serve aborted (%s): %v", isbn, err)
		}
		return
	}
	io.Copy(w, f)
}

// shaFromMirrorPath extracts the SHA-256 hash from a mirror file path.
// Returns "" for paths that aren't under _mirror/ or don't have the
// expected <hash>.<ext> filename shape.
func shaFromMirrorPath(p string) string {
	if !mirror.IsMirrorPath(p) {
		return ""
	}
	base := filepath.Base(p)
	name := strings.TrimSuffix(base, filepath.Ext(base))
	if !hashFilenameRe.MatchString(name) {
		return ""
	}
	return name
}

// serveRate parses cfg.MirrorServeRate to a bytes-per-second cap. Zero
// disables throttling (treats as unlimited).
func serveRate(cfg *config.BranchConfig) int64 {
	if cfg == nil || cfg.MirrorServeRate == "" {
		// Fall back to the documented default if config is missing.
		n, _ := config.ParseSize(config.DefaultMirrorServeRate)
		return n
	}
	n, err := config.ParseSize(cfg.MirrorServeRate)
	if err != nil || n <= 0 {
		n, _ = config.ParseSize(config.DefaultMirrorServeRate)
	}
	return n
}

// throttledCopy streams src to dst with a sleep-based bytes/sec cap.
// Simple implementation: write a chunk, measure how long it took, sleep
// the remainder of the "should have taken" budget. Good enough to be a
// polite-neighbor; not a precise traffic shaper.
func throttledCopy(ctx context.Context, dst io.Writer, src io.Reader, bytesPerSec int64) (int64, error) {
	const chunkSize = 32 * 1024
	buf := make([]byte, chunkSize)
	var total int64
	for {
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		started := time.Now()
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
		}
		if bytesPerSec > 0 && n > 0 {
			budget := time.Duration(float64(n) / float64(bytesPerSec) * float64(time.Second))
			if extra := budget - time.Since(started); extra > 0 {
				select {
				case <-ctx.Done():
					return total, ctx.Err()
				case <-time.After(extra):
				}
			}
		}
		if rerr == io.EOF {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}
