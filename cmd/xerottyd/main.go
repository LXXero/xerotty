// xerottyd is the headless xerotty daemon — owns terminal sessions,
// serves UI clients (and eventually MCP agents) over a structured
// protocol. See docs/DAEMON_PLAN.md for design.
//
// Phase 0 supports:
//   - listening on a unix socket
//   - a single named session ("default") with N tabs
//   - one or more UI clients attaching, each driving the same
//     tabs (last-writer-wins for input; cooperative for now)
//
// Usage:
//
//	xerottyd                              # listen on default socket
//	xerottyd --socket /path/to/sock        # explicit socket path
//	xerottyd --stdio                       # serve one client on
//	                                       # stdin/stdout (for SSH transport)
//
// Default socket path: $XDG_RUNTIME_DIR/xerottyd.sock, falling back
// to /tmp/xerottyd-$UID.sock if XDG_RUNTIME_DIR isn't set.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/protocol"
)

func main() {
	var socketPath string
	var stdio bool
	flag.StringVar(&socketPath, "socket", "", "unix socket path to listen on (default: $XDG_RUNTIME_DIR/xerottyd.sock)")
	flag.BoolVar(&stdio, "stdio", false, "serve one client on stdin/stdout (for SSH transport)")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "xerottyd: config error: %v\n", err)
		os.Exit(1)
	}

	if stdio {
		// Stdio mode: this process serves exactly one client over its
		// own stdin/stdout. Typical caller is `ssh host xerottyd
		// --stdio` from a client wanting a remote daemon — the SSH
		// pipe IS the transport, no listener, no socket file.
		//
		// We DO still construct a Daemon (the session machinery is
		// shared with the socket path), but skip Run() — Run wants to
		// bind a listener which we don't need. ServeConn does the
		// per-connection work directly.
		//
		// stderr stays available for logs; the protocol owns stdin
		// and stdout exclusively. Don't print to stdout under any
		// circumstance in stdio mode.
		d := daemon.New(&cfg, "")
		conn := protocol.NewStdioConn(os.Stdin, os.Stdout)
		fmt.Fprintln(os.Stderr, "xerottyd: serving one client on stdio")
		d.ServeConn(conn)
		fmt.Fprintln(os.Stderr, "xerottyd: stdio client disconnected")
		return
	}

	if socketPath == "" {
		socketPath = defaultSocketPath()
	}

	d := daemon.New(&cfg, socketPath)

	// Graceful shutdown on SIGINT/SIGTERM.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "xerottyd: shutting down")
		_ = d.Stop()
	}()

	fmt.Fprintf(os.Stderr, "xerottyd: listening on %s\n", socketPath)
	// Also print on stdout so callers parsing the output (auto-spawn
	// from xerotty) can locate the socket without parsing stderr.
	fmt.Println(socketPath)

	if err := d.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "xerottyd: %v\n", err)
		os.Exit(1)
	}
}

// defaultSocketPath picks a per-user, per-machine path for the
// daemon's unix socket. Prefers XDG_RUNTIME_DIR (which has the right
// permissions + lifetime). Falls back to /tmp with the UID baked in
// to avoid collisions on multi-user boxes.
func defaultSocketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "xerottyd.sock")
	}
	return filepath.Join(os.TempDir(), "xerottyd-"+strconv.Itoa(os.Getuid())+".sock")
}
