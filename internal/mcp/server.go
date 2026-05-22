// Package mcp implements the AI-agent control surface for xerottyd.
// It exposes a JSON-RPC 2.0 server on a separate unix socket so an
// agent (Claude Code, Xyphia, anything that speaks MCP) can attach
// alongside the regular UI clients without entangling the two
// protocols.
//
// The wire format is line-delimited JSON-RPC 2.0: each request and
// response is a single newline-terminated JSON object. Notifications
// (id absent) are silently ignored; this Phase 4 surface is strictly
// request/response.
//
// Three operating modes are recognized per-connection:
//
//   - observe (default): the agent can read tab state but write
//     attempts return error -32099 "write blocked in observe mode".
//
//   - propose: write attempts are accepted by the server, queued
//     on the session, and surfaced to a future xerotty UI gate for
//     user approval. Phase 4 ships the queue but no UI consumer;
//     until the UI lands, propose behaves like observe (writes are
//     queued silently but never applied).
//
//   - auto: writes go straight to the PTY. Use when the agent has
//     full delegated authority (typical for headless servers /
//     CI / scripted operators).
//
// Method surface (subject to migration onto formal MCP /tools spec
// once we wire that up):
//
//	tabs/list                   -> [{id, title, cols, rows, window_id, focused}]
//	tab/screen   {tab_id}        -> {cols, rows, lines:[string,...]}
//	tab/input    {tab_id, bytes} -> {ok:true} | error
//	tab/paste    {tab_id, text}  -> {ok:true} | error
//	tab/clipboard               -> {text}
//	agent/mode   {mode?}         -> {mode}        (get if mode omitted, set otherwise)
package mcp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/LXXero/xerotty/internal/daemon"
)

// Server is the MCP listener. Like daemon.Daemon it owns a unix
// socket; multiple agents can connect simultaneously, each with
// their own mode and tab subscriptions.
type Server struct {
	d          *daemon.Daemon
	socketPath string
	listener   net.Listener
}

// New constructs a Server that will listen on socketPath when Run is
// called. The Daemon is the live session host — methods read tab
// state from / write input to whichever session the agent targets.
// Phase 4 hardcodes the "default" session like everything else.
func New(d *daemon.Daemon, socketPath string) *Server {
	return &Server{d: d, socketPath: socketPath}
}

// SocketPath returns the socket path the server listens on.
func (s *Server) SocketPath() string { return s.socketPath }

// Run blocks until the listener errors or Stop is called.
func (s *Server) Run() error {
	if _, err := os.Stat(s.socketPath); err == nil {
		// Probe — same logic as daemon.Daemon.Run for stale sockets.
		if c, err := net.Dial("unix", s.socketPath); err == nil {
			c.Close()
			return fmt.Errorf("mcp: socket %s already in use", s.socketPath)
		}
		_ = os.Remove(s.socketPath)
	}
	ln, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("mcp: listen %s: %w", s.socketPath, err)
	}
	s.listener = ln
	_ = os.Chmod(s.socketPath, 0o600)

	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.serve(conn)
	}
}

// Stop closes the listener and removes the socket file.
func (s *Server) Stop() error {
	if s.listener != nil {
		_ = s.listener.Close()
	}
	if s.socketPath != "" {
		_ = os.Remove(s.socketPath)
	}
	return nil
}

// agentConn is per-connection state. Mode defaults to "observe".
type agentConn struct {
	srv  *Server
	conn net.Conn

	mu   sync.Mutex
	mode string
}

func (s *Server) serve(conn net.Conn) {
	defer conn.Close()
	c := &agentConn{srv: s, conn: conn, mode: "observe"}
	c.run()
}

// run reads line-delimited JSON-RPC requests from the conn and
// dispatches each to the method handlers. One bad request gets a
// JSON-RPC error reply; we don't drop the connection unless reading
// itself fails.
func (c *agentConn) run() {
	scanner := bufio.NewScanner(c.conn)
	// Default scanner buffer is 64KiB; bump it so screen scrapes
	// (cols*rows*4 bytes of UTF-8 in the worst case) don't truncate.
	scanner.Buffer(make([]byte, 0, 1<<16), 1<<20)
	enc := json.NewEncoder(c.conn)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			_ = enc.Encode(parseError(nil, err.Error()))
			continue
		}
		if req.JSONRPC == "" {
			req.JSONRPC = "2.0"
		}
		resp := c.handle(&req)
		if resp != nil {
			_ = enc.Encode(resp)
		}
	}
}

func (c *agentConn) handle(req *rpcRequest) *rpcResponse {
	// Notifications (no id) get no response. Phase 4 has no useful
	// notifications, so we simply ignore them.
	if req.ID == nil {
		return nil
	}
	switch req.Method {
	case "tabs/list":
		return c.handleTabsList(req)
	case "tab/screen":
		return c.handleTabScreen(req)
	case "tab/input":
		return c.handleTabInput(req)
	case "tab/paste":
		return c.handleTabPaste(req)
	case "tab/clipboard":
		return c.handleClipboard(req)
	case "agent/mode":
		return c.handleAgentMode(req)
	case "agent/clients":
		return c.handleAgentClients(req)
	case "server/info":
		return c.handleServerInfo(req)
	default:
		return methodNotFound(req.ID, req.Method)
	}
}

func (c *agentConn) handleAgentClients(req *rpcRequest) *rpcResponse {
	out := c.srv.d.AttachedClients()
	return ok(req.ID, out)
}

func (c *agentConn) handleServerInfo(req *rpcRequest) *rpcResponse {
	host, _ := os.Hostname()
	return ok(req.ID, map[string]any{
		"hostname": host,
		"pid":      os.Getpid(),
	})
}

