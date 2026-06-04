// Package launchipc implements the single-instance launch socket: a
// session-scoped unix socket the GUI listens on so that later plain
// `xerotty` invocations open a window/tab in the EXISTING GUI instead
// of spawning a whole second GUI process attached to the same daemon.
//
// Kept free of cgo and internal/app imports on purpose: the client
// side (Forward) runs in cmd/xerotty before any GUI code is touched,
// including in `-tags headless` builds.
package launchipc

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/LXXero/xerotty/internal/sockpath"
)

// Request is one forwarded launch. Action is "window" or "tab"; CWD is
// the invoking process's working directory so the new shell opens
// where the user ran `xerotty`, not wherever the GUI happens to live.
type Request struct {
	Action string `json:"action"`
	CWD    string `json:"cwd,omitempty"`
}

type response struct {
	OK  bool   `json:"ok"`
	Err string `json:"err,omitempty"`
}

// dialTimeout bounds both the liveness probe and the forward itself.
// The GUI answers from a dedicated accept goroutine, so anything
// slower than this means a dead/wedged socket — fall through and
// launch a fresh GUI rather than hang the user's shell.
const dialTimeout = 500 * time.Millisecond

// SocketPath returns the session-scoped launch socket path. ok=false
// means there is no usable session identity (Linux with neither
// WAYLAND_DISPLAY nor DISPLAY set — e.g. a bare TTY or ssh shell), in
// which case forwarding is skipped entirely and `xerotty` behaves as
// before. The session key keeps a GUI on wayland-0 from swallowing
// launches meant for a nested compositor or an X11 session, and vice
// versa. On macOS there is one Aqua session per user, so a fixed key
// is enough ($TMPDIR is already per-user there).
func SocketPath() (path string, ok bool) {
	var key string
	switch {
	case os.Getenv("WAYLAND_DISPLAY") != "":
		key = "wl-" + sanitize(os.Getenv("WAYLAND_DISPLAY"))
	case os.Getenv("DISPLAY") != "":
		key = "x11-" + sanitize(os.Getenv("DISPLAY"))
	case runtime.GOOS == "darwin":
		key = "aqua"
	default:
		return "", false
	}
	// sockpath.RuntimeDir is the shared placement convention for all
	// xerotty sockets — env-independent on macOS, where $TMPDIR
	// differs between launch contexts (a Finder-launched GUI vs a
	// shell-launched `xerotty -t` would otherwise compute different
	// paths and never find each other).
	return filepath.Join(sockpath.RuntimeDir(), "xerotty-gui-"+key+".sock"), true
}

// sanitize makes a display name filesystem-safe. DISPLAY values like
// ":0" or "host:1.0" contain characters we don't want in a socket
// filename; WAYLAND_DISPLAY can in rare setups be an absolute path.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ':', '/', '\\':
			return '_'
		}
		return r
	}, s)
}

// Forward sends req to the GUI listening on the session socket and
// waits for its ack. A nil error means the running GUI accepted the
// request and the caller should exit instead of launching; ANY error
// means "no usable GUI here" and the caller falls through to a normal
// launch — forwarding is best-effort by design.
func Forward(req Request) error {
	path, ok := SocketPath()
	if !ok {
		return fmt.Errorf("no session identity")
	}
	conn, err := net.DialTimeout("unix", path, dialTimeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(dialTimeout))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return err
	}
	var resp response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("gui refused: %s", resp.Err)
	}
	return nil
}

// Listen binds the session launch socket and serves forwarded
// requests, handing each decoded Request to enqueue (called from the
// accept goroutine — the implementation must be thread-safe and must
// NOT touch GUI state directly). Returns a cleanup func.
//
// A second GUI in the same session (`xerotty --separate`) finds the
// socket alive and simply doesn't serve: first GUI wins, silently.
// A stale socket file (GUI crashed) is detected by a probe dial and
// reclaimed.
func Listen(enqueue func(Request)) (cleanup func(), err error) {
	path, ok := SocketPath()
	if !ok {
		return func() {}, nil // no session identity: nothing to serve
	}
	if conn, err := net.DialTimeout("unix", path, dialTimeout); err == nil {
		conn.Close()
		return func() {}, nil // live owner exists (--separate case)
	}
	_ = os.Remove(path) // stale or absent; either way safe to reclaim
	ln, err := net.Listen("unix", path)
	if err != nil {
		return func() {}, err
	}
	// The socket grants "open a shell window as this user" — keep it
	// owner-only even under a loose umask or /tmp fallback.
	_ = os.Chmod(path, 0o600)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed (shutdown)
			}
			go serveConn(conn, enqueue)
		}
	}()
	return func() {
		ln.Close()
		os.Remove(path)
	}, nil
}

func serveConn(conn net.Conn, enqueue func(Request)) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(dialTimeout))
	var req Request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		_ = json.NewEncoder(conn).Encode(response{Err: "bad request"})
		return
	}
	switch req.Action {
	case "window", "tab":
		enqueue(req)
		_ = json.NewEncoder(conn).Encode(response{OK: true})
	default:
		_ = json.NewEncoder(conn).Encode(response{Err: "unknown action"})
	}
}
