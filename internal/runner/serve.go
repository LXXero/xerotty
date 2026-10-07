// Package runner holds the implementation of xerotty's non-GUI
// subcommands (serve, connect). Kept separate from cmd/xerotty so
// the GUI binary can dispatch into them without each subcommand
// being its own binary.
package runner

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/handoff"
	"github.com/LXXero/xerotty/internal/mcp"
	"github.com/LXXero/xerotty/internal/protocol"
	"github.com/LXXero/xerotty/internal/sockpath"
	"github.com/LXXero/xerotty/internal/supervise"
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
	var validateHandoff string
	fs.StringVar(&validateHandoff, "validate-handoff", "", "validate a handoff file and exit (internal: the upgrade's pre-exec gate)")
	var doUpgrade bool
	fs.BoolVar(&doUpgrade, "upgrade", false, "hot-upgrade the RUNNING daemon to the currently-installed binary (shells survive), then exit")
	var force bool
	fs.BoolVar(&force, "force", false, "with --upgrade: upgrade even when the daemon already runs the installed binary (to exercise the upgrade path)")
	var child bool
	var listenFD, controlFD int
	var noSupervisor bool
	fs.BoolVar(&child, "child", false, "run as a supervised daemon child (internal: set by the supervisor)")
	fs.IntVar(&listenFD, "listen-fd", -1, "inherited wire listener fd (internal: set by the supervisor)")
	fs.IntVar(&controlFD, "control-fd", -1, "inherited supervisor control fd (internal: set by the supervisor)")
	fs.BoolVar(&noSupervisor, "no-supervisor", false, "run the daemon as a single process: a crash loses every session (the pre-supervisor behavior)")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	if validateHandoff != "" {
		// Pre-exec gate: prove this binary parses + version-accepts
		// the handoff format. Exit code is the whole contract.
		if _, err := handoff.ReadFile(validateHandoff); err != nil {
			log.Printf("xerotty serve: %v", err)
			return 1
		}
		return 0
	}

	if doUpgrade {
		target := socketPath
		if target == "" {
			target = defaultSocketPath()
		}
		return upgradeCLI(target, force)
	}

	cfg, err := config.Load()
	if err != nil {
		log.Printf("xerotty serve: config error: %v", err)
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
	if !noMCP && mcpSocketPath == "" {
		mcpSocketPath = defaultMCPSocketPath(socketPath)
	}

	if !child && !noSupervisor {
		// Default: the supervisor owns the listener and this process;
		// the daemon proper runs as its child (see
		// docs/CRASH_RESTORE_PLAN.md). With --resume this is an
		// exec-in-place upgrade of an unsupervised daemon: the
		// supervisor adopts the handoff and the sessions move into a
		// child. Everything below this block is the child's (or an
		// unsupervised daemon's) path.
		return runSupervisor(socketPath, mcpSocketPath, noMCP, resumeFile)
	}

	d := daemon.New(&cfg, socketPath)

	var supClient *supervise.Client
	if controlFD >= 0 {
		c, err := supervise.NewClient(os.NewFile(uintptr(controlFD), "supervisor-control"))
		if err != nil {
			log.Printf("xerotty serve: %v (running unsupervised)", err)
		} else {
			supClient = c
			d.SetSupervisor(c)
		}
	}

	var inheritedLn net.Listener
	if resumeFile != "" {
		ln, err := resumeFromFile(d, resumeFile)
		if err != nil {
			log.Printf("xerotty serve: resume: %v", err)
			// Carry on as a fresh daemon: a partial resume already
			// adopted what it could; a failed parse adopted nothing.
		} else {
			inheritedLn = ln
			fmt.Fprintln(os.Stderr, "xerotty serve: resumed session from handoff")
		}
	}
	if inheritedLn == nil && listenFD >= 0 {
		f := os.NewFile(uintptr(listenFD), "wire-listener")
		ln, err := net.FileListener(f)
		_ = f.Close()
		if err != nil {
			log.Printf("xerotty serve: inherited listener: %v", err)
			return 1
		}
		inheritedLn = ln
	}

	var mcpSrv *mcp.Server
	if !noMCP {
		mcpSrv = mcp.New(d, mcpSocketPath)
		go func() {
			log.Printf("xerotty serve: MCP listening on %s", mcpSocketPath)
			if err := mcpSrv.Run(); err != nil {
				log.Printf("xerotty serve: mcp: %v", err)
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

	upgrading := upgradeOnSignal(d, mcpSrv, supClient, socketPath, mcpSocketPath)

	log.Printf("xerotty serve: listening on %s", socketPath)
	if !child {
		fmt.Println(socketPath) // stdout so auto-spawn can locate the socket
	}
	if inheritedLn != nil {
		err = d.RunWithListener(inheritedLn)
	} else {
		err = d.Run()
	}
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
		log.Printf("xerotty serve: %v", err)
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

// runSupervisor is the default `xerotty serve`: bind the wire socket,
// then run daemon children under internal/supervise until one exits
// cleanly. The socket path goes to stdout once, for auto-spawn.
func runSupervisor(socketPath, mcpSocketPath string, noMCP bool, resumeFile string) int {
	self, err := os.Executable()
	if err != nil {
		log.Printf("xerotty serve: locate self: %v", err)
		return 1
	}
	sup := supervise.New(supervise.Config{
		// The path, not the inode: children (and an upgrade's re-exec)
		// start whatever binary is installed there at that moment.
		Binary:        strings.TrimSuffix(self, " (deleted)"),
		SocketPath:    socketPath,
		MCPSocketPath: mcpSocketPath,
		NoMCP:         noMCP,
		Log:           os.Stderr,
		NoUpgrade:     os.Getenv("XEROTTY_TEST_LEGACY_SUPERVISOR") != "",
	})
	var lf *os.File
	if resumeFile != "" {
		st, err := handoff.ReadFile(resumeFile)
		_ = os.Remove(resumeFile)
		if err != nil {
			log.Printf("xerotty serve: adopt handoff: %v (starting fresh)", err)
		} else {
			lf = sup.AdoptHandoff(st)
			fmt.Fprintf(os.Stderr, "xerotty serve: supervisor adopted %d tabs from the upgrade handoff\n", len(st.Tabs))
		}
	}
	if lf == nil {
		ln, err := daemon.ListenSocket(socketPath)
		if err != nil {
			log.Printf("xerotty serve: %v", err)
			return 1
		}
		ul, ok := ln.(*net.UnixListener)
		if !ok {
			log.Printf("xerotty serve: %s is not a unix listener", socketPath)
			return 1
		}
		// Keep the socket file: closing the Go listener must not
		// unlink the path the children serve on through the
		// inherited fd.
		ul.SetUnlinkOnClose(false)
		lf, err = ul.File()
		_ = ul.Close()
		if err != nil {
			log.Printf("xerotty serve: listener fd: %v", err)
			return 1
		}
		fmt.Println(socketPath) // stdout so auto-spawn can locate the socket
	}
	sup.SetListener(lf)
	err = sup.Run()
	_ = os.Remove(socketPath)
	if err != nil {
		log.Printf("xerotty serve: %v", err)
		return 1
	}
	return 0
}
