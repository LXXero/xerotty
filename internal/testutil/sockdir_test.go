package testutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LXXero/xerotty/internal/sockpath"
)

// TestSockDirFallsBackWhenTmpUnwritable: in a sandbox where /tmp is
// read-only, SockDir must still hand back a usable directory (under
// os.TempDir()) instead of failing every socket test.
func TestSockDirFallsBackWhenTmpUnwritable(t *testing.T) {
	old := sockBase
	t.Cleanup(func() { sockBase = old })
	sockBase = filepath.Join(t.TempDir(), "missing")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	dir := SockDir(t)
	if !strings.HasPrefix(dir, tmp+string(os.PathSeparator)) {
		t.Fatalf("SockDir = %q, want a dir under TMPDIR %q", dir, tmp)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("SockDir %q not a directory (err=%v)", dir, err)
	}
}

// TestSockDirIsolatesRuntimeDir: default socket paths (the GUI's
// aggregating MCP socket) must land in the test's dir, not the real
// $XDG_RUNTIME_DIR where a live GUI already listens.
func TestSockDirIsolatesRuntimeDir(t *testing.T) {
	dir := SockDir(t)
	if got := filepath.Dir(sockpath.GUIMCPSocket()); got != dir {
		t.Fatalf("GUIMCPSocket dir = %q, want %q", got, dir)
	}
}
