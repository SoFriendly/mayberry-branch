package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
)

const BranchCredentialHeader = "X-Mayberry-Branch-Credential"

func NewBranchCredential() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func BranchCredentialHash(secret string) string {
	b, err := hex.DecodeString(secret)
	if err != nil || len(b) != 32 {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// BranchPost never forwards a machine credential to a redirected destination.
// All existing management URLs are final endpoints.
func BranchPost(client *http.Client, url, secret string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		if BranchCredentialHash(secret) == "" {
			return nil, fmt.Errorf("invalid branch credential")
		}
		req.Header.Set(BranchCredentialHeader, secret)
	}
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c.Do(req)
}
