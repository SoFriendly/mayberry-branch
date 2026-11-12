package branchhttp

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sofriendly/mayberry/internal/config"
)

// A save without the audiobook_path field must keep the configured
// folder; only an explicit "" clears it. Regression: the settings form
// used to blank the hidden input when the user clicked "Change", so
// saving (or reloading) mid-browse silently wiped the saved folder.
func TestSetupAudiobookPathSemantics(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // keep config.SaveBranch away from ~/.mayberry
	lib, ab, ab2 := t.TempDir(), t.TempDir(), t.TempDir()

	s := NewServer("", lib)
	s.SetConfig(&config.BranchConfig{Port: 1950, LibraryPath: lib, AudiobookPath: ab, DisplayName: "Test"})

	post := func(body map[string]any) int {
		t.Helper()
		data, _ := json.Marshal(body)
		r := httptest.NewRequest("POST", "/api/setup", strings.NewReader(string(data)))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w.Code
	}

	// Field omitted → saved path kept.
	if code := post(map[string]any{"library_path": lib}); code != 200 {
		t.Fatal("save failed", code)
	}
	if s.cfg.AudiobookPath != ab {
		t.Fatalf("omitted field wiped audiobook path: %q", s.cfg.AudiobookPath)
	}
	// New valid folder → replaced.
	if code := post(map[string]any{"library_path": lib, "audiobook_path": ab2}); code != 200 {
		t.Fatal("save failed", code)
	}
	if s.cfg.AudiobookPath != ab2 {
		t.Fatalf("audiobook path not updated: %q", s.cfg.AudiobookPath)
	}
	// Nonexistent folder → rejected, nothing changed.
	if code := post(map[string]any{"library_path": lib, "audiobook_path": ab2 + "/nope"}); code != 400 {
		t.Fatal("bad path accepted", code)
	}
	if s.cfg.AudiobookPath != ab2 {
		t.Fatal("failed save modified config")
	}
	// Explicit empty → cleared (the Clear button).
	if code := post(map[string]any{"library_path": lib, "audiobook_path": ""}); code != 200 {
		t.Fatal("clear failed", code)
	}
	if s.cfg.AudiobookPath != "" {
		t.Fatalf("explicit clear ignored: %q", s.cfg.AudiobookPath)
	}
}
