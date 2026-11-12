package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/sofriendly/mayberry/internal/auth"
	"github.com/sofriendly/mayberry/internal/config"
)

func TestRegistrationSilentlyEnrollsSavedIdentity(t *testing.T) {
	secret, err := auth.NewBranchCredential()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.BranchConfig{BranchID: "existing-branch", UserID: "123456789", Subdomain: "existing", BranchCredential: secret, SharedUsers: []string{"987654321"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["branch_id"] != cfg.BranchID || body["enrollment_card"] != cfg.UserID || r.Header.Get(auth.BranchCredentialHeader) != secret {
			t.Error("registration did not supply saved identity and machine credential")
		}
		w.Header().Set("X-Mayberry-Branch-Auth", "enforced")
		json.NewEncoder(w).Encode(map[string]string{"branch_id": cfg.BranchID, "user_id": cfg.UserID})
	}))
	defer server.Close()
	cfg.ServerURL = server.URL
	before := *cfg
	if got := register(cfg); got != before.BranchID {
		t.Fatalf("registration returned %q", got)
	}
	if !reflect.DeepEqual(*cfg, before) {
		t.Fatal("automatic enrollment changed existing configuration")
	}
}

func TestRegistrationFailureRetainsSavedIdentity(t *testing.T) {
	secret, _ := auth.NewBranchCredential()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "temporarily unavailable", 503) }))
	defer server.Close()
	cfg := &config.BranchConfig{BranchID: "saved-branch", UserID: "123456789", BranchCredential: secret, ServerURL: server.URL}
	id, registered := registerOrRetainIdentity(cfg)
	if registered || id != "saved-branch" || cfg.BranchID != "saved-branch" || cfg.UserID != "123456789" {
		t.Fatal("registration failure discarded saved identity")
	}
	cfg.BranchID = ""
	if id, ok := registerOrRetainIdentity(cfg); id != "" || ok {
		t.Fatal("invented identity for new installation")
	}
}
