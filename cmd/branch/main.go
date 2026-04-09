package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/sofriendly/mayberry/internal/branchhttp"
	"github.com/sofriendly/mayberry/internal/config"
	"github.com/sofriendly/mayberry/internal/friendlyid"
	"github.com/sofriendly/mayberry/internal/storage"
	"github.com/sofriendly/mayberry/internal/tunnel"
)

// ---------------------------------------------------------------------------
// Messages (Bubble Tea)
// ---------------------------------------------------------------------------

type tickMsg time.Time
type setupCompleteMsg struct{}

// ---------------------------------------------------------------------------
// Activity log — goroutine-safe ring buffer
// ---------------------------------------------------------------------------

type activityLog struct {
	mu      sync.Mutex
	entries []string
	max     int
}

func newActivityLog(max int) *activityLog {
	return &activityLog{max: max, entries: make([]string, 0, max)}
}

func (a *activityLog) Add(msg string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ts := time.Now().Format("15:04:05")
	entry := fmt.Sprintf("[%s] %s", ts, msg)
	if len(a.entries) >= a.max {
		a.entries = a.entries[1:]
	}
	a.entries = append(a.entries, entry)
}

func (a *activityLog) Lines() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.entries))
	copy(out, a.entries)
	return out
}

// ---------------------------------------------------------------------------
// Shared state between background goroutines and the TUI
// ---------------------------------------------------------------------------

type sharedState struct {
	mu        sync.RWMutex
	statuses  map[string]string
	bookCount int
	isbnCount int
}

func newSharedState() *sharedState {
	return &sharedState{
		statuses: map[string]string{
			"townsquare": "connecting",
			"tunnel":     "connecting",
			"watcher":    "starting",
		},
	}
}

func (s *sharedState) setStatus(service, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statuses[service] = status
}

func (s *sharedState) getStatuses() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.statuses))
	for k, v := range s.statuses {
		out[k] = v
	}
	return out
}

func (s *sharedState) setBookCount(books, isbns int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bookCount = books
	s.isbnCount = isbns
}

func (s *sharedState) getCounts() (int, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.bookCount, s.isbnCount
}

// ---------------------------------------------------------------------------
// Bubble Tea model — setup wizard TUI
// ---------------------------------------------------------------------------

type model struct {
	cfg          *config.BranchConfig
	setupDone    chan struct{} // closed when web setup completes
	step         int          // 0=welcome, 1=waiting
	dotCount     int
	width        int
	height       int
	quitting     bool
}

func initialModel(cfg *config.BranchConfig, setupDone chan struct{}) model {
	return model{
		cfg:       cfg,
		setupDone: setupDone,
		width:     80,
		height:    24,
	}
}


func tickCmd() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func waitForSetup(ch chan struct{}) tea.Cmd {
	return func() tea.Msg {
		<-ch
		return setupCompleteMsg{}
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(tickCmd(), waitForSetup(m.setupDone))
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		k := msg.String()
		if k == "q" || k == "ctrl+c" {
			m.quitting = true
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case setupCompleteMsg:
		return m, tea.Quit
	case tickMsg:
		m.dotCount = (m.dotCount + 1) % 4
		if m.step == 0 {
			m.step = 1
		}
		return m, tickCmd()
	}
	return m, nil
}

func (m model) View() string {
	if m.quitting {
		return ""
	}

	url := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#66BB6A")).Underline(true)
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("#6B6B6B"))

	dots := strings.Repeat(".", m.dotCount)
	var b strings.Builder
	b.WriteString("\n\n")
	b.WriteString("  📚 Open this link to set up your EPUB library branch:\n\n")
	b.WriteString("     " + url.Render(fmt.Sprintf("http://localhost:%d", m.cfg.Port)) + "\n\n")
	b.WriteString("  " + dim.Render(fmt.Sprintf("Waiting for setup%s", dots)) + "\n\n")
	b.WriteString("  " + dim.Render("Press q to cancel") + "\n")
	return b.String()
}

