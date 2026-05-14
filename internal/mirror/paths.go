// Package mirror implements the network mirror feature: a branch
// opt-in to download other branches' books for failover availability.
// See MIRROR.md for the full design and security model.
package mirror

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MirrorDirName is the subfolder inside each library/audiobook root that
// holds mirrored content. Using a stable, recognizable name (rather than
// a hidden dotted dir) so users can see and manage it from their file
// browser. The leading underscore sorts it apart from real folders.
const MirrorDirName = "_mirror"

// StagingDirName lives inside MirrorDirName and holds in-flight downloads
// that haven't been validated yet. Dot-prefixed so it's hidden by default
// and ignored by the EPUB/audiobook scanner.
const StagingDirName = ".staging"

// EnsureMirrorRoot resolves rootDir/_mirror to an absolute path, creates
// it if missing, and verifies the result is a real directory (not a
// symlink at any level). Returns the absolute mirror root.
//
// Symlink rejection defends against an attacker who pre-plants a symlink
// from somewhere in the user's library to elsewhere on the filesystem,
// hoping our writes follow it. We rebuild the resolved path component by
// component with Lstat so any symlinked segment is caught.
func EnsureMirrorRoot(rootDir string) (string, error) {
	if rootDir == "" {
		return "", fmt.Errorf("mirror: root dir not configured")
	}
	abs, err := filepath.Abs(rootDir)
	if err != nil {
		return "", fmt.Errorf("mirror: abs root: %w", err)
	}
	mirror := filepath.Join(abs, MirrorDirName)
	if err := os.MkdirAll(mirror, 0700); err != nil {
		return "", fmt.Errorf("mirror: mkdir %s: %w", mirror, err)
	}
	if err := assertNoSymlinks(mirror, abs); err != nil {
		return "", err
	}
	// Staging dir lives inside the mirror root and shares its safety.
	staging := filepath.Join(mirror, StagingDirName)
	if err := os.MkdirAll(staging, 0700); err != nil {
		return "", fmt.Errorf("mirror: mkdir staging: %w", err)
	}
	return mirror, nil
}

// assertNoSymlinks walks from stopAt down to target ensuring no path
// component is a symlink. target must be a descendant of stopAt.
func assertNoSymlinks(target, stopAt string) error {
	target = filepath.Clean(target)
	stopAt = filepath.Clean(stopAt)
	for p := target; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil {
			return fmt.Errorf("mirror: lstat %s: %w", p, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("mirror: refusing to use %s — symlinked path component", p)
		}
		if p == stopAt || filepath.Dir(p) == p {
			return nil
		}
	}
}

// MirrorPath returns the final on-disk path for a mirrored file:
// {mirrorRoot}/<hex[0:2]>/<hex[2:4]>/<hex>.<ext>
//
// hex MUST be the SHA-256 we computed locally; the caller is responsible
// for never passing source-controlled data here. ext is similarly under
// our control (validator chose it from sniffed magic bytes), not the
// source's claim.
func MirrorPath(mirrorRoot, hex, ext string) (string, error) {
	if len(hex) < 4 {
		return "", fmt.Errorf("mirror: hash too short (%d chars)", len(hex))
	}
	// Defense in depth: the hash is hex from our own sha256, so this
	// shouldn't ever fail — but we'd rather error than write garbage paths.
	if !isHexLower(hex) {
		return "", fmt.Errorf("mirror: hash is not lowercase hex")
	}
	// Extension must start with "." and contain no path separators or
	// parent-traversal sequences. We construct ext ourselves from a
	// sniffed Kind, so this is belt-and-suspenders.
	if !strings.HasPrefix(ext, ".") || strings.Contains(ext, "..") || strings.ContainsAny(ext, "/\\") {
		return "", fmt.Errorf("mirror: bad extension %q", ext)
	}
	return filepath.Join(mirrorRoot, hex[0:2], hex[2:4], hex+ext), nil
}

func isHexLower(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return len(s) > 0
}

// IsMirrorPath reports whether path lives under any _mirror/ segment.
// Used by the scanner to flag holdings as mirror copies vs. originals.
// Works on both Unix and Windows paths since we split on the platform
// separator.
func IsMirrorPath(p string) bool {
	for _, part := range strings.Split(p, string(filepath.Separator)) {
		if part == MirrorDirName {
			return true
		}
	}
	return false
}

// ConfirmInsideMirror verifies that target resolves to a path inside
// mirrorRoot. Defense against a path-derivation bug accidentally
// producing an escape — even though MirrorPath only uses our own hash,
// we belt-and-suspenders this so a future refactor can't smuggle in
// untrusted data without tripping this check.
func ConfirmInsideMirror(mirrorRoot, target string) error {
	rel, err := filepath.Rel(mirrorRoot, target)
	if err != nil {
		return fmt.Errorf("mirror: rel failed: %w", err)
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("mirror: target %s escapes mirror root %s", target, mirrorRoot)
	}
	return nil
}
