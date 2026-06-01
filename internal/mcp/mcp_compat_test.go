package mcp_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/mcp"
)

// TestMCPStandardProtocol exercises the Model Context Protocol
// shape: initialize handshake → tools/list catalogue → tools/call
// to dispatch. Same underlying capability as the native methods,
// just wrapped in the MCP envelope so generic MCP clients (Claude
// Code, Xyphia) can connect.
func TestMCPStandardProtocol(t *testing.T) {
	dir := t.TempDir()
	wireSock := filepath.Join(dir, "xerottyd.sock")
	mcpSock := filepath.Join(dir, "xerottyd.mcp.sock")

	cfg := config.Default()
	d := daemon.New(&cfg, wireSock)
	doneRun := make(chan error, 1)
	go func() { doneRun <- d.Run() }()
	defer func() { _ = d.Stop(); <-doneRun }()

	srv := mcp.New(d, mcpSock)
	doneMCP := make(chan error, 1)
	go func() { doneMCP <- srv.Run() }()
	defer func() { _ = srv.Stop(); <-doneMCP }()

	time.Sleep(80 * time.Millisecond)

	// Set up a tab via the wire protocol so tools/call has
	// something to act on.
	wc, _ := clientproto.Dial(wireSock)
	defer wc.Close()
	wc.Hello("setup")
	go wc.Run()
	wc.Attach("", true)
	attached := <-wc.Attached()
	tabID := attached.Tabs[0].ID
	go func() {
		for {
			select {
			case <-wc.CellFull():
			case <-wc.CellDiff():
			case <-wc.Cursor():
			case <-wc.Closed():
				return
			}
		}
	}()

	mc := dialMCPRaw(t, mcpSock)
	defer mc.Close()

	// initialize handshake
	initRes := mc.call(t, 1, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "mcp-test", "version": "0"},
	})
	var initOut struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(initRes, &initOut); err != nil {
		t.Fatalf("initialize result decode: %v", err)
	}
	if initOut.ServerInfo.Name != "xerotty" {
		t.Errorf("server name: got %q want xerotty", initOut.ServerInfo.Name)
	}
	if _, ok := initOut.Capabilities["tools"]; !ok {
		t.Errorf("server didn't advertise tools capability")
	}

	// notifications/initialized — should not respond.
	mc.notify(t, "notifications/initialized", nil)

	// tools/list
	listRes := mc.call(t, 2, "tools/list", nil)
	var listOut struct {
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(listRes, &listOut); err != nil {
		t.Fatalf("tools/list decode: %v", err)
	}
	if len(listOut.Tools) < 5 {
		t.Errorf("expected at least 5 tools, got %d", len(listOut.Tools))
	}
	have := map[string]bool{}
	for _, tt := range listOut.Tools {
		have[tt.Name] = true
	}
	for _, want := range []string{"list_tabs", "get_screen", "send_input", "set_agent_mode"} {
		if !have[want] {
			t.Errorf("tools/list missing %q", want)
		}
	}

	// tools/call list_tabs — should wrap result in MCP content.
	callRes := mc.call(t, 3, "tools/call", map[string]any{
		"name":      "list_tabs",
		"arguments": map[string]any{},
	})
	var wrap struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(callRes, &wrap); err != nil {
		t.Fatalf("tools/call decode: %v", err)
	}
	if wrap.IsError {
		t.Errorf("list_tabs returned isError=true")
	}
	if len(wrap.Content) != 1 || wrap.Content[0].Type != "text" {
		t.Fatalf("expected single text content block, got %+v", wrap.Content)
	}
	if !strings.Contains(wrap.Content[0].Text, `"id"`) {
		t.Errorf("list_tabs content doesn't look like a tab list: %s", wrap.Content[0].Text)
	}

	// tools/call set_agent_mode → auto, then send_input.
	_ = mc.call(t, 4, "tools/call", map[string]any{
		"name":      "set_agent_mode",
		"arguments": map[string]any{"mode": "auto"},
	})
	marker := "XEROTTY_MCP_COMPAT_OK"
	_ = mc.call(t, 5, "tools/call", map[string]any{
		"name":      "send_input",
		"arguments": map[string]any{"tab_id": tabID, "bytes": "echo " + marker + "\r"},
	})

	// Poll get_screen via tools/call until the marker shows up.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res := mc.call(t, 6, "tools/call", map[string]any{
			"name":      "get_screen",
			"arguments": map[string]any{"tab_id": tabID},
		})
		var w struct {
			Content []struct{ Text string } `json:"content"`
		}
		_ = json.Unmarshal(res, &w)
		if len(w.Content) > 0 && strings.Contains(w.Content[0].Text, marker) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("marker %q never appeared via tools/call(get_screen)", marker)
}

// dialMCPRaw uses the existing test helper from server_test.go.
func dialMCPRaw(t *testing.T, path string) *mcpClient {
	return dialMCP(t, path)
}

// notify sends a JSON-RPC notification (no id, no expected reply).
func (c *mcpClient) notify(t *testing.T, method string, params any) {
	t.Helper()
	req := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
	}
	if params != nil {
		req["params"] = params
	}
	enc, _ := json.Marshal(req)
	_, _ = c.conn.Write(append(enc, '\n'))
	// Server should NOT respond to a notification. We don't read
	// back — if the server incorrectly sends a reply, subsequent
	// call() reads will pick it up and mismatch the id.
}
