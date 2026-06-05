package runner

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"time"

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
//
// The bridge SURVIVES the socket dying (daemon hot upgrade, GUI
// restart): it re-runs discovery and reconnects, so the MCP client
// never sees its server process exit. One in-flight request can be
// lost per reconnect (its response died with the old connection) —
// the client's retry handles that. The bridge only exits when its
// STDIN closes (the client is gone) or reconnection fails for a
// sustained window.
func MCPBridge(args []string) int {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	var socketPath string
	var daemonOnly bool
	fs.StringVar(&socketPath, "socket", "", "MCP socket path to bridge to (default: discover GUI aggregator, then local daemon)")
	fs.BoolVar(&daemonOnly, "daemon", false, "bridge to the local daemon's MCP socket, skipping the GUI aggregator")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	discover := func() net.Conn {
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
		for _, p := range candidates {
			if c, err := net.Dial("unix", p); err == nil {
				// stderr only — stdout belongs to the JSON-RPC stream.
				fmt.Fprintf(os.Stderr, "xerotty mcp: bridging stdio <-> %s\n", p)
				return c
			}
		}
		return nil
	}
	return runBridge(discover, os.Stdin, os.Stdout)
}

// reconnectWindow is how long the bridge keeps retrying discovery
// after losing its socket before giving up. Long enough to span a
// daemon hot upgrade or a GUI restart comfortably.
const reconnectWindow = 30 * time.Second

// bridgeModeReplayID tags the synthetic agent/mode request the
// bridge injects after a reconnect; its response is swallowed so the
// client never sees traffic it didn't send.
const bridgeModeReplayID = "xerotty-bridge-mode-replay"

// sniffModeSet inspects an outbound request line for a mode change
// (native agent/mode or the set_agent_mode tool) and returns the
// requested mode. The daemon's trust mode is per-CONNECTION state —
// without replaying it after a reconnect, every daemon hot upgrade
// silently demoted agents back to observe and their next write
// failed mysteriously.
func sniffModeSet(line []byte) (string, bool) {
	if !bytes.Contains(line, []byte("mode")) {
		return "", false
	}
	var req struct {
		Method string `json:"method"`
		Params struct {
			Mode      string `json:"mode"`
			Name      string `json:"name"`
			Arguments struct {
				Mode string `json:"mode"`
			} `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(line, &req); err != nil {
		return "", false
	}
	switch {
	case req.Method == "agent/mode" && req.Params.Mode != "":
		return req.Params.Mode, true
	case req.Method == "tools/call" && req.Params.Name == "set_agent_mode" && req.Params.Arguments.Mode != "":
		return req.Params.Arguments.Mode, true
	}
	return "", false
}

// runBridge pumps newline-delimited JSON-RPC between in/out and
// whatever discover() connects to, reconnecting on socket loss.
// Reads from `in` are line-framed so a reconnect can never split a
// request mid-frame.
func runBridge(discover func() net.Conn, in io.Reader, out io.Writer) int {
	// Single stdin reader for the bridge's lifetime; lines fan into
	// the per-connection forward loop. Closed channel = client gone.
	lines := make(chan []byte, 16)
	go func() {
		sc := bufio.NewScanner(in)
		sc.Buffer(make([]byte, 64*1024), 16*1024*1024) // big screens, big frames
		for sc.Scan() {
			b := append(append([]byte{}, sc.Bytes()...), '\n')
			lines <- b
		}
		close(lines)
	}()

	firstDial := true
	lastMode := "" // last client-requested trust mode, replayed on reconnect
	for {
		var conn net.Conn
		deadline := time.Now().Add(reconnectWindow)
		for time.Now().Before(deadline) {
			if conn = discover(); conn != nil {
				break
			}
			if firstDial {
				// Nothing listening on first launch: fail fast with
				// guidance instead of silently spinning — the MCP
				// client surfaces this error to the user.
				fmt.Fprintln(os.Stderr, "xerotty mcp: no MCP socket reachable")
				fmt.Fprintln(os.Stderr, "  start the GUI (aggregator socket) or `xerotty serve` (daemon socket) first,")
				fmt.Fprintln(os.Stderr, "  or point at one explicitly with --socket <path>. See docs/MCP.md.")
				return 1
			}
			time.Sleep(200 * time.Millisecond)
		}
		if conn == nil {
			fmt.Fprintf(os.Stderr, "xerotty mcp: no MCP socket came back within %s — giving up\n", reconnectWindow)
			return 1
		}
		// Re-assert the client's trust mode on the fresh connection
		// (per-connection daemon state — see sniffModeSet). The
		// response is swallowed below by its synthetic id.
		swallowReplay := false
		if !firstDial && lastMode != "" {
			fmt.Fprintf(conn, `{"jsonrpc":"2.0","id":%q,"method":"agent/mode","params":{"mode":%q}}`+"\n",
				bridgeModeReplayID, lastMode)
			swallowReplay = true
			fmt.Fprintf(os.Stderr, "xerotty mcp: re-asserted mode %q after reconnect\n", lastMode)
		}
		if !firstDial {
			// The server behind the socket may be a NEW BINARY (that
			// is the whole point of hot upgrades) with a different
			// tool catalog. Nudge the client to re-fetch tools/list —
			// without this, new tools stay invisible until the user
			// restarts their session.
			fmt.Fprintln(out, `{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`)
		}
		firstDial = false
		connStart := time.Now()

		// Server → client, line-framed so the mode-replay response
		// can be filtered out; everything else passes through.
		done := make(chan struct{})
		go func() {
			defer close(done)
			sc := bufio.NewScanner(conn)
			sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
			for sc.Scan() {
				line := sc.Bytes()
				if swallowReplay && bytes.Contains(line, []byte(bridgeModeReplayID)) {
					swallowReplay = false
					continue
				}
				if _, err := out.Write(append(append([]byte{}, line...), '\n')); err != nil {
					return
				}
			}
		}()

		alive := true
		for alive {
			select {
			case b, ok := <-lines:
				if !ok {
					// Client closed stdin: flush the half-close so
					// the server can finish responding, then leave.
					if uc, isUnix := conn.(*net.UnixConn); isUnix {
						_ = uc.CloseWrite()
					} else {
						_ = conn.Close()
					}
					<-done
					return 0
				}
				if m, ok := sniffModeSet(b); ok {
					lastMode = m
				}
				if _, err := conn.Write(b); err != nil {
					// This request is lost (its response would have
					// died with the conn anyway). The client's
					// timeout/retry covers it.
					fmt.Fprintln(os.Stderr, "xerotty mcp: connection lost mid-request — re-dialing")
					alive = false
				}
			case <-done:
				// Server hung up (daemon upgrade, GUI restart).
				fmt.Fprintln(os.Stderr, "xerotty mcp: server closed the connection — re-dialing")
				alive = false
			}
		}
		_ = conn.Close()
		<-done
		// Flap guard: a connection that died young means the server
		// is bouncing — don't tight-loop redial+replay against it.
		if time.Since(connStart) < time.Second {
			time.Sleep(500 * time.Millisecond)
		}
	}
}