// ---------------------------------------------------------------------------
// Background services
// ---------------------------------------------------------------------------

// handlerSwap is an http.Handler that delegates to a swappable inner handler.
type handlerSwap struct {
	mu      sync.RWMutex
	handler http.Handler
}

func (h *handlerSwap) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	inner := h.handler
	h.mu.RUnlock()
	inner.ServeHTTP(w, r)
}

func (h *handlerSwap) Set(handler http.Handler) {
	h.mu.Lock()
	h.handler = handler
	h.mu.Unlock()
}

func startBackgroundServices(ctx context.Context, cfg *config.BranchConfig, hubURL string, state *sharedState, alog *activityLog, setupDone chan struct{}) {
	// Create local HTTP server with a swappable handler.
	branchSrv := branchhttp.NewServer("", cfg.LibraryPath)
	branchSrv.SetConfig(cfg)

	swap := &handlerSwap{handler: branchSrv}

	// Start the HTTP server once — it runs for the lifetime of the process.
	addr := fmt.Sprintf(":%d", cfg.Port)
	httpSrv := &http.Server{Addr: addr, Handler: swap}
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			alog.Add(fmt.Sprintf("HTTP server error: %v", err))
		}
	}()
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutCtx)
	}()

	// Setup callback: when a user completes the web setup wizard, swap
	// the handler and start the full service stack.
	branchSrv.SetSetupCallback(func(updated *config.BranchConfig) {
		alog.Add(fmt.Sprintf("Web setup complete — library: %s", updated.LibraryPath))
		cfg.LibraryPath = updated.LibraryPath
		cfg.DisplayName = updated.DisplayName
		cfg.Subdomain = updated.Subdomain

		newSrv := branchhttp.NewServer("", updated.LibraryPath)
		newSrv.SetConfig(cfg)
		swap.Set(newSrv)

		// Trigger an initial scan of the new library path.
		epubs, err := storage.ScanDirectory(updated.LibraryPath)
		if err == nil {
			books := newSrv.UpdateCatalog(epubs)
			state.setBookCount(len(epubs), len(books))
			alog.Add(fmt.Sprintf("Initial scan: %d book(s), %d with ISBN", len(epubs), len(books)))
		}

		// Signal the TUI that setup is done.
		select {
		case <-setupDone:
		default:
			close(setupDone)
		}

		// Start the full service stack.
		go startFullServices(ctx, cfg, hubURL, state, alog, newSrv, swap)
	})

	if cfg.LibraryPath == "" {
		// Setup-wizard-only mode.
		alog.Add(fmt.Sprintf("Awaiting setup — open http://localhost:%d in your browser", cfg.Port))
		state.setStatus("townsquare", "pending")
		state.setStatus("tunnel", "pending")
		state.setStatus("watcher", "pending")
		return
	}

	// Already configured — signal immediately and start services.
	select {
	case <-setupDone:
	default:
		close(setupDone)
	}
	startFullServices(ctx, cfg, hubURL, state, alog, branchSrv, swap)
}

