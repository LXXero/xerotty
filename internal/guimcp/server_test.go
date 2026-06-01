package guimcp_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/daemonsource"
	"github.com/LXXero/xerotty/internal/guimcp"
)

// fakeBackend is a guimcp.Backend that doesn't need the GUI or a
// real daemon. It returns a fixed tab list and records writes so
// tests can assert send_input / send_paste routing. SourceFor
// returns nil for resolution misses (matching the real "tab not
// found" path) and a sentinel for hits — but since guimcp calls
// real *daemonsource.Source methods on the result, we can only
// safely exercise the resolve-MISS path + the read paths that go
// through ListTabs. send_input/send_paste resolution is tested via
// the NSID round-trip + a not-found assertion.
//
// To exercise the WRITE path against a real Source without a
// daemon, we'd need a live hub; that's covered by the daemonsource
// integration tests. Here we focus on guimcp's own logic:
// JSON-RPC framing, MCP envelope, list_tabs field mapping,
// namespaced-ID parsing, and not-found handling.
type fakeBackend struct {
	tabs []guimcp.TabRef
}

func (f *fakeBackend) ListTabs() []guimcp.TabRef { return f.tabs }
func (f *fakeBackend) SourceFor(nsID string) (*daemonsource.Source, bool) {
	// No real Source available without a daemon — report miss.
	// Tests that need a hit go through the daemonsource integration
	// suite. This still exercises guimcp's not-found branch + the
	// fact that it parses + looks up the namespaced ID.
	return nil, false
}

func dial(t *testing.T, path string) (*mcpConn, func()) {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	return &mcpConn{conn: conn, br: bufio.NewReader(conn)}, func() { conn.Close() }
}

type mcpConn struct {
	conn net.Conn
	br   *bufio.Reader
}

func (c *mcpConn) call(t *testing.T, id int, method string, params any) (json.RawMessage, *struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}) {
	t.Helper()
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	enc, _ := json.Marshal(req)
	if _, err := fmt.Fprintln(c.conn, string(enc)); err != nil {
		t.Fatalf("send: %v", err)
	}
	line, err := c.br.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("decode %s: %v", line, err)
	}
	return resp.Result, resp.Error
}

func startServer(t *testing.T, b guimcp.Backend) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "gui.mcp.sock")
	srv := guimcp.New(b, sock)
	done := make(chan error, 1)
	go func() { done <- srv.Run() }()
	t.Cleanup(func() { _ = srv.Stop(); <-done })
	// Wait for bind.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("unix", sock); err == nil {
			c.Close()
			return sock
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("server never bound")
	return ""
}

