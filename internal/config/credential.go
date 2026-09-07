package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sofriendly/mayberry/internal/auth"
)

// Store the machine secret separately so older clients saving branch.json do
// not erase it. Generate before registration, making a lost response retryable.
func ensureCredential(dir string) (string, error) {
	p := filepath.Join(dir, "branch-credential")
	data, err := os.ReadFile(p)
	if err == nil {
		secret := strings.TrimSpace(string(data))
		if auth.BranchCredentialHash(secret) == "" {
			return "", fmt.Errorf("invalid saved branch credential; restore it from backup")
		}
		return secret, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	secret, err := auth.NewBranchCredential()
	if err != nil {
		return "", err
	}
	// Write the complete secret first, then publish without replacing another
	// process's credential. Hard-link publication is atomic on supported systems.
	f, err := os.CreateTemp(dir, ".branch-credential-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if _, err = f.WriteString(secret + "\n"); err != nil {
		f.Close()
		return "", err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	if err = os.Link(f.Name(), p); err != nil {
		if os.IsExist(err) {
			return ensureCredential(dir)
		}
		return "", err
	}
	return secret, nil
}

func EnsureBranchCredential(cfg *BranchConfig) error {
	if cfg.BranchCredential != "" {
		return nil
	}
	dir, err := configDir()
	if err != nil {
		return err
	}
	cfg.BranchCredential, err = ensureCredential(dir)
	return err
}