func startFullServices(ctx context.Context, cfg *config.BranchConfig, hubURL string, state *sharedState, alog *activityLog, branchSrv *branchhttp.Server, swap *handlerSwap) {
	// Register with Town Square
	alog.Add("Registering with Town Square...")
	branchID := register(cfg)
	if branchID != "" {
		branchSrv = branchhttp.NewServer(branchID, cfg.LibraryPath)
		branchSrv.SetConfig(cfg)
		branchSrv.SetSetupCallback(func(updated *config.BranchConfig) {
			// Settings changed — update config and rescan.
			cfg.LibraryPath = updated.LibraryPath
			cfg.DisplayName = updated.DisplayName
			cfg.Subdomain = updated.Subdomain
		})
		swap.Set(branchSrv)
		state.setStatus("townsquare", "connected")
		alog.Add(fmt.Sprintf("Registered with Town Square (id: %s)", branchID))
	} else {
		state.setStatus("townsquare", "error")
		alog.Add("Town Square registration failed")
	}

	// Fetch Town Square public key
	go func() {
		alog.Add("Fetching Town Square public key...")
		fetchPublicKey(cfg.ServerURL, branchSrv)
	}()

	// Folder watcher
	watcher := storage.NewWatcher(cfg.LibraryPath, 30*time.Second, func(epubs []string) {
		books := branchSrv.UpdateCatalog(epubs)
		state.setBookCount(len(epubs), len(books))
		state.setStatus("watcher", "watching")
		alog.Add(fmt.Sprintf("Scanned %d book(s), %d with ISBN", len(epubs), len(books)))
		if branchID != "" {
			syncBooks(cfg.ServerURL, branchID, books)
			alog.Add(fmt.Sprintf("Synced %d book(s) to Town Square", len(books)))
		}
	})
	watcher.Start()
	state.setStatus("watcher", "watching")
	alog.Add("Watching library: " + cfg.LibraryPath)

	// Tunnel — get a signed token from Town Square first.
	tunnelToken := ""
	if branchID != "" {
		tunnelToken = fetchTunnelToken(cfg.ServerURL, branchID, cfg.Subdomain)
		if tunnelToken != "" {
			alog.Add("Obtained tunnel authorization token")
		} else {
			alog.Add("Warning: no tunnel token — hub may reject connection")
		}
	}
	tun := tunnel.NewClient(cfg.Subdomain, cfg.Port, hubURL, tunnelToken, func() string {
		return fetchTunnelToken(cfg.ServerURL, branchID, cfg.Subdomain)
	})
	go func() {
		alog.Add(fmt.Sprintf("Connecting tunnel %s.branch.pub...", cfg.Subdomain))
		if err := tun.Connect(ctx); err != nil {
			state.setStatus("tunnel", "error")
			alog.Add(fmt.Sprintf("Tunnel error: %v", err))
		} else {
			state.setStatus("tunnel", "connected")
			alog.Add(fmt.Sprintf("Tunnel connected: %s.branch.pub", cfg.Subdomain))
		}
	}()

	// Heartbeat
	if branchID != "" {
		go func() {
			alog.Add("Heartbeat started (5m interval)")
			heartbeatLoop(ctx, cfg.ServerURL, branchID, alog)
		}()
	}

	// Auto-update check (every hour)
	go autoUpdateLoop(ctx, alog)

	// Clean up on context cancellation.
	go func() {
		<-ctx.Done()
		tun.Close()
		watcher.Stop()
	}()
}

// ---------------------------------------------------------------------------
// Network helpers (unchanged logic, adapted signatures)
// ---------------------------------------------------------------------------

// httpClient is used for all Town Square API calls.
var httpClient = &http.Client{Timeout: 15 * time.Second}

