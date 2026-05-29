// Package guimcp is the GUI's aggregating MCP server. Where each
// daemon exposes its OWN tabs on its OWN MCP socket, the GUI talks
// to several daemons at once (local + remote hosts), so an agent
// wanting "everything I can see in this xerotty window" would
// otherwise have to connect to each daemon's socket separately.
//
// This server runs ONE socket inside the GUI process and presents
// a unified view: tab IDs are namespaced "<host>:<tabid>" (host
// "local" for the local daemon), and each tool routes to the
// right daemon hub's Source. Read/write go through the same
// daemonsource.Source the GUI renders from, so the agent sees
// exactly what the user sees.
//
// Speaks the same line-delimited JSON-RPC 2.0 + MCP shape
// (initialize / tools/list / tools/call) as internal/mcp. Tools:
//
//	list_tabs                    -> [{id, host, title, cols, rows}]
//	get_screen   {tab_id}         -> {cols, rows, lines}
//	get_scrollback {tab_id, ...}  -> {from, to, total, lines}
//	send_input   {tab_id, bytes}
//	send_paste   {tab_id, text}
//
// No write-gating here: the GUI is the user's own trusted process.
// (The daemon-side MCP servers keep their observe/propose/auto
// gating for agents connecting directly to a daemon.)
package guimcp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	uv "github.com/charmbracelet/ultraviolet"

	"github.com/LXXero/xerotty/internal/daemonsource"
)

// Backend is what the GUI provides: enumeration of all tabs across
// every connected daemon, + resolution of a namespaced tab ID to
// its Source.
type Backend interface {
	// ListTabs returns every tab across all daemon hubs, with
	// namespaced IDs ("local:5", "kh:3").
	ListTabs() []TabRef
	// SourceFor resolves a namespaced ID to its Source.
	SourceFor(nsID string) (*daemonsource.Source, bool)
}

// TabRef is one tab in the aggregated view. Beyond identity +
// dims it carries the metadata an orchestrator wants to triage
// tabs without reading every screen: working dir, foreground
// process, whether the shell exited, and whether it's the
// user's focused tab.
type TabRef struct {
	NSID       string // "<host>:<tabid>"
	Host       string
	Title      string
	Cols       int
	Rows       int
	CWD        string // foreground proc's cwd ("" if unknown)
	Foreground string // foreground process name (vim, less, ...)
	Closed     bool   // child process exited
	ExitCode   int    // -1 while running
	Focused    bool   // the user's currently-focused tab in the GUI
}

// Server is the GUI's aggregating MCP listener.
type Server struct {
	backend    Backend
	socketPath string
	listener   net.Listener
}

// New builds a Server. Call Run to listen, Stop to tear down.
func New(b Backend, socketPath string) *Server {
	return &Server{backend: b, socketPath: socketPath}
}

// SocketPath returns the path the server listens on.
func (s *Server) SocketPath() string { return s.socketPath }

// Run blocks until the listener errors or Stop is called.
func (s *Server) Run() error {
	if fi, err := os.Stat(s.socketPath); err == nil {
		// Never delete a non-socket that happens to live at this path.
		if fi.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("guimcp: %s exists and is not a socket; refusing to remove it", s.socketPath)
		}
		if c, err := net.Dial("unix", s.socketPath); err == nil {
			c.Close()
			return fmt.Errorf("guimcp: socket %s already in use", s.socketPath)
		}
		_ = os.Remove(s.socketPath)
	}
	ln, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("guimcp: listen %s: %w", s.socketPath, err)
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

// Stop closes the listener + removes the socket.
func (s *Server) Stop() error {
	if s.listener != nil {
		_ = s.listener.Close()
	}
	if s.socketPath != "" {
		_ = os.Remove(s.socketPath)
	}
	return nil
}

func (s *Server) serve(conn net.Conn) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
	enc := json.NewEncoder(conn)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			_ = enc.Encode(rpcErrResp(nil, -32700, "parse error: "+err.Error()))
			continue
		}
		if req.ID == nil {
			continue // notification — no reply
		}
		_ = enc.Encode(s.handle(&req))
	}
}

func (s *Server) handle(req *rpcRequest) *rpcResponse {
	switch req.Method {
	case "initialize":
		return okResp(req.ID, map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "xerotty-gui", "version": "0.1.0"},
		})
	case "tools/list":
		return okResp(req.ID, map[string]any{"tools": toolCatalog()})
	case "tools/call":
		return s.handleToolsCall(req)
	// Native methods for nc -U debugging.
	case "list_tabs":
		return s.listTabs(req.ID)
	case "get_screen":
		return s.getScreen(req.ID, req.Params)
	case "get_scrollback":
		return s.getScrollback(req.ID, req.Params)
	case "send_input":
		return s.sendInput(req.ID, req.Params)
	case "send_paste":
		return s.sendPaste(req.ID, req.Params)
	default:
		return rpcErrResp(req.ID, -32601, "method not found: "+req.Method)
	}
}

func (s *Server) handleToolsCall(req *rpcRequest) *rpcResponse {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return rpcErrResp(req.ID, -32602, "invalid params: "+err.Error())
	}
	var inner *rpcResponse
	switch p.Name {
	case "list_tabs":
		inner = s.listTabs(req.ID)
	case "get_screen":
		inner = s.getScreen(req.ID, p.Arguments)
	case "get_scrollback":
		inner = s.getScrollback(req.ID, p.Arguments)
	case "send_input":
		inner = s.sendInput(req.ID, p.Arguments)
	case "send_paste":
		inner = s.sendPaste(req.ID, p.Arguments)
	default:
		return rpcErrResp(req.ID, -32601, "tools/call: unknown tool "+p.Name)
	}
	if inner.Error != nil {
		return inner
	}
	return okResp(req.ID, map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(inner.Result)}},
		"isError": false,
	})
}

