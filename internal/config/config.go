package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// BranchConfig holds local Branch daemon configuration.
type BranchConfig struct {
	BranchID      string `json:"branch_id,omitempty"`
	FriendlyID    string `json:"friendly_id"`
	DisplayName   string `json:"display_name"`
	Subdomain     string `json:"subdomain"`
	LibraryPath   string `json:"library_path"`             // EPUBs
	AudiobookPath string `json:"audiobook_path,omitempty"` // M4Bs (optional)
	Port          int    `json:"port"`
	ServerURL     string `json:"server_url"`

	// Network mirror — see MIRROR.md.
	MirrorNetwork   bool     `json:"mirror_network"`
	MirrorSize      string   `json:"mirror_size"`       // e.g. "100G", "500M"
	MirrorOnly      []string `json:"mirror_only"`       // exclusive allowlist of subdomains
	MirrorIgnore    []string `json:"mirror_ignore"`     // blocklist of subdomains
	MirrorRate      string   `json:"mirror_rate"`       // "slow" | "normal" | "fast"
	MirrorServeRate string   `json:"mirror_serve_rate"` // outbound cap when serving mirror requests, e.g. "200K"
}


const (
	DefaultServerURL = "https://mayberry.pub"
	DefaultHubURL    = "https://branch.pub"

	DefaultMirrorSize      = "100G"
	DefaultMirrorRate      = "slow"
	DefaultMirrorServeRate = "200K"
)

// ValidMirrorRates lists the accepted mirror-rate presets.
var ValidMirrorRates = []string{"slow", "normal", "fast"}

// IsValidMirrorRate reports whether s is one of the accepted rate presets.
func IsValidMirrorRate(s string) bool {
	for _, r := range ValidMirrorRates {
		if s == r {
			return true
		}
	}
	return false
}

// ParseSize parses a size string like "100G", "500M", "10K", "1024" and
// returns the value in bytes. Suffixes are case-insensitive; K/M/G/T use
// 1024-based units. A bare integer is treated as bytes.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	mult := int64(1)
	switch last := s[len(s)-1]; last {
	case 'k', 'K':
		mult = 1024
		s = s[:len(s)-1]
	case 'm', 'M':
		mult = 1024 * 1024
		s = s[:len(s)-1]
	case 'g', 'G':
		mult = 1024 * 1024 * 1024
		s = s[:len(s)-1]
	case 't', 'T':
		mult = 1024 * 1024 * 1024 * 1024
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("negative size %q", s)
	}
	return n * mult, nil
}

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
			cfg := &BranchConfig{
				Port:      1950,
				ServerURL: DefaultServerURL,
			}
			applyDefaults(cfg)
			return cfg, nil
		}
		return nil, err
	}

	var cfg BranchConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	applyDefaults(&cfg)
	return &cfg, nil
}

// applyDefaults fills in zero-valued fields with their defaults. Called from
// LoadBranch so existing configs pick up new fields without manual editing.
func applyDefaults(cfg *BranchConfig) {
	if cfg.Port == 0 {
		cfg.Port = 1950
	}
	if cfg.MirrorSize == "" {
		cfg.MirrorSize = DefaultMirrorSize
	}
	if cfg.MirrorRate == "" {
		cfg.MirrorRate = DefaultMirrorRate
	}
	if cfg.MirrorServeRate == "" {
		cfg.MirrorServeRate = DefaultMirrorServeRate
	}
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
