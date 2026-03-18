package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Token expiration constants.
const (
	DownloadTokenTTL = 10 * time.Minute
	TunnelTokenTTL   = 24 * time.Hour
)

// Claims represents the JWT payload for download authorization.
type Claims struct {
	BranchID  string `json:"branch_id"`
	ISBN      string `json:"isbn,omitempty"`
	Subdomain string `json:"subdomain,omitempty"`
	Purpose   string `json:"purpose"` // "download" or "tunnel"
	jwt.RegisteredClaims
}

// KeyPair holds an Ed25519 signing key pair.
type KeyPair struct {
	Private ed25519.PrivateKey
	Public  ed25519.PublicKey
}

// GenerateKeyPair creates a new Ed25519 key pair.
func GenerateKeyPair() (*KeyPair, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate keypair: %w", err)
	}
	return &KeyPair{Private: priv, Public: pub}, nil
}

// IssueDownloadToken creates a short-lived JWT for a book download.
func IssueDownloadToken(kp *KeyPair, branchID, isbn string) (string, error) {
	now := time.Now()
	claims := Claims{
		BranchID: branchID,
		ISBN:     isbn,
		Purpose:  "download",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "mayberry-townsquare",
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(DownloadTokenTTL)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	return token.SignedString(kp.Private)
}

// IssueTunnelToken creates a long-lived JWT authorizing a branch to register a tunnel.
func IssueTunnelToken(kp *KeyPair, branchID, subdomain string) (string, error) {
	now := time.Now()
	claims := Claims{
		BranchID:  branchID,
		Subdomain: subdomain,
		Purpose:   "tunnel",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "mayberry-townsquare",
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(TunnelTokenTTL)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	return token.SignedString(kp.Private)
}

// VerifyToken validates a JWT and returns the claims.
func VerifyToken(publicKey ed25519.PublicKey, tokenStr string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodEd25519); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return publicKey, nil
	})
	if err != nil {
		return nil, fmt.Errorf("verify token: %w", err)
	}
	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token claims")
	}
	return claims, nil
}
