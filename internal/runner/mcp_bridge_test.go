package runner

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// echoServer answers each request line with a canned response and —
// when dropAfter > 0 — closes the connection after that many
// responses, simulating a daemon hot upgrade hanging up mid-session.
func echoServer(t *testing.T, ln net.Listener, dropAfter int) {
	t.Helper()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				n := 0
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					_ = line
					n++
					fmt.Fprintf(c, `{"jsonrpc":"2.0","id":%d,"result":{"n":%d}}`+"\n", n, n)
					if dropAfter > 0 && n >= dropAfter {
						return // hang up — bridge must reconnect
					}
				}
			}(c)
		}
	}()
}

// TestBridgeRoundTripAndCleanExit: request in, response out, exit 0
// when the client closes stdin.
func TestBridgeRoundTripAndCleanExit(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "mcp.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	echoServer(t, ln, 0)

	discover := func() net.Conn {
		c, err := net.Dial("unix", sock)
		if err != nil {
			return nil
		}
		return c
	}
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"x"}` + "\n")
	var out bytes.Buffer
	if code := runBridge(discover, in, &out); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(out.String(), `"n":1`) {
		t.Fatalf("response did not come back: %q", out.String())
	}
}

// TestBridgeReconnects is the daemon-hot-upgrade story: the server
// hangs up after the first response; the bridge must re-discover,
// reconnect, and keep serving — the MCP client never notices.
func TestBridgeReconnects(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "mcp.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	echoServer(t, ln, 1) // drop the conn after every response

	var dials atomic.Int32
	discover := func() net.Conn {
		c, err := net.Dial("unix", sock)
		if err != nil {
			return nil
		}
		dials.Add(1)
		return c
	}

	// Feed two requests with a pause so the first response (and the
	// hangup) land before the second request is written.
	pr, pw := io.Pipe()
	go func() {
		fmt.Fprintln(pw, `{"jsonrpc":"2.0","id":1,"method":"a"}`)
		time.Sleep(300 * time.Millisecond)
		fmt.Fprintln(pw, `{"jsonrpc":"2.0","id":2,"method":"b"}`)
		time.Sleep(300 * time.Millisecond)
		pw.Close()
	}()
	var out bytes.Buffer
	if code := runBridge(discover, pr, &out); code != 0 {
		t.Fatalf("exit code %d (out: %q)", code, out.String())
	}
	got := out.String()
	if strings.Count(got, `"n":1`) != 2 {
		// Each fresh connection answers with n=1 — two of them
		// proves both requests were served over distinct conns.
		t.Fatalf("expected two first-responses across two conns, got %q", got)
	}
	if dials.Load() < 2 {
		t.Fatalf("bridge never reconnected (dials=%d)", dials.Load())
	}
}

// TestBridgeReplaysModeAcrossReconnect: the daemon's trust mode is
// per-connection, so the bridge must re-assert the client's last
// requested mode on every reconnect — otherwise a daemon hot upgrade
// silently demotes agents to observe and their writes start failing.
func TestBridgeReplaysModeAcrossReconnect(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "mcp.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	received := make(chan string, 64)
	var connN atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n := connN.Add(1)
			go func(c net.Conn, n int32) {
				defer c.Close()
				br := bufio.NewReader(c)
				for i := 0; ; i++ {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					received <- line
					fmt.Fprintf(c, `{"jsonrpc":"2.0","id":"resp-%d","result":{}}`+"\n", i)
					if n == 1 {
						return // first conn drops after one request → forces ONE reconnect
					}
				}
			}(c, n)
		}
	}()

	pr, pw := io.Pipe()
	go func() {
		// Elevate via the MCP tool shape, then send a write after the
		// server has dropped the first conn.
		fmt.Fprintln(pw, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"set_agent_mode","arguments":{"mode":"auto"}}}`)
		time.Sleep(400 * time.Millisecond)
		fmt.Fprintln(pw, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"send_input","arguments":{"tab_id":1,"bytes":"x"}}}`)
		time.Sleep(1200 * time.Millisecond) // span the flap-guard backoff
		pw.Close()
	}()
	var out bytes.Buffer
	discover := func() net.Conn {
		c, err := net.Dial("unix", sock)
		if err != nil {
			return nil
		}
		return c
	}
	if code := runBridge(discover, pr, &out); code != 0 {
		t.Fatalf("exit %d", code)
	}

	var lines []string
	for {
		select {
		case l := <-received:
			lines = append(lines, l)
		default:
			goto donedrain
		}
	}
donedrain:
	// Expect: conn1 got set_agent_mode; conn2 (post-drop) got the
	// REPLAYED agent/mode BEFORE the send_input.
	replayed := -1
	input := -1
	for i, l := range lines {
		if strings.Contains(l, bridgeModeReplayID) && strings.Contains(l, `"auto"`) {
			replayed = i
		}
		if strings.Contains(l, "send_input") {
			input = i
		}
	}
	if replayed == -1 {
		t.Fatalf("mode never replayed on reconnect; server saw:\n%s", strings.Join(lines, ""))
	}
	if input != -1 && replayed > input {
		t.Fatalf("replay arrived after the write it was meant to protect")
	}
	// And the client must never see the synthetic replay response.
	if strings.Contains(out.String(), bridgeModeReplayID) {
		t.Fatalf("replay response leaked to client: %q", out.String())
	}
}