func (c *agentConn) handleTabsList(req *rpcRequest) *rpcResponse {
	sess := c.srv.d.SessionByName("default")
	if sess == nil {
		return ok(req.ID, []tabSummary{})
	}
	tabs := sess.Tabs()
	out := make([]tabSummary, 0, len(tabs))
	wins := sess.Windows()
	windowOf := make(map[uint32]uint32, len(tabs))
	for _, w := range wins {
		for _, id := range w.TabIDs {
			windowOf[id] = w.ID
		}
	}
	focused := sess.FocusedTab()
	for _, t := range tabs {
		out = append(out, tabSummary{
			ID:       t.ID,
			Title:    t.Title,
			Cols:     uint16(t.Term.Width()),
			Rows:     uint16(t.Term.Height()),
			WindowID: windowOf[t.ID],
			Focused:  t.ID == focused,
		})
	}
	return ok(req.ID, out)
}

func (c *agentConn) handleTabScreen(req *rpcRequest) *rpcResponse {
	var p struct {
		TabID uint32 `json:"tab_id"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return invalidParams(req.ID, err.Error())
	}
	sess := c.srv.d.SessionByName("default")
	if sess == nil {
		return rpcErr(req.ID, -32004, "no default session", nil)
	}
	t := sess.Tab(p.TabID)
	if t == nil {
		return rpcErr(req.ID, -32004, "tab not found", nil)
	}
	// Atomic snapshot under publishMu so the agent doesn't get a
	// mid-scroll smear when reading during heavy PTY output (same
	// reason the wire-publish path uses SnapshotViewport).
	grid := t.Term.SnapshotViewport()
	rows := len(grid)
	cols := 0
	if rows > 0 {
		cols = len(grid[0])
	}
	lines := make([]string, rows)
	for r := 0; r < rows; r++ {
		var sb strings.Builder
		for col := 0; col < cols; col++ {
			cell := &grid[r][col]
			if cell.Content == "" {
				sb.WriteByte(' ')
				continue
			}
			sb.WriteString(cell.Content)
		}
		lines[r] = strings.TrimRight(sb.String(), " ")
	}
	return ok(req.ID, screenResult{Cols: uint16(cols), Rows: uint16(rows), Lines: lines})
}

func (c *agentConn) handleTabInput(req *rpcRequest) *rpcResponse {
	if err := c.requireWrite(); err != nil {
		return rpcErr(req.ID, -32099, err.Error(), nil)
	}
	var p struct {
		TabID uint32 `json:"tab_id"`
		Bytes string `json:"bytes"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return invalidParams(req.ID, err.Error())
	}
	sess := c.srv.d.SessionByName("default")
	if sess == nil {
		return rpcErr(req.ID, -32004, "no default session", nil)
	}
	t := sess.Tab(p.TabID)
	if t == nil {
		return rpcErr(req.ID, -32004, "tab not found", nil)
	}
	if c.modeIs("propose") {
		// Queue silently — UI gate consumer lands later.
		sess.QueueProposedInput(p.TabID, []byte(p.Bytes))
		return ok(req.ID, map[string]bool{"queued": true})
	}
	if _, err := t.Term.Write([]byte(p.Bytes)); err != nil {
		return rpcErr(req.ID, -32000, "write: "+err.Error(), nil)
	}
	return ok(req.ID, map[string]bool{"ok": true})
}

func (c *agentConn) handleTabPaste(req *rpcRequest) *rpcResponse {
	if err := c.requireWrite(); err != nil {
		return rpcErr(req.ID, -32099, err.Error(), nil)
	}
	var p struct {
		TabID uint32 `json:"tab_id"`
		Text  string `json:"text"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return invalidParams(req.ID, err.Error())
	}
	sess := c.srv.d.SessionByName("default")
	if sess == nil {
		return rpcErr(req.ID, -32004, "no default session", nil)
	}
	t := sess.Tab(p.TabID)
	if t == nil {
		return rpcErr(req.ID, -32004, "tab not found", nil)
	}
	if c.modeIs("propose") {
		sess.QueueProposedPaste(p.TabID, p.Text)
		return ok(req.ID, map[string]bool{"queued": true})
	}
	t.Term.Paste(p.Text)
	return ok(req.ID, map[string]bool{"ok": true})
}

func (c *agentConn) handleClipboard(req *rpcRequest) *rpcResponse {
	sess := c.srv.d.SessionByName("default")
	if sess == nil {
		return ok(req.ID, map[string]string{"text": ""})
	}
	return ok(req.ID, map[string]string{"text": sess.Clipboard()})
}

func (c *agentConn) handleAgentMode(req *rpcRequest) *rpcResponse {
	var p struct {
		Mode string `json:"mode,omitempty"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return invalidParams(req.ID, err.Error())
		}
	}
	if p.Mode == "" {
		return ok(req.ID, map[string]string{"mode": c.getMode()})
	}
	switch p.Mode {
	case "observe", "propose", "auto":
	default:
		return invalidParams(req.ID, "mode must be one of: observe, propose, auto")
	}
	c.setMode(p.Mode)
	return ok(req.ID, map[string]string{"mode": p.Mode})
}

// requireWrite returns nil if this connection's mode permits writes
// to land on a PTY (auto), or if the daemon should at least accept
// the write as a proposal (propose). observe rejects.
func (c *agentConn) requireWrite() error {
	switch c.getMode() {
	case "auto", "propose":
		return nil
	default:
		return fmt.Errorf("write blocked in observe mode; set agent/mode to auto or propose first")
	}
}

func (c *agentConn) modeIs(m string) bool { return c.getMode() == m }

func (c *agentConn) getMode() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mode
}

func (c *agentConn) setMode(m string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mode = m
}
