package daemon_test

import (
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/protocol"
)

// TestDaemonStdioTransport exercises the --stdio path without actually
// spawning ssh: we wire two io.Pipe pairs together so the daemon's
// "stdio" reads/writes are the client's stdio writes/reads. Same code
// path as `ssh host xerottyd --stdio` modulo the SSH hop.
func TestDaemonStdioTransport(t *testing.T) {
	// Daemon reads on dRead, writes on dWrite.
	// Client reads on cRead, writes on cWrite.
	// Wire them: client writes → daemon reads, daemon writes → client reads.
	dRead, cWrite := io.Pipe()
	cRead, dWrite := io.Pipe()

	cfg := config.Default()
	d := daemon.New(&cfg, "")

	daemonConn := protocol.NewStdioConn(dRead, dWrite)
	clientConn := protocol.NewStdioConn(cRead, cWrite)

	serveDone := make(chan struct{})
	go func() {
		d.ServeConn(daemonConn)
		close(serveDone)
	}()

	c := clientproto.Wrap(closeIgnoringConn{clientConn})
	if _, err := c.Hello("stdio-test"); err != nil {
		t.Fatalf("hello: %v", err)
	}
	go c.Run()

	if err := c.Attach("", true); err != nil {
		t.Fatalf("attach: %v", err)
	}

	var attached *protocol.Attached
	select {
	case attached = <-c.Attached():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Attached over stdio")
	}
	if len(attached.Tabs) != 1 {
		t.Fatalf("expected 1 tab, got %d", len(attached.Tabs))
	}
	tabID := attached.Tabs[0].ID

	var mirror [][]protocol.Cell
	select {
	case full := <-c.CellFull():
		mirror = full.Grid
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for initial CellFull over stdio")
	}

	marker := "XEROTTY_STDIO_OK"
	if err := c.SendInput(tabID, []byte("echo "+marker+"\r")); err != nil {
		t.Fatalf("send input: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for {
		if mirrorContainsStdio(mirror, marker) {
			break
		}
		select {
		case full := <-c.CellFull():
			mirror = full.Grid
		case diff := <-c.CellDiff():
			for _, e := range diff.Cells {
				if int(e.Row) < len(mirror) && int(e.Col) < len(mirror[e.Row]) {
					mirror[e.Row][e.Col] = e.Cell
				}
			}
		case <-deadline:
			t.Fatalf("timed out waiting for marker %q over stdio transport", marker)
		}
	}

	// Tear down: close client write side → daemon read sees EOF →
	// ServeConn returns. Don't leak the goroutine.
	_ = cWrite.Close()
	_ = dWrite.Close()
	select {
	case <-serveDone:
	case <-time.After(2 * time.Second):
		t.Fatal("daemon ServeConn didn't return after pipes closed")
	}
}

func mirrorContainsStdio(mirror [][]protocol.Cell, needle string) bool {
	for _, row := range mirror {
		var sb strings.Builder
		for _, c := range row {
			if c.Content == "" {
				sb.WriteByte(' ')
				continue
			}
			sb.WriteString(c.Content)
		}
		if strings.Contains(sb.String(), needle) {
			return true
		}
	}
	return false
}

// closeIgnoringConn lets the client side close its conn without also
// closing the underlying pipe twice (which io.PipeWriter would error
// on). The daemon side keeps its own conn handle which closes the
// pipe properly.
type closeIgnoringConn struct {
	net.Conn
}

func (closeIgnoringConn) Close() error { return nil }
