package mcp_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/mcp"
)

// TestMCPRoundTrip exercises every Phase 4 method against a live
// daemon. Wires up:
//
//   - the regular wire-protocol daemon on one unix socket (so we have
//     a session + tab to talk to)
//   - the MCP server on a second socket
//   - one wire-protocol client that creates a session/tab
//   - one MCP client (raw JSON-RPC over the second socket) that
//     reads + writes via the agent surface
//
// The PTY echo confirms an MCP write actually landed on the tab.
func TestMCPRoundTrip(t *testing.T) {
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

	time.Sleep(80 * time.Millisecond) // listeners need to bind

	// Set up a session + tab via the wire protocol.
	wc, err := clientproto.Dial(wireSock)
	if err != nil {
		t.Fatalf("dial wire: %v", err)
	}
	defer wc.Close()
	if _, err := wc.Hello("setup-client"); err != nil {
		t.Fatalf("wire hello: %v", err)
	}
	go wc.Run()
	if err := wc.Attach("", true); err != nil {
		t.Fatalf("wire attach: %v", err)
	}
	attached := <-wc.Attached()
	tabID := attached.Tabs[0].ID

	// Drain initial cell traffic so it doesn't backpressure the
	// publishLoop while the test runs.
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

	// Connect MCP client and round-trip every method.
	mc := dialMCP(t, mcpSock)
	defer mc.Close()

	// tabs/list
	listRes := mc.call(t, 1, "tabs/list", nil)
	var tabs []struct {
		ID    uint32 `json:"id"`
		Title string `json:"title"`
		Cols  uint16 `json:"cols"`
		Rows  uint16 `json:"rows"`
	}
	if err := json.Unmarshal(listRes, &tabs); err != nil {
		t.Fatalf("tabs/list result: %v: %s", err, listRes)
	}
	if len(tabs) != 1 || tabs[0].ID != tabID {
		t.Fatalf("tabs/list: got %+v, want one tab id %d", tabs, tabID)
	}

	// tab/screen — should work in observe mode (default).
	screenRes := mc.call(t, 2, "tab/screen", map[string]any{"tab_id": tabID})
	var screen struct {
		Cols  uint16   `json:"cols"`
		Rows  uint16   `json:"rows"`
		Lines []string `json:"lines"`
	}
	if err := json.Unmarshal(screenRes, &screen); err != nil {
		t.Fatalf("tab/screen result: %v", err)
	}
	if screen.Cols == 0 || screen.Rows == 0 || len(screen.Lines) == 0 {
		t.Fatalf("tab/screen returned empty screen: %+v", screen)
	}

	// tab/input in observe mode should be blocked.
	errResp := mc.callExpectErr(t, 3, "tab/input", map[string]any{
		"tab_id": tabID, "bytes": "ls\r",
	})
	if errResp.Code != -32099 {
		t.Errorf("expected -32099 for write-in-observe, got %d (%s)", errResp.Code, errResp.Message)
	}

	// Switch to auto mode and try again.
	modeRes := mc.call(t, 4, "agent/mode", map[string]any{"mode": "auto"})
	var modeOut struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(modeRes, &modeOut); err != nil {
		t.Fatalf("agent/mode result: %v", err)
	}
	if modeOut.Mode != "auto" {
		t.Fatalf("mode set: got %q, want auto", modeOut.Mode)
	}

	marker := "XEROTTY_MCP_OK"
	inputRes := mc.call(t, 5, "tab/input", map[string]any{
		"tab_id": tabID, "bytes": "echo " + marker + "\r",
	})
	var okOut struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(inputRes, &okOut); err != nil || !okOut.OK {
		t.Fatalf("tab/input in auto: %v %s", err, inputRes)
	}

	// Poll tab/screen until the marker shows up — proves the MCP
	// write actually hit the PTY.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res := mc.call(t, 6, "tab/screen", map[string]any{"tab_id": tabID})
		if err := json.Unmarshal(res, &screen); err != nil {
			t.Fatalf("tab/screen poll: %v", err)
		}
		for _, line := range screen.Lines {
			if strings.Contains(line, marker) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("marker %q never appeared in tab/screen lines: %+v", marker, screen.Lines)
}

// TestMCPProposeQueuesWrite confirms that propose mode accepts the
// write request but does NOT pass it through to the PTY; it lands
// on the session's proposed-actions queue instead.
func TestMCPProposeQueuesWrite(t *testing.T) {
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

	wc, err := clientproto.Dial(wireSock)
	if err != nil {
		t.Fatalf("dial wire: %v", err)
	}
	defer wc.Close()
	if _, err := wc.Hello("propose-setup"); err != nil {
		t.Fatalf("wire hello: %v", err)
	}
	go wc.Run()
	if err := wc.Attach("", true); err != nil {
		t.Fatalf("wire attach: %v", err)
	}
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

	mc := dialMCP(t, mcpSock)
	defer mc.Close()

	_ = mc.call(t, 1, "agent/mode", map[string]any{"mode": "propose"})
	res := mc.call(t, 2, "tab/input", map[string]any{
		"tab_id": tabID, "bytes": "dangerous-command\r",
	})
	var queued struct {
		Queued bool `json:"queued"`
	}
	if err := json.Unmarshal(res, &queued); err != nil || !queued.Queued {
		t.Fatalf("propose write: expected queued:true, got %s", res)
	}

	sess := d.SessionByName("default")
	pending := sess.PendingProposals()
	if len(pending) != 1 {
		t.Fatalf("expected 1 queued proposal, got %d", len(pending))
	}
	if pending[0].TabID != tabID || string(pending[0].Bytes) != "dangerous-command\r" {
		t.Errorf("proposal mismatch: %+v", pending[0])
	}
}

// --- helpers ---

type mcpClient struct {
	conn net.Conn
	br   *bufio.Reader
}

func dialMCP(t *testing.T, path string) *mcpClient {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial mcp %s: %v", path, err)
	}
	return &mcpClient{conn: conn, br: bufio.NewReader(conn)}
}

func (c *mcpClient) Close() error { return c.conn.Close() }

func (c *mcpClient) call(t *testing.T, id int, method string, params any) json.RawMessage {
	t.Helper()
	resp := c.callRaw(t, id, method, params)
	if resp.Error != nil {
		t.Fatalf("rpc %s: error %d %s", method, resp.Error.Code, resp.Error.Message)
	}
	return resp.Result
}

func (c *mcpClient) callExpectErr(t *testing.T, id int, method string, params any) struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
} {
	t.Helper()
	resp := c.callRaw(t, id, method, params)
	if resp.Error == nil {
		t.Fatalf("rpc %s: expected error, got result %s", method, resp.Result)
	}
	return struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}{resp.Error.Code, resp.Error.Message}
}

func (c *mcpClient) callRaw(t *testing.T, id int, method string, params any) struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
} {
	t.Helper()
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		req["params"] = params
	}
	enc, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("encode req: %v", err)
	}
	if _, err := fmt.Fprintln(c.conn, string(enc)); err != nil {
		t.Fatalf("send rpc: %v", err)
	}
	line, err := c.br.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read rpc response: %v", err)
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("decode rpc response %s: %v", line, err)
	}
	return resp
}
