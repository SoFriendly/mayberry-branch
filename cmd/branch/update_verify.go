package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// releasePubKeyHex is the public half of the offline release-signing key
// (cmd/relsign in the private repo). The private seed lives only on the
// release machine, never on any server, so neither mayberry.pub nor its
// database can produce a signature — a compromised server can serve a
// malicious binary but no client will install it.
var releasePubKeyHex = "bd86150969e50c60b9c19f9611263d1c6c1f84b2266d6423d18b5cbcd5ec1aa6"

// fetchReleaseSignature downloads the detached signature uploaded next to
// every release binary (an ordinary /releases/ entry named "<binary>.sig").
func fetchReleaseSignature(client *http.Client, binaryURL string) ([]byte, error) {
	resp, err := client.Get(binaryURL + ".sig")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("signature download returned %d", resp.StatusCode)
	}
	sig, err := io.ReadAll(io.LimitReader(resp.Body, ed25519.SignatureSize+1))
	if err != nil {
		return nil, err
	}
	if len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("signature is %d bytes, want %d", len(sig), ed25519.SignatureSize)
	}
	return sig, nil
}

// verifyReleaseSignature checks the downloaded binary at path against its
// detached signature before anything executes or installs it. The signed
// message binds the file hash to the release name and version (matching
// cmd/relsign), so a signature can't be replayed onto a different platform's
// binary or a different version.
func verifyReleaseSignature(path, name, version string, sig []byte) error {
	pub, err := hex.DecodeString(releasePubKeyHex)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("bad embedded release key")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(data)
	msg := append([]byte(fmt.Sprintf("mayberry-release-v1\n%s\n%s\n", name, version)), hash[:]...)
	if !ed25519.Verify(ed25519.PublicKey(pub), msg, sig) {
		return fmt.Errorf("signature verification failed")
	}
	return nil
}

// isNewerVersion reports whether a is a strictly newer semver than b.
// The updater only ever moves forward: combined with the signed version in
// verifyReleaseSignature, a compromised server can't downgrade clients to an
// older (signed) binary with known holes. Unparseable versions are never
// "newer", so garbage from the server fails closed.
func isNewerVersion(a, b string) bool {
	pa, aok := parseSemver(a)
	pb, bok := parseSemver(b)
	if !aok || !bok {
		return false
	}
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func parseSemver(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.SplitN(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".", 3)
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
