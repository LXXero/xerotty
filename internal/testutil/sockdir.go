// Package testutil holds helpers shared by the test suites. Nothing
// here is linked into the binary.
package testutil

import (
	"os"
	"testing"

	"github.com/LXXero/xerotty/internal/sockpath"
)

// sockBase is where SockDir first tries to make its directory. A
// variable so the test can point it at an unwritable path.
var sockBase = "/tmp"

// SockDir returns a fresh directory for a test's unix sockets and
// points xerotty's socket-path recordings at it.
//
// Not t.TempDir(): that nests the test name under $TMPDIR, which on
// macOS is already ~50 bytes of /var/folders/..., so any descriptively
// named test blows past sun_path's 104 bytes and every dial fails
// with EINVAL ("connect: invalid argument"). /tmp keeps the whole
// socket path under 40 bytes on every platform. Where /tmp is not
// writable (a read-only sandbox) it falls back to os.TempDir(), which
// such sandboxes point at a writable, usually short, directory.
//
// The recording redirect (XEROTTY_CACHE_DIR) matters because each MCP
// server start Records where it bound: without it a test run
// overwrote the recording the developer's live GUI/daemon had left,
// and `xerotty mcp` stopped finding them until the next daemon start.
// XDG_RUNTIME_DIR is redirected for the same reason: code that binds a
// default socket (the GUI's aggregating MCP server in internal/app)
// would otherwise bind in the real runtime dir and collide with the
// developer's live GUI ("socket ... already in use").
// It rides on t.Setenv, so callers must not be t.Parallel() tests.
func SockDir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp(sockBase, "xt")
	if err != nil {
		dir, err = os.MkdirTemp("", "xt")
	}
	if err != nil {
		t.Fatalf("testutil.SockDir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv(sockpath.EnvCacheDir, dir)
	t.Setenv("XDG_RUNTIME_DIR", dir)
	return dir
}