func (s *Server) listTabs(id json.RawMessage) *rpcResponse {
	refs := s.backend.ListTabs()
	out := make([]map[string]any, len(refs))
	for i, r := range refs {
		out[i] = map[string]any{
			"id":         r.NSID,
			"host":       r.Host,
			"title":      r.Title,
			"cols":       r.Cols,
			"rows":       r.Rows,
			"cwd":        r.CWD,
			"foreground": r.Foreground,
			"closed":     r.Closed,
			"exit_code":  r.ExitCode,
			"focused":    r.Focused,
		}
	}
	return okResp(id, out)
}

func (s *Server) getScreen(id json.RawMessage, params json.RawMessage) *rpcResponse {
	var p struct {
		TabID string `json:"tab_id"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return rpcErrResp(id, -32602, err.Error())
	}
	src, ok := s.backend.SourceFor(p.TabID)
	if !ok {
		return rpcErrResp(id, -32004, "tab not found: "+p.TabID)
	}
	grid := src.SnapshotViewport()
	lines := gridLines(grid)
	cols := 0
	if len(grid) > 0 {
		cols = len(grid[0])
	}
	return okResp(id, map[string]any{"cols": cols, "rows": len(grid), "lines": lines})
}

func (s *Server) getScrollback(id json.RawMessage, params json.RawMessage) *rpcResponse {
	var p struct {
		TabID string `json:"tab_id"`
		From  *int   `json:"from,omitempty"`
		To    *int   `json:"to,omitempty"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return rpcErrResp(id, -32602, err.Error())
	}
	src, ok := s.backend.SourceFor(p.TabID)
	if !ok {
		return rpcErrResp(id, -32004, "tab not found: "+p.TabID)
	}
	total := src.ScrollbackLen()
	from := total - 200
	to := total
	if p.From != nil {
		from = *p.From
	}
	if p.To != nil {
		to = *p.To
	}
	if from < 0 {
		from = 0
	}
	if to > total {
		to = total
	}
	var lines []string
	if to > from {
		lines = gridLines(src.SnapshotScrollbackRange(from, to))
	}
	return okResp(id, map[string]any{"from": from, "to": to, "total": total, "lines": lines})
}

func (s *Server) sendInput(id json.RawMessage, params json.RawMessage) *rpcResponse {
	var p struct {
		TabID string `json:"tab_id"`
		Bytes string `json:"bytes"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return rpcErrResp(id, -32602, err.Error())
	}
	src, ok := s.backend.SourceFor(p.TabID)
	if !ok {
		return rpcErrResp(id, -32004, "tab not found: "+p.TabID)
	}
	if _, err := src.Write([]byte(p.Bytes)); err != nil {
		return rpcErrResp(id, -32000, "write: "+err.Error())
	}
	return okResp(id, map[string]bool{"ok": true})
}

func (s *Server) sendPaste(id json.RawMessage, params json.RawMessage) *rpcResponse {
	var p struct {
		TabID string `json:"tab_id"`
		Text  string `json:"text"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return rpcErrResp(id, -32602, err.Error())
	}
	src, ok := s.backend.SourceFor(p.TabID)
	if !ok {
		return rpcErrResp(id, -32004, "tab not found: "+p.TabID)
	}
	src.Paste(p.Text)
	return okResp(id, map[string]bool{"ok": true})
}

// --- helpers ---

func gridLines(grid [][]uv.Cell) []string {
	lines := make([]string, len(grid))
	for r, row := range grid {
		var sb strings.Builder
		for _, c := range row {
			if c.Content == "" {
				sb.WriteByte(' ')
				continue
			}
			sb.WriteString(c.Content)
		}
		lines[r] = strings.TrimRight(sb.String(), " ")
	}
	return lines
}

// MakeNSID builds a namespaced tab ID from host + daemon tab ID.
func MakeNSID(host string, tabID uint32) string {
	return host + ":" + strconv.FormatUint(uint64(tabID), 10)
}

func toolCatalog() []map[string]any {
	strProp := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	obj := func(props map[string]any, req ...string) map[string]any {
		m := map[string]any{"type": "object", "properties": props}
		if len(req) > 0 {
			m["required"] = req
		}
		return m
	}
	return []map[string]any{
		{"name": "list_tabs", "description": "List every tab across all daemons this GUI is connected to (local + remote hosts). IDs are namespaced \"<host>:<tabid>\".", "inputSchema": obj(map[string]any{})},
		{"name": "get_screen", "description": "Read a tab's visible viewport as text. tab_id is the namespaced ID from list_tabs.", "inputSchema": obj(map[string]any{"tab_id": strProp("namespaced id")}, "tab_id")},
		{"name": "get_scrollback", "description": "Read a tab's scrollback history. Defaults to the last 200 rows.", "inputSchema": obj(map[string]any{"tab_id": strProp("namespaced id"), "from": map[string]any{"type": "integer"}, "to": map[string]any{"type": "integer"}}, "tab_id")},
		{"name": "send_input", "description": "Write raw bytes to a tab's PTY. \\r submits a shell command.", "inputSchema": obj(map[string]any{"tab_id": strProp("namespaced id"), "bytes": strProp("raw bytes")}, "tab_id", "bytes")},
		{"name": "send_paste", "description": "Paste text into a tab (bracketed-paste aware).", "inputSchema": obj(map[string]any{"tab_id": strProp("namespaced id"), "text": strProp("text")}, "tab_id", "text")},
	}
}
