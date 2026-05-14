package mirror

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// StageFile opens a fresh staging file under mirrorRoot/.staging/ for an
// in-flight download. Returns the file handle (caller writes to it),
// its path, and a cleanup func that removes the file if it's still there.
//
// Files in staging have NOT been validated. The scanner ignores this
// directory because it starts with a dot. We unlink on any error so
// failed downloads leave no residue.
func StageFile(mirrorRoot string) (*os.File, string, func(), error) {
	stagingDir := filepath.Join(mirrorRoot, StagingDirName)
	if err := os.MkdirAll(stagingDir, 0700); err != nil {
		return nil, "", nil, fmt.Errorf("mirror: staging mkdir: %w", err)
	}
	var name [16]byte
	if _, err := rand.Read(name[:]); err != nil {
		return nil, "", nil, fmt.Errorf("mirror: random name: %w", err)
	}
	stagingPath := filepath.Join(stagingDir, hex.EncodeToString(name[:])+".tmp")
	// O_EXCL guards against the absurdly unlikely random collision.
	f, err := os.OpenFile(stagingPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, "", nil, fmt.Errorf("mirror: staging open: %w", err)
	}
	cleanup := func() {
		_ = os.Remove(stagingPath)
	}
	return f, stagingPath, cleanup, nil
}

// Promote moves a validated staging file into its final mirror location.
// Steps (each guards against a distinct attack):
//
//  1. Verify final is inside mirrorRoot (path containment).
//  2. Create the parent fanout dir without following symlinks at any
//     component, then Lstat it to confirm we ended up at a real dir.
//  3. Atomic rename. If a file with the same name already exists (same
//     content hash already mirrored), rename will overwrite it on Unix
//     — that's fine, the bytes are identical.
//
// On any error the staging file is left in place; the caller's cleanup
// is responsible for removing it.
func Promote(mirrorRoot, stagingPath, finalPath string) error {
	if err := ConfirmInsideMirror(mirrorRoot, finalPath); err != nil {
		return err
	}
	parent := filepath.Dir(finalPath)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return fmt.Errorf("mirror: mkdir parent: %w", err)
	}
	// Re-check after MkdirAll: an attacker who could pre-plant the fanout
	// dirs as symlinks (e.g. via another process running as the same user)
	// would have escaped detection at EnsureMirrorRoot time. assertNoSymlinks
	// climbs from parent up to mirrorRoot.
	if err := assertNoSymlinks(parent, mirrorRoot); err != nil {
		return err
	}
	if err := os.Rename(stagingPath, finalPath); err != nil {
		return fmt.Errorf("mirror: rename: %w", err)
	}
	return nil
}
