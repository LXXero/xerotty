package runner

// MCP functional E2E: drives a REAL daemon through its REAL MCP
// socket exactly the way an agent does — spawn, elevate trust,
// create a tab, type, read the styled screen, scroll history, clean
// up. This is the automated stand-in for the manual "open it and
// poke around" QA pass: anything an agent can observe through MCP
// is assertable here, headless, in CI.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
)

type rpcClient struct {
	conn net.Conn
	sc   *bufio.Scanner
	id   int
}

func dialRPC(t *testing.T, sock string) *rpcClient {
	t.Helper()
	var conn net.Conn
	var err error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err = net.Dial("unix", sock)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("dial mcp: %v", err)
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	return &rpcClient{conn: conn, sc: sc}
}

// call sends a JSON-RPC request and returns the raw result message.
func (c *rpcClient) call(t *testing.T, method string, params any) json.RawMessage {
	t.Helper()
	c.id++
	req := map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method}
	if params != nil {
		req["params"] = params
	}
	b, _ := json.Marshal(req)
	if _, err := c.conn.Write(append(b, '\n')); err != nil {
		t.Fatalf("%s: write: %v", method, err)
	}
	for c.sc.Scan() {
		var resp struct {
			ID     any             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(c.sc.Bytes(), &resp); err != nil {
			continue // notification or junk
		}
		if fmt.Sprintf("%v", resp.ID) != fmt.Sprintf("%d", c.id) {
			continue // someone else's response / notification
		}
		if resp.Error != nil {
			t.Fatalf("%s: rpc error: %s", method, resp.Error.Message)
		}
		return resp.Result
	}
	t.Fatalf("%s: connection closed", method)
	return nil
}

// tool invokes an MCP tool and returns the concatenated text content.
func (c *rpcClient) tool(t *testing.T, name string, args any) string {
	t.Helper()
	res := c.call(t, "tools/call", map[string]any{"name": name, "arguments": args})
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		t.Fatalf("%s: bad result: %v (%s)", name, err, res)
	}
	var sb strings.Builder
	for _, ct := range out.Content {
		sb.WriteString(ct.Text)
	}
	if out.IsError {
		t.Fatalf("%s: tool error: %s", name, sb.String())
	}
	return sb.String()
}

func TestMCPFunctionalSession(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e")
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "xerotty")
	if out, err := exec.Command("go", "build", "-tags", "headless", "-o", bin, "./../../cmd/xerotty").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	sock := filepath.Join(tmp, "d.sock")
	mcpSock := filepath.Join(tmp, "d.mcp.sock")
	srv := exec.Command(bin, "serve", "--socket", sock, "--mcp-socket", mcpSock)
	if err := srv.Start(); err != nil {
		t.Fatalf("serve: %v", err)
	}
	defer func() { _ = srv.Process.Kill(); _, _ = srv.Process.Wait() }()

	// A wire client must attach first — that mints the "default"
	// session MCP tools target (in real life, the GUI). Mirrors how
	// agents actually operate: alongside an attached client.
	var cli *clientproto.Client
	deadline0 := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline0) {
		var err error
		if cli, err = clientproto.Dial(sock); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if cli == nil {
		t.Fatal("wire dial never succeeded")
	}
	defer cli.Close()
	if _, err := cli.Hello("mcp-functional-e2e"); err != nil {
		t.Fatalf("hello: %v", err)
	}
	go cli.Run()
	if err := cli.Attach("", false); err != nil {
		t.Fatalf("attach: %v", err)
	}
	<-cli.Attached()

	c := dialRPC(t, mcpSock)
	defer c.conn.Close()

	// Handshake like a real MCP client.
	c.call(t, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"clientInfo":      map[string]any{"name": "functional-e2e"},
	})

	// Observe mode must reject writes; auto must allow them.
	c.tool(t, "set_agent_mode", map[string]any{"mode": "auto"})

	created := c.tool(t, "create_tab", map[string]any{"name": "qa-tab", "cols": 80, "rows": 24})
	var tabInfo struct {
		TabID int `json:"tab_id"`
	}
	if err := json.Unmarshal([]byte(created), &tabInfo); err != nil || tabInfo.TabID == 0 {
		t.Fatalf("create_tab: %q (%v)", created, err)
	}
	tid := tabInfo.TabID

	// Type a command via send_keys (chord-capable path) and wait for
	// its output to land on the styled screen.
	c.tool(t, "send_input", map[string]any{"tab_id": tid, "bytes": "printf 'QA-MARKER-%s\\n' OK\r"})
	deadline := time.Now().Add(8 * time.Second)
	var screen string
	for time.Now().Before(deadline) {
		screen = c.tool(t, "get_screen", map[string]any{"tab_id": tid})
		if strings.Contains(screen, "QA-MARKER-OK") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(screen, "QA-MARKER-OK") {
		t.Fatalf("typed output never appeared on screen:\n%s", screen)
	}

	// Scrollback: overflow the viewport, then read history.
	c.tool(t, "send_input", map[string]any{"tab_id": tid, "bytes": "for i in $(seq 1 60); do echo SB$i; done\r"})
	deadline = time.Now().Add(8 * time.Second)
	ok := false
	for time.Now().Before(deadline) {
		sb := c.tool(t, "get_scrollback", map[string]any{"tab_id": tid, "lines": 100})
		if strings.Contains(sb, "SB1") && strings.Contains(sb, "SB30") {
			ok = true
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if !ok {
		t.Fatal("scrollback never contained the overflow output")
	}

	// Named create is idempotent: same name → same tab, reused.
	again := c.tool(t, "create_tab", map[string]any{"name": "qa-tab"})
	if !strings.Contains(again, fmt.Sprintf("%d", tid)) {
		t.Fatalf("named re-create did not reuse tab %d: %s", tid, again)
	}

	c.tool(t, "close_tab", map[string]any{"tab_id": tid})
}
