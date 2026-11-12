package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sofriendly/mayberry/internal/auth"
)

func TestCredentialPersistsAcrossConcurrentStarts(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	results := make(chan string, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			secret, err := ensureCredential(dir)
			if err != nil {
				t.Error(err)
				return
			}
			results <- secret
		}()
	}
	wg.Wait()
	close(results)
	first := ""
	for secret := range results {
		if auth.BranchCredentialHash(secret) == "" {
			t.Fatal("invalid credential")
		}
		if first == "" {
			first = secret
		}
		if first != secret {
			t.Fatal("startup race changed credential")
		}
	}
	// An older daemon's config save does not overwrite the separate secret.
	old := []byte(`{"branch_id":"saved-branch","user_id":"123456789","subdomain":"saved","shared_users":["987654321"],"library_path":"/books","port":1950}`)
	p := filepath.Join(dir, "branch.json")
	if err := os.WriteFile(p, old, 0600); err != nil {
		t.Fatal(err)
	}
	secret, err := ensureCredential(dir)
	if err != nil || secret != first {
		t.Fatal("credential not retained", err)
	}
	data, _ := os.ReadFile(p)
	if string(data) != string(old) {
		t.Fatal("configuration changed")
	}
	var cfg BranchConfig
	if err := json.Unmarshal(old, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.BranchCredential = secret
	data, err = json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	json.Unmarshal(data, &saved)
	if saved["user_id"] != "123456789" || saved["branch_id"] != "saved-branch" {
		t.Fatal("saved identity changed")
	}
	for _, v := range saved {
		if v == secret {
			t.Fatal("secret serialized in branch.json")
		}
	}
}

func TestInvalidCredentialIsNeverReplaced(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "branch-credential")
	os.WriteFile(p, []byte("damaged"), 0600)
	if _, err := ensureCredential(dir); err == nil {
		t.Fatal("accepted invalid saved credential")
	}
	data, _ := os.ReadFile(p)
	if string(data) != "damaged" {
		t.Fatal("silently replaced credential")
	}
}
