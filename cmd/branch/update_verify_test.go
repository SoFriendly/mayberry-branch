package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Signing here mirrors cmd/relsign exactly; if the message formats drift,
// every release bricks auto-update, so the roundtrip is pinned by test.
func TestVerifyReleaseSignature(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	orig := releasePubKeyHex
	releasePubKeyHex = hex.EncodeToString(priv.Public().(ed25519.PublicKey))
	defer func() { releasePubKeyHex = orig }()

	sign := func(name, version string, data []byte) []byte {
		hash := sha256.Sum256(data)
		msg := append([]byte(fmt.Sprintf("mayberry-release-v1\n%s\n%s\n", name, version)), hash[:]...)
		return ed25519.Sign(priv, msg)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "mayberry-linux-amd64")
	data := []byte("fake binary contents")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	sig := sign("mayberry-linux-amd64", "0.8.29", data)

	if err := verifyReleaseSignature(path, "mayberry-linux-amd64", "0.8.29", sig); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	// Same signature must not transfer to another platform's name, another
	// version, or tampered bytes.
	if err := verifyReleaseSignature(path, "mayberry-darwin-arm64", "0.8.29", sig); err == nil {
		t.Fatal("signature accepted for wrong release name")
	}
	if err := verifyReleaseSignature(path, "mayberry-linux-amd64", "0.8.30", sig); err == nil {
		t.Fatal("signature accepted for wrong version")
	}
	if err := os.WriteFile(path, append(data, '!'), 0644); err != nil {
		t.Fatal(err)
	}
	if err := verifyReleaseSignature(path, "mayberry-linux-amd64", "0.8.29", sig); err == nil {
		t.Fatal("signature accepted for tampered binary")
	}
}

func TestIsNewerVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.8.29", "0.8.28", true},
		{"0.8.28", "0.8.28", false},
		{"0.8.27", "0.8.28", false},
		{"0.9.0", "0.8.28", true},
		{"1.0.0", "0.99.99", true},
		{"0.8.28", "0.7.35", true},
		{"unknown", "0.8.28", false},
		{"", "0.8.28", false},
		{"0.8.29", "dev", false}, // dev builds never auto-update (also gated earlier)
		{"v0.8.29", "0.8.28", true},
	}
	for _, c := range cases {
		if got := isNewerVersion(c.a, c.b); got != c.want {
			t.Errorf("isNewerVersion(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}
