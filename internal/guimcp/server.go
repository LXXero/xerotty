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
//	send_keys    {tab_id, text?, keys?[]}
//	send_paste   {tab_id, text}
//	create_tab   {host?, name?, cols?, rows?} -> {tab_id, reused}
//	close_tab    {tab_id}
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

	"github.com/LXXero/xerotty/internal/daemonsource"
	"github.com/LXXero/xerotty/internal/screentext"
	"github.com/LXXero/xerotty/internal/sendkeys"
	"github.com/LXXero/xerotty/internal/sockpath"
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
	// CreateTab opens (or, for a non-empty name, finds) a tab on the
	// named host's daemon. host "" means the default ("local") hub.
	// Returns the namespaced ID and whether an existing named tab was
	// reused. The new tab pops into the GUI via the daemon's topology
	// broadcast — same path as any other client creating one.
	CreateTab(host, name string, cols, rows int) (nsID string, reused bool, err error)
	// CloseTab closes the tab (daemon-side close; the GUI tab reaps
	// via the normal vanish path).
	CloseTab(nsID string) error
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
	// Record where we actually bound so `xerotty mcp` (possibly
	// spawned with a different environment — macOS TMPDIR varies by
	// launch context) can find us without recomputing defaults.
	_ = sockpath.Record(sockpath.RecordGUIMCP, s.socketPath)
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
	case "send_keys":
		return s.sendKeys(req.ID, req.Params)
	case "send_paste":
		return s.sendPaste(req.ID, req.Params)
	case "create_tab":
		return s.createTab(req.ID, req.Params)
	case "close_tab":
		return s.closeTab(req.ID, req.Params)
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
	case "create_tab":
		inner = s.createTab(req.ID, p.Arguments)
	case "close_tab":
		inner = s.closeTab(req.ID, p.Arguments)
	case "send_input":
		inner = s.sendInput(req.ID, p.Arguments)
	case "send_keys":
		inner = s.sendKeys(req.ID, p.Arguments)
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
		TabID  string `json:"tab_id"`
		Styled bool   `json:"styled,omitempty"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return rpcErrResp(id, -32602, err.Error())
	}
	src, ok := s.backend.SourceFor(p.TabID)
	if !ok {
		return rpcErrResp(id, -32004, "tab not found: "+p.TabID)
	}
	grid := src.SnapshotViewport()
	cols := 0
	if len(grid) > 0 {
		cols = len(grid[0])
	}
	pos := src.Emulator().CursorPosition()
	res := map[string]any{
		"cols": cols, "rows": len(grid),
		"cursor": map[string]any{
			"row": pos.Y, "col": pos.X, "visible": src.CursorVisible(),
		},
	}
	if p.Styled {
		res["runs"] = screentext.StyledLines(grid)
	} else {
		res["lines"] = screentext.Lines(grid)
	}
	return okResp(id, res)
}

func (s *Server) getScrollback(id json.RawMessage, params json.RawMessage) *rpcResponse {
	var p struct {
		TabID  string `json:"tab_id"`
		From   *int   `json:"from,omitempty"`
		To     *int   `json:"to,omitempty"`
		Styled bool   `json:"styled,omitempty"`
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
	res := map[string]any{"from": from, "to": to, "total": total}
	if to > from {
		grid := src.SnapshotScrollbackRange(from, to)
		if p.Styled {
			res["runs"] = screentext.StyledLines(grid)
		} else {
			res["lines"] = screentext.Lines(grid)
		}
	} else if !p.Styled {
		res["lines"] = []string{}
	}
	return okResp(id, res)
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

func (s *Server) sendKeys(id json.RawMessage, params json.RawMessage) *rpcResponse {
	var p struct {
		TabID string   `json:"tab_id"`
		Text  string   `json:"text,omitempty"`
		Keys  []string `json:"keys,omitempty"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return rpcErrResp(id, -32602, err.Error())
	}
	src, ok := s.backend.SourceFor(p.TabID)
	if !ok {
		return rpcErrResp(id, -32004, "tab not found: "+p.TabID)
	}
	// Text first (typed verbatim), then the keys — so the common
	// "type command, press enter" is one call with no escape grammar.
	buf := []byte(p.Text)
	kb, err := sendkeys.Translate(p.Keys, src.AppCursorMode())
	if err != nil {
		return rpcErrResp(id, -32602, err.Error())
	}
	buf = append(buf, kb...)
	if len(buf) == 0 {
		return rpcErrResp(id, -32602, "nothing to send: pass text and/or keys")
	}
	if _, err := src.Write(buf); err != nil {
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

func (s *Server) createTab(id json.RawMessage, params json.RawMessage) *rpcResponse {
	var p struct {
		Host string `json:"host,omitempty"`
		Name string `json:"name,omitempty"`
		Cols int    `json:"cols,omitempty"`
		Rows int    `json:"rows,omitempty"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return rpcErrResp(id, -32602, err.Error())
		}
	}
	if p.Cols <= 0 {
		p.Cols = 80
	}
	if p.Rows <= 0 {
		p.Rows = 24
	}
	nsID, reused, err := s.backend.CreateTab(p.Host, p.Name, p.Cols, p.Rows)
	if err != nil {
		return rpcErrResp(id, -32000, "create tab: "+err.Error())
	}
	return okResp(id, map[string]any{"tab_id": nsID, "name": p.Name, "reused": reused})
}

func (s *Server) closeTab(id json.RawMessage, params json.RawMessage) *rpcResponse {
	var p struct {
		TabID string `json:"tab_id"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return rpcErrResp(id, -32602, err.Error())
	}
	if err := s.backend.CloseTab(p.TabID); err != nil {
		return rpcErrResp(id, -32004, err.Error())
	}
	return okResp(id, map[string]any{"closed": p.TabID})
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
		{"name": "get_screen", "description": "Read a tab's visible viewport. tab_id is the namespaced ID from list_tabs. Always includes cursor {row, col, visible}. With styled=true, lines become runs of styled text ({t, fg, bg, a}) instead of flat strings — use it to tell presentation apart from content: faint (a:\"faint\") text at/after the cursor is typically a TUI's autocomplete ghost text the user has NOT typed; red fg usually means an error.", "inputSchema": obj(map[string]any{"tab_id": strProp("namespaced id"), "styled": map[string]any{"type": "boolean", "description": "return styled runs instead of flat lines"}}, "tab_id")},
		{"name": "get_scrollback", "description": "Read a tab's scrollback history. Defaults to the last 200 rows. styled=true returns styled runs instead of flat lines (see get_screen).", "inputSchema": obj(map[string]any{"tab_id": strProp("namespaced id"), "from": map[string]any{"type": "integer"}, "to": map[string]any{"type": "integer"}, "styled": map[string]any{"type": "boolean"}}, "tab_id")},
		{"name": "send_input", "description": "Write raw bytes to a tab's PTY. The string is used as-is after standard JSON unescaping — no extra escape layer. Prefer send_keys for keystrokes (enter, ctrl+c, arrows): it cannot be mis-escaped.", "inputSchema": obj(map[string]any{"tab_id": strProp("namespaced id"), "bytes": strProp("raw bytes")}, "tab_id", "bytes")},
		{"name": "send_keys", "description": "Press keys by NAME — use this instead of guessing raw byte escapes for send_input (sending Enter as \\r/\\n escape soup is a known failure loop; here it is just \"enter\"). Optional `text` is typed first, completely literally (no escape interpretation), then each `keys` token is pressed in order. Tokens: a single literal character, or a named key (enter, esc, tab, backspace, space, delete, insert, up, down, left, right, home, end, pageup, pagedown, f1-f12), with optional modifier prefixes joined by + or - (ctrl+c, alt+enter, ctrl+shift+up, ctrl++ = ctrl and '+'; tmux-style C-c / M-x also accepted). Arrows honor the tab's app-cursor mode automatically. Example: run a command = {text: \"ls\", keys: [\"enter\"]}; interrupt = {keys: [\"ctrl+c\"]}.", "inputSchema": obj(map[string]any{"tab_id": strProp("namespaced id"), "text": strProp("literal text typed before the keys"), "keys": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "key tokens pressed in order"}}, "tab_id")},
		{"name": "send_paste", "description": "Paste text into a tab (bracketed-paste aware).", "inputSchema": obj(map[string]any{"tab_id": strProp("namespaced id"), "text": strProp("text")}, "tab_id", "text")},
		{"name": "create_tab", "description": "Open a tab on a host's daemon (default: the local one). Pass a stable `name` to make this idempotent — the first call spawns and labels the tab, later calls with the same name return that same tab (reused=true) instead of stacking duplicates; prefer that over spawning fresh tabs. The tab appears in the user's GUI immediately. Returns the namespaced tab_id.", "inputSchema": obj(map[string]any{"host": strProp("host namespace from list_tabs (default \"\" = local)"), "name": strProp("stable reuse label (e.g. \"build\"); omit for a one-off tab"), "cols": map[string]any{"type": "integer"}, "rows": map[string]any{"type": "integer"}})},
		{"name": "close_tab", "description": "Close a tab (daemon-side; it disappears from the user's GUI too). tab_id is the namespaced ID.", "inputSchema": obj(map[string]any{"tab_id": strProp("namespaced id")}, "tab_id")},
	}
}
