package feedprefs

import (
	"os"
	"testing"
)

func TestPreferenceNormalization(t *testing.T) {
	p := Defaults()
	p.Languages = []string{"English", "eng-US", "FR_ca"}
	if err := p.Normalize(); err != nil {
		t.Fatal(err)
	}
	if len(p.Languages) != 2 || p.Languages[0] != "en" || p.Languages[1] != "fr" {
		t.Fatal(p.Languages)
	}
}

// Optional export for a real DOM validation, without a Node dependency in Go builds.
func TestExportSettingsPage(t *testing.T) {
	if path := os.Getenv("MAYBERRY_SETTINGS_FIXTURE"); path != "" {
		if err := os.WriteFile(path, []byte(Page("/api/feed-preferences")), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
