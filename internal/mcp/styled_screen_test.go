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
	"github.com/LXXero/xerotty/internal/screentext"
)

// TestMCPStyledScreen covers the agent-visibility path for styled
// output: faint runs (TUI ghost-text suggestions) and colored runs
// (errors) must come back distinguishable from plain text, plus the
// cursor block. A flat-text agent reading Claude Code's UI used to
// mistake the dim autocomplete hint after the prompt for text the
// user actually typed — styled=true is the fix.
func TestMCPStyledScreen(t *testing.T) {
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
	if _, err := wc.Hello("styled-setup"); err != nil {
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
	mc.call(t, 1, "agent/mode", map[string]any{"mode": "auto"})
	// Faint "ghost" + red "ERR" via SGR, like a TUI would render.
	// The escapes are spelled \033 for printf to expand — sending
	// raw ESC bytes as INPUT doesn't survive the shell's line editor
	// (ZLE eats them as key-sequence prefixes).
	mc.call(t, 2, "tab/input", map[string]any{
		"tab_id": tabID,
		"bytes":  `printf 'typed \033[2mghost\033[0m \033[31mERR\033[0m\n'` + "\r",
	})

	type screen struct {
		Cols   uint16 `json:"cols"`
		Rows   uint16 `json:"rows"`
		Cursor struct {
			Row     int  `json:"row"`
			Col     int  `json:"col"`
			Visible bool `json:"visible"`
		} `json:"cursor"`
		Lines []string           `json:"lines"`
		Runs  [][]screentext.Run `json:"runs"`
	}

	var faint, red bool
	deadline := time.Now().Add(8 * time.Second)
	id := 10
	for time.Now().Before(deadline) && !(faint && red) {
		id++
		res := mc.call(t, id, "tab/screen", map[string]any{"tab_id": tabID, "styled": true})
		var sc screen
		if err := json.Unmarshal(res, &sc); err != nil {
			t.Fatalf("styled screen result: %v: %s", err, res)
		}
		if len(sc.Lines) != 0 {
			t.Fatalf("styled=true must not include flat lines")
		}
		for _, line := range sc.Runs {
			for _, run := range line {
				if run.Text == "ghost" && run.Attrs == "faint" {
					faint = true
				}
				// SGR 31 = red, palette index 1.
				if run.Text == "ERR" {
					if n, ok := run.Fg.(float64); ok && int(n) == 1 {
						red = true
					}
				}
			}
		}
		if !(faint && red) {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if !faint {
		t.Errorf("faint ghost run never appeared as its own styled run")
	}
	if !red {
		t.Errorf("red ERR run never appeared with fg=1")
	}

	// Flat mode still works and carries the cursor too.
	res := mc.call(t, 999, "tab/screen", map[string]any{"tab_id": tabID})
	var sc screen
	if err := json.Unmarshal(res, &sc); err != nil {
		t.Fatalf("flat screen result: %v: %s", err, res)
	}
	if len(sc.Lines) == 0 {
		t.Errorf("flat screen lost its lines")
	}
	if !sc.Cursor.Visible {
		t.Errorf("cursor should be visible on an idle shell")
	}
	if sc.Cursor.Row == 0 && sc.Cursor.Col == 0 {
		t.Errorf("cursor position never moved off origin — likely not wired")
	}
}