func register(cfg *config.BranchConfig) string {
	body, err := json.Marshal(map[string]string{
		"branch_id":    cfg.BranchID,
		"display_name": cfg.DisplayName,
		"subdomain":    cfg.Subdomain,
		"tunnel_id":    cfg.Subdomain,
	})
	if err != nil {
		log.Printf("branch: register marshal: %v", err)
		return ""
	}
	resp, err := httpClient.Post(cfg.ServerURL+"/api/branches/register", "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("branch: register: %v", err)
		return ""
	}
	defer resp.Body.Close()

	var result struct {
		BranchID string `json:"branch_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("branch: register decode: %v", err)
		return ""
	}
	if result.BranchID != "" && result.BranchID != cfg.BranchID {
		cfg.BranchID = result.BranchID
		if err := config.SaveBranch(cfg); err != nil {
			log.Printf("branch: save config: %v", err)
		}
	}
	return result.BranchID
}

// fetchTunnelToken requests a signed tunnel JWT from Town Square.
func fetchTunnelToken(serverURL, branchID, subdomain string) string {
	body, _ := json.Marshal(map[string]string{
		"branch_id": branchID,
		"subdomain": subdomain,
	})
	resp, err := httpClient.Post(serverURL+"/api/tunnel/token", "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("branch: tunnel token: %v", err)
		return ""
	}
	defer resp.Body.Close()
	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("branch: tunnel token decode: %v", err)
		return ""
	}
	return result.Token
}

func syncBooks(serverURL, branchID string, books []branchhttp.BookMeta) {
	body, err := json.Marshal(map[string]any{
		"branch_id": branchID,
		"books":     books,
	})
	if err != nil {
		log.Printf("branch: sync marshal: %v", err)
		return
	}
	resp, err := httpClient.Post(serverURL+"/api/branches/sync", "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("branch: sync: %v", err)
		return
	}
	resp.Body.Close()
}

const heartbeatInterval = 5 * time.Minute

func heartbeatLoop(ctx context.Context, serverURL, branchID string, alog *activityLog) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			body, err := json.Marshal(map[string]string{"branch_id": branchID})
			if err != nil {
				alog.Add("Heartbeat marshal error: " + err.Error())
				continue
			}
			resp, err := httpClient.Post(serverURL+"/api/branches/heartbeat", "application/json", bytes.NewReader(body))
			if err != nil {
				alog.Add("Heartbeat failed: " + err.Error())
				continue
			}
			resp.Body.Close()
			alog.Add("Heartbeat sent")
		}
	}
}

func fetchPublicKey(serverURL string, srv *branchhttp.Server) {
	resp, err := http.Get(serverURL + "/api/public-key")
	if err != nil {
		return
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil || len(data) != ed25519.PublicKeySize {
		return
	}
	srv.SetPublicKey(ed25519.PublicKey(data))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

// Version is set at build time via -ldflags.
var Version = "dev"

func main() {
	// Subcommand handling
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "service":
			handleServiceCommand()
			return
		case "update":
			handleUpdate()
			return
		case "version":
			fmt.Println("mayberry " + Version)
			return
		}
	}

	// Check for updates in the background (non-blocking).
	go checkForUpdate()

	libraryDir := flag.String("library", envOr("MAYBERRY_LIBRARY", ""), "path to epub library folder")
	displayName := flag.String("name", "", "display name (auto-generates friendly-id if empty)")
	serverURL := flag.String("server", envOr("MAYBERRY_SERVER", config.DefaultServerURL), "Town Square server URL")
	hubURL := flag.String("hub", envOr("MAYBERRY_HUB", config.DefaultHubURL), "tunnel hub URL")
	port := flag.Int("port", 1950, "local HTTP port")
	daemon := flag.Bool("daemon", false, "run in background without TUI")
	flag.Parse()

	// Load or create config
	cfg, err := config.LoadBranch()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	if *libraryDir != "" {
		cfg.LibraryPath = *libraryDir
	}
	// If no library path is configured, run in setup wizard mode instead of exiting.
	// The web UI at http://localhost:{port} will show a setup wizard.
	if *serverURL != "" {
		cfg.ServerURL = *serverURL
	}
	if *port != 0 {
		cfg.Port = *port
	}

	// First run: generate friendly-id
	if cfg.FriendlyID == "" {
		cfg.FriendlyID = friendlyid.Generate()
	}
	if *displayName != "" {
		cfg.DisplayName = *displayName
	}
	if cfg.DisplayName == "" {
		cfg.DisplayName = cfg.FriendlyID
	}
	cfg.Subdomain = config.Sanitize(cfg.DisplayName)

	if err := config.SaveBranch(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "branch: warning: could not save config: %v\n", err)
	}

	// Background context — lives for the entire process
	sigCtx, sigStop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer sigStop()

	needsSetup := cfg.LibraryPath == ""

	// Daemon mode — run services directly, no TUI.
	if *daemon {
		state := newSharedState()
		alog := newActivityLog(100)
		setupDone := make(chan struct{})
		go startBackgroundServices(sigCtx, cfg, *hubURL, state, alog, setupDone)
		if needsSetup {
			fmt.Printf("📚 Mayberry Branch — setup required\n")
			fmt.Printf("   Open http://localhost:%d to configure\n", cfg.Port)
		} else {
			fmt.Printf("📚 %s is running\n", cfg.DisplayName)
			fmt.Printf("   Dashboard: http://localhost:%d\n", cfg.Port)
		}
		<-sigCtx.Done()
		fmt.Println("\nShutting down...")
		return
	}

	// Interactive mode.
	if needsSetup {
		// First run — start HTTP server for wizard, show TUI.
		state := newSharedState()
		alog := newActivityLog(100)
		setupDone := make(chan struct{})
		log.SetOutput(io.Discard) // suppress logs during TUI
		go startBackgroundServices(sigCtx, cfg, *hubURL, state, alog, setupDone)

		p := tea.NewProgram(
			initialModel(cfg, setupDone),
			tea.WithAltScreen(),
		)
		if _, err := p.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
			os.Exit(1)
		}
		log.SetOutput(os.Stderr)

		cfg, _ = config.LoadBranch()
		if cfg.LibraryPath == "" {
			fmt.Println("Setup cancelled.")
			return
		}
	}

	// Already configured (or just finished setup) — install as background service.
	fmt.Println()
	fmt.Printf("  📚 Mayberry Branch — %s\n", cfg.DisplayName)
	fmt.Println()
	fmt.Println("  Installing as a background service...")

	if err := serviceInstall(); err != nil {
		// Fallback: run in foreground.
		fmt.Printf("  Could not install service: %v\n", err)
		fmt.Println("  Running in foreground instead.")
		fmt.Println()

		state := newSharedState()
		alog := newActivityLog(100)
		setupDone := make(chan struct{})
		go startBackgroundServices(sigCtx, cfg, *hubURL, state, alog, setupDone)

		// Wait for services to connect, then print status.
		time.Sleep(3 * time.Second)
		printStatus(cfg)
		<-sigCtx.Done()
		fmt.Println("\nShutting down...")
		return
	}

	// Service installed — it runs independently. Give it a moment to start.
	time.Sleep(2 * time.Second)
	printStatus(cfg)
}

func printStatus(cfg *config.BranchConfig) {
	fmt.Printf("  ✓ Branch is running.\n")
	fmt.Println()
	fmt.Printf("  Your EPUBs are now discoverable on the Mayberry network.\n")
	fmt.Printf("  Anyone can browse and download from your branch at:\n")
	fmt.Println()
	fmt.Printf("    https://%s.branch.pub\n", cfg.Subdomain)
	fmt.Println()
	fmt.Printf("  Manage your branch:\n")
	fmt.Printf("    Dashboard:  http://localhost:%d\n", cfg.Port)
	fmt.Printf("    Settings:   http://localhost:%d/settings\n", cfg.Port)
	fmt.Printf("    Update:     mayberry update\n")
	fmt.Printf("    Stop:       mayberry service uninstall\n")
	fmt.Println()
}

func autoUpdateLoop(ctx context.Context, alog *activityLog) {
	if Version == "dev" {
		return
	}
	// Short delay before first check, then every 15 minutes.
	select {
	case <-ctx.Done():
		return
	case <-time.After(1 * time.Minute):
	}

	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()

	for {
		if performAutoUpdate(alog) {
			return // updated and restarting
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func performAutoUpdate(alog *activityLog) bool {
	checkClient := &http.Client{Timeout: 10 * time.Second}
	resp, err := checkClient.Get(config.DefaultServerURL + "/api/releases/latest")
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	var info struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return false
	}
	if info.Version == "" || info.Version == "unknown" || info.Version == Version {
		return false
	}

	alog.Add(fmt.Sprintf("Auto-updating: %s -> %s", Version, info.Version))
	log.Printf("auto-update: %s -> %s", Version, info.Version)

	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	url := fmt.Sprintf("%s/releases/mayberry-%s-%s%s", config.DefaultServerURL, runtime.GOOS, runtime.GOARCH, ext)

	dlClient := &http.Client{Timeout: 5 * time.Minute}
	dlResp, err := dlClient.Get(url)
	if err != nil {
		log.Printf("auto-update: download failed: %v", err)
		return false
	}
	if dlResp.StatusCode != 200 {
		dlResp.Body.Close()
		log.Printf("auto-update: download returned %d", dlResp.StatusCode)
		return false
	}
	defer dlResp.Body.Close()

	execPath, err := os.Executable()
	if err != nil {
		return false
	}
	execPath, _ = filepath.EvalSymlinks(execPath)

	tmpPath := execPath + ".update"
	tmp, err := os.Create(tmpPath)
	if err != nil {
		return false
	}

	if _, err := io.Copy(tmp, dlResp.Body); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return false
	}
	tmp.Close()
	os.Chmod(tmpPath, 0755)

	// Swap the binary.
	if runtime.GOOS == "windows" {
		oldPath := execPath + ".old"
		os.Remove(oldPath)
		os.Rename(execPath, oldPath)
	}
	if err := os.Rename(tmpPath, execPath); err != nil {
		os.Remove(tmpPath)
		return false
	}

	alog.Add(fmt.Sprintf("Updated to %s, restarting...", info.Version))
	log.Printf("auto-update: updated to %s, restarting service", info.Version)

	// Restart the service.
	switch runtime.GOOS {
	case "darwin":
		plist := macPlistPath()
		if _, err := os.Stat(plist); err == nil {
			exec.Command("launchctl", "unload", plist).Run()
			exec.Command("launchctl", "load", plist).Run()
		}
	case "linux":
		upath := linuxUnitPath()
		if _, err := os.Stat(upath); err == nil {
			exec.Command("systemctl", "--user", "restart", linuxUnit).Run()
		}
	}
	return true
}

func checkForUpdate() {
	if Version == "dev" {
		return
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(config.DefaultServerURL + "/api/releases/latest")
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var info struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return
	}
	if info.Version != "" && info.Version != "unknown" && info.Version != Version {
		fmt.Fprintf(os.Stderr, "\n  Update available: %s -> %s (run 'mayberry update' to install)\n\n", Version, info.Version)
	}
}

func handleUpdate() {
	serverURL := config.DefaultServerURL

	// Check latest version.
	resp, err := httpClient.Get(serverURL + "/api/releases/latest")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to check for updates: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var info struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to parse update response: %v\n", err)
		os.Exit(1)
	}

	if info.Version == "" || info.Version == "unknown" {
		fmt.Println("No releases available yet.")
		return
	}

	if info.Version == Version {
		fmt.Printf("Already up to date (version %s).\n", Version)
		return
	}

	fmt.Printf("Update available: %s -> %s\n", Version, info.Version)

	// Download the new binary.
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	ext := ""
	if goos == "windows" {
		ext = ".exe"
	}
	url := fmt.Sprintf("%s/releases/mayberry-%s-%s%s", serverURL, goos, goarch, ext)

	fmt.Printf("Downloading from %s...\n", url)
	dlResp, err := httpClient.Get(url)
	if err != nil || dlResp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "Download failed (status %d): %v\n", dlResp.StatusCode, err)
		os.Exit(1)
	}
	defer dlResp.Body.Close()

	// Write to a temp file first.
	execPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot determine executable path: %v\n", err)
		os.Exit(1)
	}
	execPath, _ = filepath.EvalSymlinks(execPath)

	tmpPath := execPath + ".update"
	tmp, err := os.Create(tmpPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot create temp file: %v\n", err)
		os.Exit(1)
	}

	if _, err := io.Copy(tmp, dlResp.Body); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		fmt.Fprintf(os.Stderr, "Download failed: %v\n", err)
		os.Exit(1)
	}
	tmp.Close()
	os.Chmod(tmpPath, 0755)

	// Swap the binary. On Windows, rename the old one out of the way first.
	if runtime.GOOS == "windows" {
		oldPath := execPath + ".old"
		os.Remove(oldPath)
		if err := os.Rename(execPath, oldPath); err != nil {
			os.Remove(tmpPath)
			fmt.Fprintf(os.Stderr, "Failed to replace binary: %v\n", err)
			os.Exit(1)
		}
	}
	if err := os.Rename(tmpPath, execPath); err != nil {
		os.Remove(tmpPath)
		fmt.Fprintf(os.Stderr, "Failed to replace binary: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Updated to version %s.\n", info.Version)

	// Restart the background service if installed.
	switch runtime.GOOS {
	case "darwin":
		plist := macPlistPath()
		if _, err := os.Stat(plist); err == nil {
			fmt.Println("Restarting background service...")
			exec.Command("launchctl", "unload", plist).Run()
			exec.Command("launchctl", "load", plist).Run()
			fmt.Println("Service restarted.")
		}
	case "linux":
		upath := linuxUnitPath()
		if _, err := os.Stat(upath); err == nil {
			fmt.Println("Restarting background service...")
			exec.Command("systemctl", "--user", "restart", linuxUnit).Run()
			fmt.Println("Service restarted.")
		}
	}
}

func handleServiceCommand() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: mayberry service [install|uninstall|deregister]\n")
		os.Exit(1)
	}

	switch os.Args[2] {
	case "install":
		if err := serviceInstall(); err != nil {
			fmt.Fprintf(os.Stderr, "service install: %v\n", err)
			os.Exit(1)
		}
	case "uninstall":
		if err := serviceUninstall(); err != nil {
			fmt.Fprintf(os.Stderr, "service uninstall: %v\n", err)
			os.Exit(1)
		}
	case "deregister":
		if err := branchDeregister(); err != nil {
			fmt.Fprintf(os.Stderr, "deregister: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "Unknown service action: %s\nUsage: mayberry service [install|uninstall|deregister]\n", os.Args[2])
		os.Exit(1)
	}
}

func branchDeregister() error {
	cfg, err := config.LoadBranch()
	if err != nil {
		return err
	}
	if cfg.BranchID == "" {
		return fmt.Errorf("branch is not registered")
	}

	body, _ := json.Marshal(map[string]string{"branch_id": cfg.BranchID})
	resp, err := http.Post(cfg.ServerURL+"/api/branches/deregister", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		cfg.BranchID = ""
		config.SaveBranch(cfg)
		fmt.Println("Branch successfully deregistered from Town Square.")
	} else {
		return fmt.Errorf("deregistration failed with status: %d", resp.StatusCode)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Service management — macOS (LaunchAgents) and Linux (systemd)
// ---------------------------------------------------------------------------

const (
	macLabel    = "com.sofriendly.mayberry"
	linuxUnit   = "mayberry-branch.service"
)

func macPlistPath() string {
	return filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents", macLabel+".plist")
}

func linuxUnitPath() string {
	return filepath.Join(os.Getenv("HOME"), ".config", "systemd", "user", linuxUnit)
}

var plistTemplate = template.Must(template.New("plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>{{ .Label }}</string>
    <key>ProgramArguments</key>
    <array>
        <string>{{ .ExecPath }}</string>
        <string>--daemon</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>{{ .LogPath }}/mayberry.log</string>
    <key>StandardErrorPath</key>
    <string>{{ .LogPath }}/mayberry.log</string>
</dict>
</plist>
`))

