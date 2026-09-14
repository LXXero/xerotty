// Package sockpath is the single source of truth for where
// xerotty's unix sockets live. Four packages used to carry their
// own copy of the "XDG_RUNTIME_DIR or temp fallback" logic — and
// the fallback used os.TempDir(), which on macOS reads $TMPDIR, a
// per-launch-context /var/folders/... path. A Finder-launched GUI
// (launchd's TMPDIR) and an MCP bridge spawned by an agent (different
// or empty TMPDIR) would compute different "defaults" and never find
// each other's sockets. Every socket-path decision goes through
// RuntimeDir so that can't happen again.
package sockpath

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// RuntimeDir returns the directory xerotty sockets are created in.
// XDG_RUNTIME_DIR when set (Linux sessions — right perms, right
// lifetime). Otherwise a deterministic, env-independent
// /tmp/xerotty-<uid> created 0700, tmux-style: /tmp's sticky bit
// stops other users renaming it, and the ownership check below
// refuses a dir someone else squatted.
func RuntimeDir() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return dir
	}
	dir := filepath.Join("/tmp", "xerotty-"+strconv.Itoa(os.Getuid()))
	if err := os.MkdirAll(dir, 0o700); err == nil {
		if fi, err := os.Stat(dir); err == nil {
			if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) == os.Getuid() && fi.IsDir() {
				// Re-tighten in case the dir predates us with looser
				// perms (MkdirAll doesn't chmod existing dirs).
				_ = os.Chmod(dir, 0o700)
				return dir
			}
		}
	}
	// Squatted or unwritable — fall back to the old per-process temp
	// dir rather than putting sockets somewhere another user owns.
	return os.TempDir()
}

// DaemonSocket is the default wire-protocol socket for the local
// daemon ($RUNTIME/xerottyd.sock).
func DaemonSocket() string {
	return filepath.Join(RuntimeDir(), "xerottyd.sock")
}

// GUIMCPSocket is the GUI's aggregating MCP socket
// ($RUNTIME/xerotty-gui.mcp.sock).
func GUIMCPSocket() string {
	return filepath.Join(RuntimeDir(), "xerotty-gui.mcp.sock")
}

// MCPSocketFor derives an MCP socket path from a daemon's main wire
// socket: same dir, .mcp.sock suffix replacing .sock.
func MCPSocketFor(mainSocket string) string {
	dir := filepath.Dir(mainSocket)
	name := filepath.Base(mainSocket)
	if strings.HasSuffix(name, ".sock") {
		name = strings.TrimSuffix(name, ".sock") + ".mcp.sock"
	} else {
		name += ".mcp.sock"
	}
	return filepath.Join(dir, name)
}

// --- Socket-path recording ---
//
// Computing default paths on both sides only works when both sides
// run with the same environment AND the socket is at its default
// location. So the bind side also RECORDS where it actually
// listened, in a $HOME-anchored file (os.UserCacheDir — identical
// under launchd, login shells, and agent-spawned processes), and
// dial-side consumers like `xerotty mcp` read the recording first.
// Recordings can go stale (crash, reboot) — readers must treat them
// as candidates to dial-verify, never as truth.

// Names for Record/Recorded.
const (
	RecordGUIMCP    = "gui-mcp"    // the GUI's aggregating MCP socket
	RecordDaemonMCP = "daemon-mcp" // the local daemon's MCP socket
)

// EnvCacheDir relocates xerotty's per-user cache dir (socket-path
// recordings, the daemon log). Same story as config.EnvConfigDir:
// os.UserCacheDir ignores XDG_CACHE_HOME on macOS, and every MCP
// server start Records its socket — so `go test ./...` on a Mac used
// to overwrite the recording the live GUI/daemon had left, and
// `xerotty mcp` lost them until the next daemon start.
const EnvCacheDir = "XEROTTY_CACHE_DIR"

// cacheDir returns $XEROTTY_CACHE_DIR or <os.UserCacheDir>/xerotty,
// created 0700.
func cacheDir() (string, error) {
	dir := os.Getenv(EnvCacheDir)
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(base, "xerotty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func recordPath(name string) (string, error) {
	dir, err := cacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".path"), nil
}

// Record notes that the socket for `name` is listening at sock.
// Best-effort: failure to record only degrades discovery back to
// the computed defaults, so callers may ignore the error.
func Record(name, sock string) error {
	p, err := recordPath(name)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(sock+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Recorded returns the last recorded socket path for `name`, or ""
// if nothing was recorded. The caller must verify it's live (dial
// it) — recordings outlive the processes that wrote them.
func Recorded(name string) string {
	p, err := recordPath(name)
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// DaemonLogFile opens the canonical daemon log
// (<UserCacheDir>/xerotty/xerottyd.log, append mode, truncated when
// it grows past ~5MB). Spawned daemons MUST point stderr here rather
// than inheriting the spawner's: an ssh-spawned daemon that inherits
// the ssh session's pipe dies of SIGPIPE on its FIRST log line after
// that session ends (Go intentionally re-raises EPIPE on fds 1/2) —
// which silently killed remote daemons that were supposed to outlive
// their client, and murdered one mid-`serve --upgrade`. Returns nil
// (caller should treat as /dev/null) when the cache dir is unusable.
func DaemonLogFile() *os.File {
	d, err := cacheDir()
	if err != nil {
		return nil
	}
	path := filepath.Join(d, "xerottyd.log")
	if st, err := os.Stat(path); err == nil && st.Size() > 5<<20 {
		_ = os.Truncate(path, 0)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil
	}
	return f
}
