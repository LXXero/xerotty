// Package runner holds the implementation of xerotty's non-GUI
// subcommands (serve, connect). Kept separate from cmd/xerotty so
// the GUI binary can dispatch into them without each subcommand
// being its own binary.
package runner

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/mcp"
	"github.com/LXXero/xerotty/internal/protocol"
	"github.com/LXXero/xerotty/internal/sockpath"
)

// Serve runs the `xerotty serve` subcommand: a headless daemon that
// owns PTYs, sessions, and the wire protocol socket + the MCP
// agent socket. args is the slice AFTER the subcommand name, so
// flag.NewFlagSet can parse it cleanly.
//
//	xerotty serve                              # listen on default sockets
//	xerotty serve --socket /path/to/sock        # explicit wire socket
//	xerotty serve --stdio                       # serve one client on
//	                                            # stdin/stdout (SSH transport)
//	xerotty serve --no-mcp                      # disable MCP agent socket
//
// Default socket path: $XDG_RUNTIME_DIR/xerottyd.sock, falling back
// to /tmp/xerottyd-$UID.sock if XDG_RUNTIME_DIR isn't set. The MCP
// socket lives alongside it with a .mcp.sock suffix.
func Serve(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var socketPath string
	var mcpSocketPath string
	var noMCP bool
	var stdio bool
	fs.StringVar(&socketPath, "socket", "", "unix socket path (default: $XDG_RUNTIME_DIR/xerottyd.sock)")
	fs.StringVar(&mcpSocketPath, "mcp-socket", "", "MCP socket path for AI agents (default: alongside --socket)")
	fs.BoolVar(&noMCP, "no-mcp", false, "disable the MCP agent socket entirely")
	fs.BoolVar(&stdio, "stdio", false, "bridge stdin/stdout to the persistent daemon socket (for SSH transport). Auto-spawns a daemon if none is running.")
	var stdioEphemeral bool
	fs.BoolVar(&stdioEphemeral, "stdio-ephemeral", false, "old --stdio behavior: serve one client on stdin/stdout from an in-process daemon that dies with the connection. Loses tabs on disconnect.")
	var resumeFile string
	fs.StringVar(&resumeFile, "resume", "", "resume from a hot-upgrade handoff file (internal: set by the exec-in-place upgrade)")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "xerotty serve: config error: %v\n", err)
		return 1
	}

	if stdioEphemeral {
		// Old behavior: serve one client out of an in-process
		// daemon that dies with the connection. Tabs do NOT
		// survive disconnect. Useful for one-off scripted runs
		// where persistence is undesirable.
		d := daemon.New(&cfg, "")
		conn := protocol.NewStdioConn(os.Stdin, os.Stdout)
		fmt.Fprintln(os.Stderr, "xerotty serve: ephemeral stdio mode, one client")
		d.ServeConn(conn)
		fmt.Fprintln(os.Stderr, "xerotty serve: stdio client disconnected")
		return 0
	}

	if stdio {
		// Bridge mode: connect to (or auto-spawn) a persistent
		// daemon on the local box, then proxy bytes between our
		// stdin/stdout and the daemon's unix socket. This is what
		// `ssh host xerotty serve --stdio` should do: when you
		// reconnect later your tabs are still there because the
		// remote-side daemon outlives the SSH connection.
		target := socketPath
		if target == "" {
			target = defaultSocketPath()
		}
		return runStdioBridge(target)
	}

	if socketPath == "" {
		socketPath = defaultSocketPath()
	}

	d := daemon.New(&cfg, socketPath)

	if resumeFile != "" {
		if err := resumeFromFile(d, resumeFile); err != nil {
			fmt.Fprintf(os.Stderr, "xerotty serve: resume: %v\n", err)
			// Carry on as a fresh daemon: a partial resume already
			// adopted what it could; a failed parse adopted nothing.
		} else {
			fmt.Fprintln(os.Stderr, "xerotty serve: resumed session from hot upgrade")
		}
	}

	var mcpSrv *mcp.Server
	if !noMCP {
		if mcpSocketPath == "" {
			mcpSocketPath = defaultMCPSocketPath(socketPath)
		}
		mcpSrv = mcp.New(d, mcpSocketPath)
		go func() {
			fmt.Fprintf(os.Stderr, "xerotty serve: MCP listening on %s\n", mcpSocketPath)
			if err := mcpSrv.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "xerotty serve: mcp: %v\n", err)
			}
		}()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "xerotty serve: shutting down")
		if mcpSrv != nil {
			_ = mcpSrv.Stop()
		}
		_ = d.Stop()
	}()

	upgrading := upgradeOnSignal(d, mcpSrv, socketPath, mcpSocketPath)

	fmt.Fprintf(os.Stderr, "xerotty serve: listening on %s\n", socketPath)
	fmt.Println(socketPath) // stdout so auto-spawn can locate the socket
	err = d.Run()
	select {
	case <-upgrading:
		// An exec-in-place upgrade owns the process now: quiesce
		// stopped the listener (that's why Run returned) and the
		// upgrade goroutine is between serialize and exec. Park —
		// the exec replaces this image, or the goroutine exits the
		// process itself on failure.
		select {}
	default:
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "xerotty serve: %v\n", err)
		return 1
	}
	return 0
}

// defaultSocketPath picks a per-user, per-machine path for the
// daemon's unix socket. Prefers XDG_RUNTIME_DIR (right perms +
// lifetime). Falls back to /tmp with UID baked in for multi-user
// boxes. Name kept as "xerottyd.sock" — the wire format hasn't
// changed, this is just the same daemon under a different binary.
func defaultSocketPath() string {
	return sockpath.DaemonSocket()
}

// defaultMCPSocketPath derives the MCP socket path from the main
// socket: same dir, .mcp.sock suffix appended before .sock.
func defaultMCPSocketPath(mainSocket string) string {
	return sockpath.MCPSocketFor(mainSocket)
}

// DefaultSocketPath is the exported flavor of defaultSocketPath so
// the connect subcommand can share the same default.
func DefaultSocketPath() string { return defaultSocketPath() }
