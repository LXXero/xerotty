package runner

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
)

// GUIMCPSocketPath picks the GUI's aggregating MCP socket path:
// $XDG_RUNTIME_DIR/xerotty-gui.mcp.sock (or /tmp fallback). Lives
// here (not internal/app) so the headless build's `xerotty mcp`
// can derive it too; the GUI calls this when binding the server so
// the two sides can never drift.
func GUIMCPSocketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "xerotty-gui.mcp.sock")
	}
	return filepath.Join(os.TempDir(), "xerotty-gui-"+strconv.Itoa(os.Getuid())+".mcp.sock")
}

// MCPBridge implements `xerotty mcp`: a stdio <-> unix-socket bridge
// so MCP clients — which spawn a command and speak line-delimited
// JSON-RPC over its stdin/stdout — can talk to the MCP servers that
// live on unix sockets (the GUI's aggregator, or a daemon's own).
// One-time client setup is then just:
//
//	claude mcp add xerotty -- xerotty mcp
//
// Default target is the GUI aggregator (all hosts, namespaced tab
// IDs), falling back to the local daemon's MCP socket so the same
// command works on a headless server. Both sides of the bridge are
// newline-delimited JSON-RPC 2.0, so this is a verbatim byte pump.
func MCPBridge(args []string) int {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	var socketPath string
	var daemonOnly bool
	fs.StringVar(&socketPath, "socket", "", "MCP socket path to bridge to (default: GUI aggregator, then local daemon)")
	fs.BoolVar(&daemonOnly, "daemon", false, "bridge to the local daemon's MCP socket, skipping the GUI aggregator")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	var candidates []string
	switch {
	case socketPath != "":
		candidates = []string{socketPath}
	case daemonOnly:
		candidates = []string{defaultMCPSocketPath(defaultSocketPath())}
	default:
		candidates = []string{
			GUIMCPSocketPath(),
			defaultMCPSocketPath(defaultSocketPath()),
		}
	}

	var conn net.Conn
	for _, p := range candidates {
		c, err := net.Dial("unix", p)
		if err == nil {
			conn = c
			// stderr only — stdout belongs to the JSON-RPC stream.
			fmt.Fprintf(os.Stderr, "xerotty mcp: bridging stdio <-> %s\n", p)
			break
		}
	}
	if conn == nil {
		fmt.Fprintf(os.Stderr, "xerotty mcp: no MCP socket reachable (tried %v)\n", candidates)
		fmt.Fprintln(os.Stderr, "  start the GUI (aggregator socket) or `xerotty serve` (daemon socket) first,")
		fmt.Fprintln(os.Stderr, "  or point at one explicitly with --socket <path>. See docs/MCP.md.")
		return 1
	}
	defer conn.Close()
	return bridgeStdio(conn, os.Stdin, os.Stdout)
}

// bridgeStdio pumps bytes both ways until either side hangs up.
// stdin EOF half-closes the socket (CloseWrite) so the server sees
// a clean disconnect and can finish flushing responses; the bridge
// exits when the server side closes.
func bridgeStdio(conn net.Conn, in io.Reader, out io.Writer) int {
	go func() {
		_, _ = io.Copy(conn, in)
		if uc, ok := conn.(*net.UnixConn); ok {
			_ = uc.CloseWrite()
		} else {
			_ = conn.Close()
		}
	}()
	if _, err := io.Copy(out, conn); err != nil {
		fmt.Fprintf(os.Stderr, "xerotty mcp: %v\n", err)
		return 1
	}
	return 0
}
