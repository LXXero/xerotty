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
//   - propose: write attempts are accepted + queued on the
//     session for review. Consumed by the GUI's approval banner
//     and the list_proposals / approve_proposal / drop_proposal
//     tools. Approval requires auto mode or token authentication
//     (see the trust-model config), so a propose-mode agent can't
//     approve its own writes.
//
//   - auto: writes go straight to the PTY. Use when the agent has
//     full delegated authority (typical for headless servers /
//     CI / scripted operators).
//
// Supports the standard MCP shape (initialize / tools/list /
// tools/call) AND native JSON-RPC methods for `nc -U` debugging.
// Native methods: tabs/list, tab/screen, tab/scrollback,
// tab/input, tab/paste, tab/create, tab/close, tab/resize,
// tab/clipboard, proposals/{list,approve,drop}, agent/{mode,
// authenticate,clients}, server/info.
package mcp

import (
	"bufio"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/sockpath"
)

// Server is the MCP listener. Like daemon.Daemon it owns a unix
// socket; multiple agents can connect simultaneously, each with
// their own mode and tab subscriptions.
type Server struct {
	d          *daemon.Daemon
	socketPath string
	listener   net.Listener

	// Trust-model config (copied from cfg.MCP at construction).
	defaultMode     string
	allowModeChange bool
	approvalToken   string
}

// New constructs a Server that will listen on socketPath when Run is
// called. The Daemon is the live session host — methods read tab
// state from / write input to whichever session the agent targets.
// Trust-model knobs (default mode, mode-change lock, approval
// token) come from the daemon's config.
func New(d *daemon.Daemon, socketPath string) *Server {
	cfg := d.Config()
	mode := cfg.MCP.DefaultMode
	if mode == "" {
		mode = "observe"
	}
	return &Server{
		d:               d,
		socketPath:      socketPath,
		defaultMode:     mode,
		allowModeChange: cfg.MCP.AllowModeChange,
		approvalToken:   cfg.MCP.ApprovalToken,
	}
}

// SocketPath returns the socket path the server listens on.
func (s *Server) SocketPath() string { return s.socketPath }

// Run blocks until the listener errors or Stop is called.
func (s *Server) Run() error {
	if fi, err := os.Stat(s.socketPath); err == nil {
		// Same logic as daemon.Daemon.Run: never delete a non-socket
		// that happens to live at this path.
		if fi.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("mcp: %s exists and is not a socket; refusing to remove it", s.socketPath)
		}
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
	// Record the bound path so `xerotty mcp` can discover this
	// daemon even when its environment computes different defaults
	// (macOS TMPDIR varies by launch context).
	_ = sockpath.Record(sockpath.RecordDaemonMCP, s.socketPath)

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

// agentConn is per-connection state. Mode starts at the server's
// configured DefaultMode. authed goes true after a successful
// agent/authenticate with the approval token; authed connections
// can elevate to auto + approve/drop even when AllowModeChange is
// false.
type agentConn struct {
	srv  *Server
	conn net.Conn

	mu     sync.Mutex
	mode   string
	authed bool
}

func (s *Server) serve(conn net.Conn) {
	defer conn.Close()
	c := &agentConn{srv: s, conn: conn, mode: s.defaultMode}
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
	// Native methods — kept for simple `nc -U` debugging without
	// the MCP handshake/wrapping ceremony.
	case "tabs/list":
		return c.handleTabsList(req)
	case "tab/screen":
		return c.handleTabScreen(req)
	case "tab/scrollback":
		return c.handleTabScrollback(req)
	case "tab/input":
		return c.handleTabInput(req)
	case "tab/paste":
		return c.handleTabPaste(req)
	case "tab/create":
		return c.handleTabCreate(req)
	case "tab/close":
		return c.handleTabClose(req)
	case "tab/resize":
		return c.handleTabResize(req)
	case "proposals/list":
		return c.handleProposalsList(req)
	case "proposals/approve":
		return c.handleProposalsApprove(req)
	case "proposals/drop":
		return c.handleProposalsDrop(req)
	case "tab/clipboard":
		return c.handleClipboard(req)
	case "agent/mode":
		return c.handleAgentMode(req)
	case "agent/authenticate":
		return c.handleAgentAuthenticate(req)
	case "agent/clients":
		return c.handleAgentClients(req)
	case "server/info":
		return c.handleServerInfo(req)

	// Standard MCP methods — what Claude Code / Xyphia / other
	// MCP-aware clients speak. tools/call dispatches to the same
	// native handlers above.
	case "initialize":
		return c.handleMCPInitialize(req)
	case "tools/list":
		return c.handleMCPToolsList(req)
	case "tools/call":
		return c.handleMCPToolsCall(req)
	case "notifications/initialized":
		// Notification — no response needed. Some clients send
		// this after initialize as part of the handshake.
		return nil

	default:
		return methodNotFound(req.ID, req.Method)
	}
}