func TestGUIMCPInitializeAndToolsList(t *testing.T) {
	sock := startServer(t, &fakeBackend{})
	c, closeC := dial(t, sock)
	defer closeC()

	res, rerr := c.call(t, 1, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	if rerr != nil {
		t.Fatalf("initialize error: %+v", rerr)
	}
	var initOut struct {
		ServerInfo struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
		Capabilities map[string]any `json:"capabilities"`
	}
	json.Unmarshal(res, &initOut)
	if initOut.ServerInfo.Name != "xerotty-gui" {
		t.Errorf("server name %q, want xerotty-gui", initOut.ServerInfo.Name)
	}
	if _, ok := initOut.Capabilities["tools"]; !ok {
		t.Errorf("no tools capability advertised")
	}

	res, _ = c.call(t, 2, "tools/list", nil)
	var tl struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	json.Unmarshal(res, &tl)
	have := map[string]bool{}
	for _, tool := range tl.Tools {
		have[tool.Name] = true
	}
	for _, want := range []string{"list_tabs", "get_screen", "get_scrollback", "send_input", "send_paste"} {
		if !have[want] {
			t.Errorf("tools/list missing %q", want)
		}
	}
}

func TestGUIMCPListTabsAggregatesAndNamespaces(t *testing.T) {
	b := &fakeBackend{tabs: []guimcp.TabRef{
		{NSID: "local:1", Host: "local", Title: "bash", Cols: 80, Rows: 24, CWD: "/home/x", Foreground: "bash", Closed: false, ExitCode: -1, Focused: true},
		{NSID: "kh:3", Host: "kh", Title: "vim", Cols: 120, Rows: 40, CWD: "/srv", Foreground: "vim", Closed: false, ExitCode: -1, Focused: false},
		{NSID: "kh:7", Host: "kh", Title: "", Cols: 80, Rows: 24, CWD: "", Foreground: "", Closed: true, ExitCode: 0, Focused: false},
	}}
	sock := startServer(t, b)
	c, closeC := dial(t, sock)
	defer closeC()

	res, rerr := c.call(t, 1, "list_tabs", nil)
	if rerr != nil {
		t.Fatalf("list_tabs error: %+v", rerr)
	}
	var tabs []struct {
		ID         string `json:"id"`
		Host       string `json:"host"`
		Title      string `json:"title"`
		CWD        string `json:"cwd"`
		Foreground string `json:"foreground"`
		Closed     bool   `json:"closed"`
		ExitCode   int    `json:"exit_code"`
		Focused    bool   `json:"focused"`
	}
	if err := json.Unmarshal(res, &tabs); err != nil {
		t.Fatalf("decode list_tabs: %v", err)
	}
	if len(tabs) != 3 {
		t.Fatalf("want 3 tabs, got %d", len(tabs))
	}
	byID := map[string]int{}
	for i, tab := range tabs {
		byID[tab.ID] = i
	}
	// Namespacing: both hosts represented, distinct prefixes.
	if _, ok := byID["local:1"]; !ok {
		t.Errorf("missing local:1")
	}
	if _, ok := byID["kh:3"]; !ok {
		t.Errorf("missing kh:3")
	}
	// Triage metadata round-trips.
	l := tabs[byID["local:1"]]
	if l.CWD != "/home/x" || l.Foreground != "bash" || !l.Focused {
		t.Errorf("local:1 metadata wrong: %+v", l)
	}
	dead := tabs[byID["kh:7"]]
	if !dead.Closed || dead.ExitCode != 0 {
		t.Errorf("kh:7 should be closed exit 0: %+v", dead)
	}
}

func TestGUIMCPToolsCallEnvelope(t *testing.T) {
	b := &fakeBackend{tabs: []guimcp.TabRef{{NSID: "local:1", Host: "local"}}}
	sock := startServer(t, b)
	c, closeC := dial(t, sock)
	defer closeC()

	// tools/call list_tabs → MCP content envelope wrapping the JSON.
	res, rerr := c.call(t, 1, "tools/call", map[string]any{
		"name": "list_tabs", "arguments": map[string]any{},
	})
	if rerr != nil {
		t.Fatalf("tools/call error: %+v", rerr)
	}
	var wrap struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	json.Unmarshal(res, &wrap)
	if wrap.IsError || len(wrap.Content) != 1 || wrap.Content[0].Type != "text" {
		t.Fatalf("bad envelope: %+v", wrap)
	}
	// The text payload is the JSON list_tabs result.
	var inner []map[string]any
	if err := json.Unmarshal([]byte(wrap.Content[0].Text), &inner); err != nil {
		t.Fatalf("content text not the JSON result: %v", err)
	}
	if len(inner) != 1 || inner[0]["id"] != "local:1" {
		t.Errorf("envelope content wrong: %s", wrap.Content[0].Text)
	}
}

func TestGUIMCPNamespacedResolveMiss(t *testing.T) {
	sock := startServer(t, &fakeBackend{})
	c, closeC := dial(t, sock)
	defer closeC()

	// SourceFor always misses in the fake → get_screen / send_input
	// return "tab not found", exercising guimcp's parse + lookup +
	// error path for namespaced IDs.
	for _, tc := range []struct {
		method string
		params map[string]any
	}{
		{"get_screen", map[string]any{"tab_id": "kh:99"}},
		{"send_input", map[string]any{"tab_id": "kh:99", "bytes": "ls\r"}},
		{"send_paste", map[string]any{"tab_id": "kh:99", "text": "x"}},
		{"get_scrollback", map[string]any{"tab_id": "kh:99"}},
	} {
		_, rerr := c.call(t, 1, tc.method, tc.params)
		if rerr == nil {
			t.Errorf("%s on missing tab: expected error, got success", tc.method)
			continue
		}
		if rerr.Code != -32004 {
			t.Errorf("%s: code %d, want -32004 (not found)", tc.method, rerr.Code)
		}
	}
}
