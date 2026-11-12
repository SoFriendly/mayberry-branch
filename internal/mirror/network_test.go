package mirror

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestMirrorBlocksPrivateDestinations(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.100.100.200", "::1", "::ffff:127.0.0.1", "fc00::1", "fe80::1", "64:ff9b::7f00:1"} {
		if publicAddress(netip.MustParseAddr(ip)) {
			t.Errorf("allowed %s", ip)
		}
	}
	for _, u := range []string{"http://example.com/file", "https://127.0.0.1/file", "https://[::1]/file", "https://user@example.com/file", "file:///etc/passwd"} {
		if validateDownloadURL(u) == nil {
			t.Errorf("allowed %s", u)
		}
	}
	if err := validateDownloadURL("https://test.branch.pub/download/9780123456789?token=fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := dialPublic(context.Background(), "tcp", "localhost:1950"); err == nil {
		t.Fatal("dialed local network")
	}
}

func TestMirrorDoesNotFollowSourceRedirect(t *testing.T) {
	reached := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	defer target.Close()
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer source.Close()
	// Bypass only the initial dial in this test to use a local TLS fixture.
	client := source.Client()
	client.CheckRedirect = httpClient.CheckRedirect
	resp, err := client.Get(source.URL)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if reached || resp.StatusCode != 302 {
		t.Fatal("source redirect followed")
	}
}
