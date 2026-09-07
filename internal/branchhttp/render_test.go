package branchhttp

import (
	"html"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sofriendly/mayberry/internal/config"
)

func TestPagesEscapeConfiguredText(t *testing.T) {
	value := `"><img src=x onerror="alert(1)">`
	s := &Server{branchID: "test", cfg: &config.BranchConfig{DisplayName: value, Subdomain: value, LibraryPath: value, AudiobookPath: value}}
	for _, render := range []func(){
		func() {
			w := httptest.NewRecorder()
			s.serveDashboard(w, httptest.NewRequest("GET", "/", nil))
			checkEscaped(t, w.Body.String(), value)
		},
		func() {
			w := httptest.NewRecorder()
			s.serveSetupWizard(w, httptest.NewRequest("GET", "/", nil))
			checkEscaped(t, w.Body.String(), value)
		},
		func() {
			w := httptest.NewRecorder()
			s.handleSettingsPage(w, httptest.NewRequest("GET", "/settings", nil))
			checkEscaped(t, w.Body.String(), value)
		},
	} {
		render()
	}
}

func checkEscaped(t *testing.T, body, value string) {
	t.Helper()
	if strings.Contains(body, value) || !strings.Contains(body, html.EscapeString(value)) {
		t.Fatal("configured HTML was not escaped")
	}
}
