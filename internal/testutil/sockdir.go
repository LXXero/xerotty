// Package testutil holds helpers shared by the test suites. Nothing
// here is linked into the binary.
package testutil

import (
	"os"
	"testing"

	"github.com/LXXero/xerotty/internal/sockpath"
)

// SockDir returns a fresh directory for a test's unix sockets and
// points xerotty's socket-path recordings at it.
//
// Not t.TempDir(): that nests the test name under $TMPDIR, which on
// macOS is already ~50 bytes of /var/folders/..., so any descriptively
// named test blows past sun_path's 104 bytes and every dial fails
// with EINVAL ("connect: invalid argument"). /tmp keeps the whole
// socket path under 40 bytes on every platform.
//
// The recording redirect (XEROTTY_CACHE_DIR) matters because each MCP
// server start Records where it bound: without it a test run
// overwrote the recording the developer's live GUI/daemon had left,
// and `xerotty mcp` stopped finding them until the next daemon start.
// It rides on t.Setenv, so callers must not be t.Parallel() tests.
func SockDir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "xt")
	if err != nil {
		t.Fatalf("testutil.SockDir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv(sockpath.EnvCacheDir, dir)
	return dir
}
