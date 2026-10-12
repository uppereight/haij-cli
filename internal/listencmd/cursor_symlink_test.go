package listencmd

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWriteCursorDoesNotFollowSymlinks: the cursor was written through
// a fixed .haij/listen-cursor.tmp, and os.WriteFile follows a symlink
// there, so a checkout that ships one (e.g. -> ~/.bashrc) made
// `haij listen` overwrite the target. The write must leave it alone.
func TestWriteCursorDoesNotFollowSymlinks(t *testing.T) {
	cwd := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cwd, ".haij")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"listen-cursor.tmp", "listen-cursor"} {
		if err := os.Symlink(victim, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}

	if err := writeCursor(cwd, "evt_123"); err != nil {
		t.Fatalf("writeCursor: %v", err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "precious" {
		t.Errorf("symlink target overwritten: %q", b)
	}
	if got := readCursor(cwd); got != "evt_123" {
		t.Errorf("cursor after write = %q, want evt_123", got)
	}
	if fi, err := os.Lstat(filepath.Join(dir, "listen-cursor")); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("listen-cursor is not a regular file after the write (%v, %v)", fi, err)
	}
}

// TestReadCursorIgnoresSymlinks: a cursor file that is a symlink is not
// read, so its target's contents never reach the server as a cursor.
func TestReadCursorIgnoresSymlinks(t *testing.T) {
	cwd := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("do-not-send"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cwd, ".haij")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "listen-cursor")); err != nil {
		t.Fatal(err)
	}
	if got := readCursor(cwd); got != "" {
		t.Errorf("readCursor followed a symlink and returned %q", got)
	}
}