func (c *agentConn) handleTabCreate(req *rpcRequest) *rpcResponse {
	if err := c.requireWrite(); err != nil {
		return rpcErr(req.ID, -32099, err.Error(), nil)
	}
	var p struct {
		WindowID uint32 `json:"window_id,omitempty"`
		Cols     int    `json:"cols,omitempty"`
		Rows     int    `json:"rows,omitempty"`
		Cwd      string `json:"cwd,omitempty"`
		Name     string `json:"name,omitempty"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return invalidParams(req.ID, err.Error())
		}
	}
	if p.Cols == 0 {
		p.Cols = 80
	}
	if p.Rows == 0 {
		p.Rows = 24
	}
	sess := c.srv.d.SessionByName("default")
	if sess == nil {
		return rpcErr(req.ID, -32004, "no default session", nil)
	}
	// Funnel through the daemon so the new tab broadcasts to every
	// attached wire client — an MCP-created tab must be visible to a
	// watching GUI, not just to the agent that made it. A non-empty
	// name makes this idempotent: a second call with the same name
	// returns the existing tab (reused=true) instead of stacking a
	// duplicate.
	t, _, created, err := c.srv.d.CreateNamedTab(sess, p.Name, p.WindowID, p.Cols, p.Rows, p.Cwd)
	if err != nil {
		return rpcErr(req.ID, -32000, "new tab: "+err.Error(), nil)
	}
	// Report actual (clamped) dims — NewTab bounds to MaxTabDim.
	return ok(req.ID, map[string]any{
		"tab_id": t.ID,
		"cols":   t.Term.Width(),
		"rows":   t.Term.Height(),
		"name":   t.Name,
		"reused": !created,
	})
}

func (c *agentConn) handleTabClose(req *rpcRequest) *rpcResponse {
	if err := c.requireWrite(); err != nil {
		return rpcErr(req.ID, -32099, err.Error(), nil)
	}
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
	// Funnel through the daemon so the close broadcasts to every
	// attached wire client.
	c.srv.d.CloseTab(sess, p.TabID)
	return ok(req.ID, map[string]bool{"ok": true})
}

func (c *agentConn) handleTabResize(req *rpcRequest) *rpcResponse {
	if err := c.requireWrite(); err != nil {
		return rpcErr(req.ID, -32099, err.Error(), nil)
	}
	var p struct {
		TabID uint32 `json:"tab_id"`
		Cols  int    `json:"cols"`
		Rows  int    `json:"rows"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return invalidParams(req.ID, err.Error())
	}
	if p.Cols < 1 || p.Rows < 1 {
		return invalidParams(req.ID, "cols/rows must be >= 1")
	}
	sess := c.srv.d.SessionByName("default")
	if sess == nil {
		return rpcErr(req.ID, -32004, "no default session", nil)
	}
	t := sess.Tab(p.TabID)
	if t == nil {
		return rpcErr(req.ID, -32004, "tab not found", nil)
	}
	t.Term.Resize(daemon.ClampTabDim(p.Cols), daemon.ClampTabDim(p.Rows))
	// Fan out to every subscriber so all attached clients re-paint at
	// the new size (a resize emits no PTY output). Mirrors the wire
	// handler.
	c.srv.d.WakeTabSubscribers(p.TabID)
	return ok(req.ID, map[string]bool{"ok": true})
}

func (c *agentConn) handleProposalsList(req *rpcRequest) *rpcResponse {
	sess := c.srv.d.SessionByName("default")
	if sess == nil {
		return ok(req.ID, []map[string]any{})
	}
	pending := sess.PendingProposals()
	out := make([]map[string]any, len(pending))
	for i, p := range pending {
		kind := "input"
		if p.IsPaste {
			kind = "paste"
		}
		out[i] = map[string]any{
			"index":   i,
			"tab_id":  p.TabID,
			"kind":    kind,
			"payload": string(p.Bytes),
		}
	}
	return ok(req.ID, out)
}

