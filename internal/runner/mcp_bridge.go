package runner

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"

	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/sockpath"
)

// MCPBridge implements `xerotty mcp`: a stdio <-> unix-socket bridge
// so MCP clients — which spawn a command and speak line-delimited
// JSON-RPC over its stdin/stdout — can talk to the MCP servers that
// live on unix sockets (the GUI's aggregator, or a daemon's own).
// One-time client setup is then just:
//
//	claude mcp add xerotty -- xerotty mcp
//
// Discovery order (each candidate is dial-verified, first live one
// wins):
//
//  1. --socket PATH               explicit, no fallback
//  2. recorded GUI MCP socket     where the GUI actually bound
//  3. default GUI MCP socket
//  4. recorded daemon MCP socket  where the daemon actually bound
//  5. config tabs.daemon_socket   user's configured override
//  6. default daemon MCP socket
//
// (2)+(3) are skipped with --daemon. The recordings exist because
// computed defaults aren't enough on macOS: without XDG_RUNTIME_DIR
// the temp dir comes from $TMPDIR, which differs per launch context,
// so the bind side and an agent-spawned bridge can compute different
// "defaults". See internal/sockpath.
func MCPBridge(args []string) int {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	var socketPath string
	var daemonOnly bool
	fs.StringVar(&socketPath, "socket", "", "MCP socket path to bridge to (default: discover GUI aggregator, then local daemon)")
	fs.BoolVar(&daemonOnly, "daemon", false, "bridge to the local daemon's MCP socket, skipping the GUI aggregator")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	var candidates []string
	seen := map[string]bool{}
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			candidates = append(candidates, p)
		}
	}
	if socketPath != "" {
		add(socketPath)
	} else {
		if !daemonOnly {
			add(sockpath.Recorded(sockpath.RecordGUIMCP))
			add(sockpath.GUIMCPSocket())
		}
		add(sockpath.Recorded(sockpath.RecordDaemonMCP))
		if cfg, err := config.Load(); err == nil && cfg.Tabs.DaemonSocket != "" {
			add(sockpath.MCPSocketFor(cfg.Tabs.DaemonSocket))
		}
		add(sockpath.MCPSocketFor(sockpath.DaemonSocket()))
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
