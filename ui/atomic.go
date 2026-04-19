package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// atomicWriteFile writes data to path via tmp + rename so a SIGKILL / power
// loss / OOM mid-write never leaves a truncated file on disk. Matches the
// pattern already used in auth/auth.go for credentials and sessions.
//
// Uses a random tmp-file suffix so concurrent writers to the same path
// don't clobber each other's intermediate state. Without the suffix, two
// goroutines racing on (e.g.) SetContainerStatus → container_status.json
// could interleave Remove / WriteFile / Rename calls and one would get
// ENOENT on rename. Random suffix makes each write's tmp file unique;
// the last Rename wins deterministically (last-writer-wins, same as any
// concurrent save semantics).
//
// Caller is responsible for any subsequent os.Chown — rename preserves the
// inode's uid/gid from the tmp file, which is whatever umask gave us.
func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("atomic-write: mkdir %s: %w", dir, err)
	}
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return fmt.Errorf("atomic-write: random suffix: %w", err)
	}
	tmp := path + "." + hex.EncodeToString(suffix) + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return fmt.Errorf("atomic-write: write tmp %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("atomic-write: rename %s→%s: %w", tmp, path, err)
	}
	return nil
}
