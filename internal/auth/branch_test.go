package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBranchCredentialDoesNotFollowRedirect(t *testing.T) {
	secret, _ := NewBranchCredential()
	reached := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(BranchCredentialHeader) != secret {
			t.Error("credential missing at intended endpoint")
		}
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	resp, err := BranchPost(origin.Client(), origin.URL, secret, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if reached || resp.StatusCode != 307 {
		t.Fatal("credential request followed a redirect")
	}
}