var systemdTemplate = template.Must(template.New("unit").Parse(`[Unit]
Description=Mayberry Branch Daemon
After=network-online.target
Wants=network-online.target

[Service]
ExecStart={{ .ExecPath }} --daemon
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`))

func serviceInstall() error {
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot determine executable path: %w", err)
	}

	switch runtime.GOOS {
	case "darwin":
		return installLaunchAgent(execPath)
	case "linux":
		return installSystemdUnit(execPath)
	case "windows":
		return installWindowsRegistry(execPath)
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}

func serviceUninstall() error {
	switch runtime.GOOS {
	case "darwin":
		return uninstallLaunchAgent()
	case "linux":
		return uninstallSystemdUnit()
	case "windows":
		return uninstallWindowsRegistry()
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}

func installWindowsRegistry(execPath string) error {
	// Add to HKCU Run key for current user persistence
	cmd := exec.Command("reg", "add", "HKCU\\Software\\Microsoft\\Windows\\CurrentVersion\\Run", "/v", "MayberryBranch", "/t", "REG_SZ", "/d", fmt.Sprintf("\"%s\" --daemon", execPath), "/f")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("reg add: %s — %w", strings.TrimSpace(string(out)), err)
	}
	fmt.Println("Mayberry Branch added to Windows Startup (Registry Run key).")
	return nil
}

