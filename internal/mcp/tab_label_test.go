package mcp_test

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/mcp"
	"github.com/LXXero/xerotty/internal/testutil"
)

// TestMCPTabLabel checks that the daemon's list_tabs reports each
// tab's label: the assigned name for a named tab, and the program
// title without its leading status glyph for an unnamed one.
func TestMCPTabLabel(t *testing.T) {
	dir := testutil.SockDir(t)
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
	if _, err := wc.Hello("setup"); err != nil {
		t.Fatalf("hello: %v", err)
	}
	go wc.Run()
	if err := wc.Attach("", true); err != nil {
		t.Fatalf("attach: %v", err)
	}
	attached := <-wc.Attached()
	claudeID := attached.Tabs[0].ID
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

	sess := d.SessionByName("default")
	if sess == nil {
		t.Fatal("no default session")
	}
	named, _, err := sess.NewTab(0, 80, 24, "", "build", nil)
	if err != nil {
		t.Fatalf("new named tab: %v", err)
	}
	// Let the shells print their prompts (which may set a title)
	// before giving the first tab a Claude-style title.
	time.Sleep(400 * time.Millisecond)
	for _, tab := range sess.Tabs() {
		if tab.ID == claudeID {
			tab.SetTitle("✳ Claude Code")
		}
	}

	mc := dialMCP(t, mcpSock)
	defer mc.Close()
	var tabs []struct {
		ID    uint32 `json:"id"`
		Label string `json:"label"`
	}
	if err := json.Unmarshal(mc.call(t, 1, "tabs/list", nil), &tabs); err != nil {
		t.Fatalf("tabs/list: %v", err)
	}
	got := map[uint32]string{}
	for _, tab := range tabs {
		got[tab.ID] = tab.Label
	}
	if got[named.ID] != "build" {
		t.Errorf("named tab label = %q, want %q", got[named.ID], "build")
	}
	if got[claudeID] != "Claude Code" {
		t.Errorf("Claude tab label = %q, want %q", got[claudeID], "Claude Code")
	}
}