func (c *agentConn) handleProposalsApprove(req *rpcRequest) *rpcResponse {
	// Approval needs auto authority — a propose-mode agent must
	// not approve its own queued writes.
	if err := c.requireApprovalAuthority(); err != nil {
		return rpcErr(req.ID, -32099, err.Error(), nil)
	}
	var p struct {
		Index int `json:"index"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return invalidParams(req.ID, err.Error())
	}
	sess := c.srv.d.SessionByName("default")
	if sess == nil {
		return rpcErr(req.ID, -32004, "no default session", nil)
	}
	found, _, err := sess.ApproveProposal(p.Index)
	if !found {
		return rpcErr(req.ID, -32004, "no proposal at index", nil)
	}
	if err != nil {
		return rpcErr(req.ID, -32000, "apply: "+err.Error(), nil)
	}
	return ok(req.ID, map[string]bool{"approved": true})
}

func (c *agentConn) handleProposalsDrop(req *rpcRequest) *rpcResponse {
	// Dropping (rejecting) also needs auto authority — otherwise a
	// propose-mode agent could clear its own pending writes to
	// hide them from a reviewer.
	if err := c.requireApprovalAuthority(); err != nil {
		return rpcErr(req.ID, -32099, err.Error(), nil)
	}
	var p struct {
		Index int `json:"index"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return invalidParams(req.ID, err.Error())
	}
	sess := c.srv.d.SessionByName("default")
	if sess == nil {
		return rpcErr(req.ID, -32004, "no default session", nil)
	}
	if !sess.DropProposal(p.Index) {
		return rpcErr(req.ID, -32004, "no proposal at index", nil)
	}
	return ok(req.ID, map[string]bool{"dropped": true})
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
			Name:     t.Name,
			Title:    t.Title(),
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

// handleTabScrollback reads a slice of scrollback rows for a tab.
// Defaults to the most recent 200 rows; caller can pass {from, to}
// for an absolute range (0 = oldest scrollback row). Returns
// strings with trailing whitespace trimmed.
//
// Atomic snapshot under publishMu — same reason tab/screen uses
// SnapshotViewport, just for the scrollback half.
func (c *agentConn) handleTabScrollback(req *rpcRequest) *rpcResponse {
	var p struct {
		TabID uint32 `json:"tab_id"`
		From  *int   `json:"from,omitempty"`
		To    *int   `json:"to,omitempty"`
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
	sbLen := t.Term.ScrollbackLen()
	from := sbLen - 200
	to := sbLen
	if p.From != nil {
		from = *p.From
	}
	if p.To != nil {
		to = *p.To
	}
	if from < 0 {
		from = 0
	}
	if to > sbLen {
		to = sbLen
	}
	if to <= from {
		return ok(req.ID, scrollbackResult{From: from, To: from, Total: sbLen, Lines: []string{}})
	}
	rows := t.Term.SnapshotScrollbackRange(from, to)
	lines := make([]string, len(rows))
	for i, row := range rows {
		var sb strings.Builder
		for _, cell := range row {
			if cell.Content == "" {
				sb.WriteByte(' ')
				continue
			}
			sb.WriteString(cell.Content)
		}
		lines[i] = strings.TrimRight(sb.String(), " ")
	}
	return ok(req.ID, scrollbackResult{From: from, To: to, Total: sbLen, Lines: lines})
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
	// Mode-change lock: when AllowModeChange is false, a connection
	// can't change its own mode UNLESS it authenticated with the
	// approval token. This is what turns propose mode into a real
	// gate — agents land in DefaultMode and stay there; only a
	// token-bearing reviewer elevates.
	if !c.srv.allowModeChange && !c.isAuthed() {
		// Allow de-escalation (toward observe) always; block
		// elevation toward auto.
		if modeRank(p.Mode) > modeRank(c.getMode()) {
			return rpcErr(req.ID, -32099, "mode change blocked: server has allow_mode_change=false; authenticate with the approval token to elevate", nil)
		}
	}
	c.setMode(p.Mode)
	return ok(req.ID, map[string]string{"mode": p.Mode})
}

// modeRank orders modes by write authority: observe < propose <
// auto. Used to allow de-escalation but block elevation under a
// mode-change lock.
func modeRank(m string) int {
	switch m {
	case "auto":
		return 2
	case "propose":
		return 1
	default:
		return 0
	}
}

// handleAgentAuthenticate grants approval authority when the
// connection presents the configured approval token. After this
// the connection may elevate to auto + approve/drop proposals
// even under a mode-change lock. No-op error if no token is
// configured (auth not in use).
func (c *agentConn) handleAgentAuthenticate(req *rpcRequest) *rpcResponse {
	if c.srv.approvalToken == "" {
		return rpcErr(req.ID, -32601, "agent/authenticate: no approval_token configured on this daemon", nil)
	}
	var p struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return invalidParams(req.ID, err.Error())
	}
	// Constant-time compare to avoid leaking the token via timing.
	if subtle.ConstantTimeCompare([]byte(p.Token), []byte(c.srv.approvalToken)) != 1 {
		return rpcErr(req.ID, -32099, "agent/authenticate: invalid token", nil)
	}
	c.mu.Lock()
	c.authed = true
	c.mu.Unlock()
	return ok(req.ID, map[string]bool{"authenticated": true})
}

func (c *agentConn) isAuthed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.authed
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

// requireApprovalAuthority gates the approve_proposal path. A
// propose-mode agent must NOT be able to approve its own queued
// writes — that defeats the entire "human (or distinct authority)
// reviews before it lands" purpose of propose mode. Only auto-mode
// connections (which already have full write authority anyway) can
// approve. Observe + propose are rejected.
//
// Intent: the approval comes from a SEPARATE auto-authorized
// connection — a human's review tool, a supervising orchestrator,
// or the (future) GUI gate — not the proposing agent itself.
func (c *agentConn) requireApprovalAuthority() error {
	// A token-authenticated connection always has approval
	// authority (it's the designated reviewer). Otherwise require
	// auto mode — and note that under a mode-change lock, only an
	// authed connection could have reached auto anyway.
	if c.isAuthed() {
		return nil
	}
	if c.getMode() != "auto" {
		return fmt.Errorf("approve/drop requires auto mode or token authentication; current mode %q has no approval authority", c.getMode())
	}
	return nil
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