func uninstallWindowsRegistry() error {
	cmd := exec.Command("reg", "delete", "HKCU\\Software\\Microsoft\\Windows\\CurrentVersion\\Run", "/v", "MayberryBranch", "/f")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("reg delete: %s — %w", strings.TrimSpace(string(out)), err)
	}
	fmt.Println("Mayberry Branch removed from Windows Startup.")
	return nil
}

func installLaunchAgent(execPath string) error {
	ppath := macPlistPath()
	logDir := filepath.Join(os.Getenv("HOME"), "Library", "Logs", "Mayberry")
	os.MkdirAll(filepath.Dir(ppath), 0755)
	os.MkdirAll(logDir, 0755)

	var buf bytes.Buffer
	if err := plistTemplate.Execute(&buf, map[string]string{
		"Label":    macLabel,
		"ExecPath": execPath,
		"LogPath":  logDir,
	}); err != nil {
		return err
	}
	if err := os.WriteFile(ppath, buf.Bytes(), 0644); err != nil {
		return err
	}

	// Load the agent
	if out, err := exec.Command("launchctl", "load", ppath).CombinedOutput(); err != nil {
		fmt.Printf("Plist written to %s\n", ppath)
		return fmt.Errorf("launchctl load: %s — %w", strings.TrimSpace(string(out)), err)
	}
	fmt.Printf("Service installed and loaded: %s\n", ppath)
	fmt.Printf("Logs: %s/mayberry.log\n", logDir)
	return nil
}

