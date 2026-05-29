package mcp_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/mcp"
	"github.com/LXXero/xerotty/internal/protocol"
)

// TestMCPCreateBroadcastsToWireClient verifies that a tab created by
// an MCP agent (auto mode) is broadcast to a watching wire client via
// MsgTopologyChanged — i.e. MCP-driven structural changes route
// through the same daemon funnel as wire ones.
func TestMCPCreateBroadcastsToWireClient(t *testing.T) {
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

	time.Sleep(80 * time.Millisecond) // listeners bind

	// Wire client attaches to the default session (creating the first
	// tab) and watches the topology stream.
	wc, err := clientproto.Dial(wireSock)
	if err != nil {
		t.Fatalf("dial wire: %v", err)
	}
	defer wc.Close()
	if _, err := wc.Hello("watcher"); err != nil {
		t.Fatalf("wire hello: %v", err)
	}
	go wc.Run()
	if err := wc.Attach("", true); err != nil {
		t.Fatalf("wire attach: %v", err)
	}
	attached := <-wc.Attached()
	if len(attached.Tabs) != 1 {
		t.Fatalf("expected 1 initial tab, got %d", len(attached.Tabs))
	}
	startRev := attached.Revision

	// Drain render frames so Run never back-pressures; keep Topology
	// for the assertion.
	go func() {
		for {
			select {
			case <-wc.Closed():
				return
			case <-wc.CellFull():
			case <-wc.CellDiff():
			case <-wc.Cursor():
			case <-wc.Title():
			case <-wc.Bell():
			case <-wc.ChildExit():
			case <-wc.TabState():
			case <-wc.ScrollbackAppend():
			case <-wc.ScrollbackCleared():
			case <-wc.TabCreated():
			case <-wc.WindowCreated():
			case <-wc.ClipboardSet():
			case <-wc.ProposalsChanged():
			case <-wc.Errors():
			}
		}
	}()

	// MCP agent elevates to auto and creates a tab.
	mc := dialMCP(t, mcpSock)
	defer mc.Close()
	mc.call(t, 1, "agent/mode", map[string]any{"mode": "auto"})
	mc.call(t, 2, "tab/create", map[string]any{"cols": 80, "rows": 24})

	// The wire client should learn about the new tab via a topology
	// broadcast with a newer revision and 2 tabs.
	deadline := time.After(3 * time.Second)
	for {
		select {
		case topo := <-wc.Topology():
			if topo.Revision <= startRev {
				continue
			}
			if len(topo.Tabs) != 2 {
				t.Fatalf("wire client sees %d tabs after MCP create, want 2: %+v", len(topo.Tabs), topo.Tabs)
			}
			return // success
		case <-deadline:
			t.Fatal("wire client never received MsgTopologyChanged for the MCP-created tab")
		}
	}
}

var _ = protocol.MsgTopologyChanged
