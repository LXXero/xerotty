package runner

import (
	"bufio"
	"bytes"
	"net"
	"path/filepath"
	"strings"
	"testing"
)

// TestBridgeStdio runs the pump against a fake MCP server on a real
// unix socket: one request in via the bridge's stdin, one response
// out via its stdout, clean exit when the server hangs up.
func TestBridgeStdio(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "mcp.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		line, err := bufio.NewReader(c).ReadString('\n')
		if err != nil || !strings.Contains(line, `"initialize"`) {
			return // bridge mangled the request; client gets no reply and the test fails on output
		}
		_, _ = c.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}` + "\n"))
	}()

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n")
	var out bytes.Buffer
	if code := bridgeStdio(conn, in, &out); code != 0 {
		t.Fatalf("bridge exit code %d", code)
	}
	if !strings.Contains(out.String(), `"ok":true`) {
		t.Fatalf("response did not come back through the bridge: %q", out.String())
	}
}
