package runner

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
)

// mcpProbe is a minimal line-JSON-RPC client for poking the test
// daemon's MCP socket.
type mcpProbe struct {
	conn net.Conn
	br   *bufio.Reader
	id   int
}

func dialProbe(path string) (*mcpProbe, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, err
	}
	return &mcpProbe{conn: conn, br: bufio.NewReader(conn)}, nil
}

func (p *mcpProbe) call(method string, params any) (json.RawMessage, error) {
	p.id++
	req := map[string]any{"jsonrpc": "2.0", "id": p.id, "method": method}
	if params != nil {
		req["params"] = params
	}
	b, _ := json.Marshal(req)
	if _, err := fmt.Fprintln(p.conn, string(b)); err != nil {
		return nil, err
	}
	line, err := p.br.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("rpc %s: %s", method, resp.Error.Message)
	}
	return resp.Result, nil
}

func (p *mcpProbe) screen(tabID uint32) (string, error) {
	res, err := p.call("tab/screen", map[string]any{"tab_id": tabID})
	if err != nil {
		return "", err
	}
	var sc struct {
		Lines []string `json:"lines"`
	}
	if err := json.Unmarshal(res, &sc); err != nil {
		return "", err
	}
	return strings.Join(sc.Lines, "\n"), nil
}

// attachWireClient creates the "default" session + initial tab the
// way a GUI would (the daemon only materializes a session on wire
// attach). Keeps draining frames until the daemon hangs up on it at
// upgrade time — which is expected and fine.
func attachWireClient(t *testing.T, sock string) (cleanup func(), instanceID string) {
	t.Helper()
	cli, err := clientproto.Dial(sock)
	if err != nil {
		t.Fatalf("wire dial: %v", err)
	}
	if _, err := cli.Hello("upgrade-e2e"); err != nil {
		t.Fatalf("wire hello: %v", err)
	}
	go cli.Run()
	if err := cli.Attach("", true); err != nil {
		t.Fatalf("wire attach: %v", err)
	}
	select {
	case att := <-cli.Attached():
		instanceID = att.InstanceID
	case <-time.After(5 * time.Second):
		t.Fatal("never attached")
	}
	go func() {
		for {
			select {
			case <-cli.Closed():
				return
			case <-cli.CellFull():
			case <-cli.CellDiff():
			case <-cli.Cursor():
			case <-cli.TabState():
			case <-cli.ScrollbackAppend():
			case <-cli.Topology():
			case <-cli.TabCreated():
			case <-cli.Title():
			case <-cli.Errors():
			}
		}
	}()
	return func() { cli.Close() }, instanceID
}
