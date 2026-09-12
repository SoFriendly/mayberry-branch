package branchhttp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sofriendly/mayberry/internal/config"
)

func TestCatalogPreferencesProxyAndTunnelIsolation(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		user, pass, ok := r.BasicAuth()
		if !ok || user != "123456789" || pass != user {
			t.Error("saved card not sent to Town Square")
		}
		if r.URL.Path != "/api/feed-preferences" {
			t.Error("incorrect preferences endpoint")
		}
		if r.Method == http.MethodPut {
			b, _ := io.ReadAll(r.Body)
			if string(b) != `{"exclude_mirrored":true}` {
				t.Error("preferences changed in proxy")
			}
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"preferences":{"exclude_mirrored":true},"branches":[]}`)
	}))
	defer upstream.Close()
	s := NewServer("branch", t.TempDir())
	s.SetConfig(&config.BranchConfig{UserID: "123456789", ServerURL: upstream.URL})
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		r := httptest.NewRequest(method, "/api/feed-preferences", strings.NewReader(`{"exclude_mirrored":true}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 || strings.Contains(w.Body.String(), "123456789") {
			t.Fatal("proxy failed or exposed card")
		}
	}
	for _, path := range []string{"/catalog-settings", "/api/feed-preferences", "/api/sharing", "/api/guest-card"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("X-Mayberry-Via-Tunnel", "true")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal("settings reachable over public tunnel")
		}
	}
	r := httptest.NewRequest("PUT", "/api/feed-preferences", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://untrusted.example")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cross-origin settings write accepted")
	}
	if calls != 2 {
		t.Fatal("blocked requests reached Town Square")
	}
	r = httptest.NewRequest("GET", "/catalog-settings", nil)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "My catalog") || !strings.Contains(w.Body.String(), "Friends and sharing") {
		t.Fatal("local settings page missing")
	}
}