func uninstallLaunchAgent() error {
	ppath := macPlistPath()
	if _, err := os.Stat(ppath); err != nil {
		return fmt.Errorf("service not installed (no plist at %s)", ppath)
	}
	// Unload first (ignore errors — may already be unloaded)
	exec.Command("launchctl", "unload", ppath).Run()
	if err := os.Remove(ppath); err != nil {
		return err
	}
	fmt.Printf("Service uninstalled: %s\n", ppath)
	return nil
}

func installSystemdUnit(execPath string) error {
	upath := linuxUnitPath()
	os.MkdirAll(filepath.Dir(upath), 0755)

	var buf bytes.Buffer
	if err := systemdTemplate.Execute(&buf, map[string]string{
		"ExecPath": execPath,
	}); err != nil {
		return err
	}
	if err := os.WriteFile(upath, buf.Bytes(), 0644); err != nil {
		return err
	}

	// Reload and enable
	exec.Command("systemctl", "--user", "daemon-reload").Run()
	if out, err := exec.Command("systemctl", "--user", "enable", "--now", linuxUnit).CombinedOutput(); err != nil {
		fmt.Printf("Unit written to %s\n", upath)
		return fmt.Errorf("systemctl enable: %s — %w", strings.TrimSpace(string(out)), err)
	}
	fmt.Printf("Service installed and started: %s\n", upath)
	fmt.Println("View logs: journalctl --user -u mayberry-branch -f")
	return nil
}

func uninstallSystemdUnit() error {
	upath := linuxUnitPath()
	if _, err := os.Stat(upath); err != nil {
		return fmt.Errorf("service not installed (no unit at %s)", upath)
	}
	// Stop and disable (ignore errors — may already be stopped)
	exec.Command("systemctl", "--user", "disable", "--now", linuxUnit).Run()
	if err := os.Remove(upath); err != nil {
		return err
	}
	exec.Command("systemctl", "--user", "daemon-reload").Run()
	fmt.Printf("Service uninstalled: %s\n", upath)
	return nil
}

