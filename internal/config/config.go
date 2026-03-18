package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// BranchConfig holds local Branch daemon configuration.
type BranchConfig struct {
	BranchID    string `json:"branch_id,omitempty"`
	FriendlyID  string `json:"friendly_id"`
	DisplayName string `json:"display_name"`
	Subdomain   string `json:"subdomain"`
	LibraryPath string `json:"library_path"`
	Port        int    `json:"port"`
	ServerURL   string `json:"server_url"`
}


const (
	DefaultServerURL = "https://mayberry.pub"
	DefaultHubURL    = "https://branch.pub"
)

var subdomainRe = regexp.MustCompile(`[^a-z0-9-]`)

// Sanitize converts a display name into a valid subdomain.
func Sanitize(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = subdomainRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	// Collapse consecutive hyphens.
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	return s
}

func configDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".mayberry")
	return dir, os.MkdirAll(dir, 0700)
}

// LoadBranch reads or creates the Branch config file.
func LoadBranch() (*BranchConfig, error) {
	dir, err := configDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "branch.json")

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &BranchConfig{
				Port:      1950,
				ServerURL: DefaultServerURL,
			}, nil
		}
		return nil, err
	}

	var cfg BranchConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if cfg.Port == 0 {
		cfg.Port = 1950
	}
	return &cfg, nil
}

// SaveBranch persists the Branch config to disk.
func SaveBranch(cfg *BranchConfig) error {
	dir, err := configDir()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "branch.json"), data, 0600)
}
