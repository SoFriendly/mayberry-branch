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

func TestSharingProxyAndCrossOriginIsolation(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		u, p, ok := r.BasicAuth()
		if !ok || u != "123456789" || p != u {
			t.Error("missing saved card")
		}
		if r.URL.Path != "/api/sharing" && r.URL.Path != "/api/guest-card" {
			t.Error("unexpected path")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"shared_users":["987654321"]}`)
	}))
	defer upstream.Close()
	s := NewServer("branch", t.TempDir())
	s.SetConfig(&config.BranchConfig{UserID: "123456789", ServerURL: upstream.URL})
	for _, path := range []string{"/api/sharing", "/api/guest-card"} {
		for _, blocked := range []bool{false, true} {
			r := httptest.NewRequest("POST", path, strings.NewReader(`{"shared_users":[]}`))
			r.Header.Set("Content-Type", "application/json")
			if blocked {
				r.Header.Set("Origin", "https://evil.example")
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			want := 200
			if blocked {
				want = 403
			}
			if w.Code != want {
				t.Fatal(path, w.Code)
			}
		}
	}
	if calls != 2 {
		t.Fatal("blocked write reached upstream")
	}
}

func TestTunnelUsesCurrentSharing(t *testing.T) {
	status := http.StatusNoContent
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, _, _ := r.BasicAuth()
		if user != "987654321" || r.URL.Path != "/api/branch-access" || r.URL.Query().Get("branch_id") != "branch" {
			t.Error("wrong sharing check")
		}
		w.WriteHeader(status)
	}))
	defer upstream.Close()
	s := NewServer("branch", t.TempDir())
	s.SetConfig(&config.BranchConfig{UserID: "123456789", SharedUsers: []string{"987654321"}, ServerURL: upstream.URL})
	handler := s.tunnelAuth(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	for _, test := range []struct{ upstream, want int }{{204, 204}, {403, 401}, {500, 503}} {
		status = test.upstream
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("X-Mayberry-Via-Tunnel", "true")
		r.SetBasicAuth("987654321", "987654321")
		w := httptest.NewRecorder()
		handler(w, r)
		if w.Code != test.want {
			t.Fatal("stale local list used or sharing check failed", w.Code)
		}
	}
}
