package branchhttp

import (
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
	"strings"
	"sync"

	"github.com/sofriendly/mayberry/internal/auth"
	"github.com/sofriendly/mayberry/internal/config"
	"github.com/sofriendly/mayberry/internal/epub"
)

// SetupCallback is called after the user completes the web setup wizard.
// It receives the updated config so the caller can trigger a rescan.
type SetupCallback func(cfg *config.BranchConfig)

// Server is the Branch local HTTP server providing the dashboard and download endpoint.
type Server struct {
	mux        *http.ServeMux
	branchID   string
	libraryDir string
	publicKey  ed25519.PublicKey
	coverDir   string // cached cover images

	mu       sync.RWMutex
	catalog  []CatalogEntry // current epub catalog
	holdings map[string]string // isbn -> filepath

	cfg           *config.BranchConfig
	onSetup       SetupCallback
}

// CatalogEntry is a scanned epub with its metadata and path.
type CatalogEntry struct {
	Path     string `json:"path"`
	Title    string `json:"title"`
	Author   string `json:"author"`
	ISBN     string `json:"isbn"`
	HasCover bool   `json:"has_cover"`
}

// NewServer creates the Branch local HTTP server.
func NewServer(branchID, libraryDir string) *Server {
	home, _ := os.UserHomeDir()
	coverDir := filepath.Join(home, ".mayberry", "covers")
	os.MkdirAll(coverDir, 0755)

	s := &Server{
		mux:        http.NewServeMux(),
		branchID:   branchID,
		libraryDir: libraryDir,
		holdings:   make(map[string]string),
		coverDir:   coverDir,
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

// SetPublicKey sets the Town Square public key for JWT verification.
func (s *Server) SetPublicKey(pk ed25519.PublicKey) {
	s.publicKey = pk
}

// BookMeta contains ISBN and EPUB-extracted metadata for sync.
type BookMeta struct {
	ISBN          string   `json:"isbn"`
	Title         string   `json:"title,omitempty"`
	Author        string   `json:"author,omitempty"`
	PublishedDate string   `json:"published_date,omitempty"`
	Categories    []string `json:"categories,omitempty"`
}

// bookID returns the ISBN if available, otherwise a content hash of title+author.
func bookID(isbn, title, author string) string {
	if isbn != "" {
		return isbn
	}
	h := sha256.Sum256([]byte(strings.ToLower(title + "\x00" + author)))
	return "MB" + hex.EncodeToString(h[:6]) // e.g. "MB1a2b3c4d5e6f"
}

// UpdateCatalog replaces the current catalog with newly scanned epub files.
// Returns metadata for all books (for sync to Town Square).
func (s *Server) UpdateCatalog(epubPaths []string) []BookMeta {
	var entries []CatalogEntry
	holdings := make(map[string]string)
	var books []BookMeta

	for _, p := range epubPaths {
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

		hasCover := false
		if len(meta.CoverData) > 0 {
			// Cache cover to disk. Use ISBN if available, otherwise hash the path.
			coverName := meta.ISBN
			if coverName == "" {
				coverName = filepath.Base(p)
			}
			ext := ".jpg"
			if strings.Contains(meta.CoverType, "png") {
				ext = ".png"
			}
			coverPath := filepath.Join(s.coverDir, coverName+ext)
			if err := os.WriteFile(coverPath, meta.CoverData, 0644); err == nil {
				hasCover = true
			}
		}

		id := bookID(meta.ISBN, meta.Title, meta.Author)
		entry := CatalogEntry{
			Path:     p,
			Title:    meta.Title,
			Author:   meta.Author,
			ISBN:     meta.ISBN,
			HasCover: hasCover,
		}
		entries = append(entries, entry)
		if meta.Title != "" {
			holdings[id] = p
			books = append(books, BookMeta{
				ISBN:          id,
				Title:         meta.Title,
				Author:        meta.Author,
				PublishedDate: meta.PublishedDate,
				Categories:    meta.Subjects,
			})
		}
	}

	s.mu.Lock()
	s.catalog = entries
	s.holdings = holdings
	s.mu.Unlock()

	log.Printf("branch: catalog updated — %d books, %d with ISBN", len(entries), len(books))
	return books
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.handleDashboard)
	s.mux.HandleFunc("/settings", s.handleSettingsPage)
	s.mux.HandleFunc("/api/catalog", s.handleCatalog)
	s.mux.HandleFunc("/api/status", s.handleStatus)
	s.mux.HandleFunc("/api/setup", s.handleSetup)
	s.mux.HandleFunc("/api/browse", s.handleBrowse)
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
      <label>Library Folder</label>
      <div class="hint">Point to the folder containing your .epub files (EPUB format only). All subfolders are scanned recursively.</div>
      <div id="picker-selected" class="picker-selected" style="display:none"></div>
      <input type="hidden" id="library_path" name="library_path">
      <div id="picker" class="picker"></div>
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
async function loadDir(path) {
  var url = '/api/browse' + (path ? '?path=' + encodeURIComponent(path) : '');
  var resp = await fetch(url);
  var data = await resp.json();
  var picker = document.getElementById('picker');
  picker.innerHTML = '';
  var header = document.createElement('div');
  header.className = 'picker-current';
  header.innerHTML = '<span>' + data.current + '</span><button class="select-btn" onclick="selectFolder(\'' + data.current.replace(/\\/g, '\\\\').replace(/'/g, "\\'") + '\')">Select This Folder</button>';
  picker.appendChild(header);
  (data.entries || []).forEach(function(e) {
    if (!e.is_dir) return;
    var row = document.createElement('div');
    row.className = 'picker-row';
    row.innerHTML = '<span class="icon">' + (e.name === '..' ? '⬆' : '📁') + '</span><span class="name">' + e.name + '</span>';
    row.onclick = function() { loadDir(e.path); };
    picker.appendChild(row);
  });
}
function selectFolder(path) {
  document.getElementById('library_path').value = path;
  document.getElementById('picker').style.display = 'none';
  var sel = document.getElementById('picker-selected');
  sel.style.display = 'flex';
  sel.innerHTML = '<span>' + path + '</span><button class="change-btn" onclick="changeFolder()">Change</button>';
}
function changeFolder() {
  document.getElementById('picker').style.display = '';
  document.getElementById('picker-selected').style.display = 'none';
  document.getElementById('library_path').value = '';
}
loadDir('');

async function submitSetup(e) {
  e.preventDefault();
  var btn = document.getElementById('submit-btn');
  var alert = document.getElementById('alert');
  if (!document.getElementById('library_path').value) { alert.className='alert alert-error'; alert.textContent='Please select a library folder.'; alert.style.display='block'; return; }
  btn.disabled = true; btn.textContent = 'Setting up...'; alert.style.display = 'none';
  var body = { library_path: document.getElementById('library_path').value.trim(), display_name: document.getElementById('display_name').value.trim() };
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
    var coverKey=b.isbn||encodeURIComponent(b.path.split('/').pop());
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
		LibraryPath string `json:"library_path"`
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	req.LibraryPath = strings.TrimSpace(req.LibraryPath)
	if req.LibraryPath == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Library path is required"})
		return
	}

	// Validate the path exists and is a directory.
	info, err := os.Stat(req.LibraryPath)
	if err != nil || !info.IsDir() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Library path does not exist or is not a directory"})
		return
	}

	// Update config.
	if s.cfg == nil {
		s.cfg = &config.BranchConfig{Port: 1950, ServerURL: config.DefaultServerURL}
	}
	s.cfg.LibraryPath = req.LibraryPath
	s.libraryDir = req.LibraryPath

	if req.DisplayName != "" {
		s.cfg.DisplayName = req.DisplayName
		s.cfg.Subdomain = config.Sanitize(req.DisplayName)
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
	subdomain := ""
	if s.cfg != nil {
		displayName = s.cfg.DisplayName
		libraryPath = s.cfg.LibraryPath
		subdomain = s.cfg.Subdomain
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
      <label>Library Folder</label>
      <div class="hint">Point to the folder containing your .epub files (EPUB format only). Subfolders are scanned recursively.</div>
      <div id="picker-selected" class="picker-selected" style="%s"><span>%s</span><button type="button" class="change-btn" onclick="changeFolder()">Change</button></div>
      <input type="hidden" id="library_path" name="library_path" value="%s">
      <div id="picker" class="picker" style="%s"></div>
    </div>
    <button type="submit" class="btn-primary" id="submit-btn">Save Settings</button>
  </form>
</div>
<script>
async function loadDir(path) {
  var url = '/api/browse' + (path ? '?path=' + encodeURIComponent(path) : '');
  var resp = await fetch(url);
  var data = await resp.json();
  var picker = document.getElementById('picker');
  picker.innerHTML = '';
  var header = document.createElement('div');
  header.className = 'picker-current';
  header.innerHTML = '<span>' + data.current + '</span><button class="select-btn" onclick="selectFolder(\'' + data.current.replace(/\\/g, '\\\\').replace(/'/g, "\\'") + '\')">Select This Folder</button>';
  picker.appendChild(header);
  (data.entries || []).forEach(function(e) {
    if (!e.is_dir) return;
    var row = document.createElement('div');
    row.className = 'picker-row';
    row.innerHTML = '<span class="icon">' + (e.name === '..' ? '⬆' : '📁') + '</span><span class="name">' + e.name + '</span>';
    row.onclick = function() { loadDir(e.path); };
    picker.appendChild(row);
  });
}
function selectFolder(path) {
  document.getElementById('library_path').value = path;
  document.getElementById('picker').style.display = 'none';
  var sel = document.getElementById('picker-selected');
  sel.style.display = 'flex';
  sel.innerHTML = '<span>' + path + '</span><button type="button" class="change-btn" onclick="changeFolder()">Change</button>';
}
function changeFolder() {
  document.getElementById('picker').style.display = '';
  document.getElementById('picker-selected').style.display = 'none';
  document.getElementById('library_path').value = '';
  loadDir('');
}
document.getElementById('display_name').addEventListener('input', function() {
  var slug = this.value.trim().toLowerCase().replace(/[^a-z0-9-]/g, '-').replace(/-+/g, '-').replace(/^-|-$/g, '');
  document.getElementById('slug-preview').textContent = (slug || '...') + '.branch.pub';
});
async function saveSettings(e) {
  e.preventDefault();
  var btn = document.getElementById('submit-btn');
  var alert = document.getElementById('alert');
  if (!document.getElementById('library_path').value) { alert.className='alert alert-error'; alert.textContent='Please select a library folder.'; alert.style.display='block'; return; }
  btn.disabled = true; btn.textContent = 'Saving...'; alert.style.display = 'none';
  var body = { library_path: document.getElementById('library_path').value.trim(), display_name: document.getElementById('display_name').value.trim() };
  try {
    var resp = await fetch('/api/setup', { method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(body) });
    var data = await resp.json();
    if (!resp.ok) throw new Error(data.error || 'Save failed');
    alert.className = 'alert alert-success'; alert.textContent = 'Settings saved! Rescanning library...'; alert.style.display = 'block';
    btn.disabled = false; btn.textContent = 'Save Settings';
    var slug = body.display_name.toLowerCase().replace(/[^a-z0-9-]/g, '-').replace(/-+/g, '-').replace(/^-|-$/g, '');
    document.getElementById('slug-preview').textContent = slug + '.branch.pub';
  } catch (err) {
    alert.className = 'alert alert-error'; alert.textContent = err.message; alert.style.display = 'block';
    btn.disabled = false; btn.textContent = 'Save Settings';
  }
}
if (!document.getElementById('library_path').value) { loadDir(''); }
</script>
</body>
</html>`, displayName, subdomain,
		pickerSelectedStyle(libraryPath), libraryPath, libraryPath,
		pickerBrowseStyle(libraryPath))
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

	w.Header().Set("Content-Type", "application/epub+zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.epub"`, isbn))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", finfo.Size()))
	io.Copy(w, f)
}
