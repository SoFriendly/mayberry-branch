package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Check that the new executable can start on this OS before touching the
// working one. The version command does not load configuration or start a
// daemon. This is an availability check, not release signature verification.
func checkUpdateExecutable(path, version string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil {
		return fmt.Errorf("new executable cannot run on this system: %w", err)
	}
	if strings.TrimSpace(string(out)) != "mayberry "+version {
		return fmt.Errorf("new executable does not match advertised version")
	}
	return nil
}

// Windows cannot overwrite the running executable. Restore the old path if
// the second rename fails, so the installed service can still restart.
func replaceExecutable(current, candidate string, windows bool) error {
	if !windows {
		return os.Rename(candidate, current)
	}
	backup := current + ".old"
	if err := os.Remove(backup); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(current, backup); err != nil {
		return err
	}
	if err := os.Rename(candidate, current); err != nil {
		if restore := os.Rename(backup, current); restore != nil {
			return fmt.Errorf("replace: %v; restore: %v; original remains at %s", err, restore, backup)
		}
		return err
	}
	return nil
}
