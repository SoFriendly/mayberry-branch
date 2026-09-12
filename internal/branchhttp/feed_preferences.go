package branchhttp

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sofriendly/mayberry/internal/feedprefs"
)

func (s *Server) handleCatalogSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", 405)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if s.cfg == nil {
		fmt.Fprint(w, feedprefs.Page("/api/feed-preferences"))
		return
	}
	fmt.Fprint(w, feedprefs.LocalPage("/api/feed-preferences", s.cfg.UserID, s.cfg.SharedUsers))
}

// The browser never receives the card in JavaScript. Local settings use the
// saved card to talk to Town Square; the hosted page supports reader-only cards.
func (s *Server) handleFeedPreferences(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		http.Error(w, "Method not allowed", 405)
		return
	}
	if s.cfg == nil || s.cfg.UserID == "" {
		http.Error(w, "Register your branch before editing catalog filters", 409)
		return
	}
	origin, originErr := url.Parse(r.Header.Get("Origin"))
	if r.Method == http.MethodPut && (r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Sec-Fetch-Site") == "cross-site" || (r.Header.Get("Origin") != "" && (originErr != nil || origin.Host != r.Host))) {
		http.Error(w, "Invalid settings request", 403)
		return
	}
	body := http.MaxBytesReader(w, r.Body, 32<<10)
	req, err := http.NewRequestWithContext(r.Context(), r.Method, strings.TrimRight(s.cfg.ServerURL, "/")+"/api/feed-preferences", body)
	if err != nil {
		http.Error(w, "Invalid catalog server", 502)
		return
	}
	req.SetBasicAuth(s.cfg.UserID, s.cfg.UserID)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "Catalog server unavailable. Try again shortly.", 502)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, io.LimitReader(resp.Body, 1<<20))
}
