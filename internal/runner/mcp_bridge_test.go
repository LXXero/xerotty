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
