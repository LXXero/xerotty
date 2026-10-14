package mcp_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/mcp"
	"github.com/LXXero/xerotty/internal/testutil"
)

// TestMCPSendKeysSubmits is the "how do I press Enter" regression:
// tab/keys with {text, keys:["enter"]} must actually run the command
// — no JSON escape guessing, no incantations. Also checks an
// unknown token errors loudly with the vocabulary.
func TestMCPSendKeysSubmits(t *testing.T) {
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
	wc.Hello("keys-setup")
	go wc.Run()
	wc.Attach("", true)
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

	// The output line "KEYS_OK_42" only exists if enter SUBMITTED the
	// command (the echoed command line says $((...)), not the result).
	mc.call(t, 2, "tab/keys", map[string]any{
		"tab_id": tabID,
		"text":   `echo "KEYS_OK_$((6 * 7))"`,
		"keys":   []string{"enter"},
	})

	found := false
	deadline := time.Now().Add(8 * time.Second)
	id := 10
	for time.Now().Before(deadline) && !found {
		id++
		res := mc.call(t, id, "tab/screen", map[string]any{"tab_id": tabID})
		var sc struct {
			Lines []string `json:"lines"`
		}
		if err := json.Unmarshal(res, &sc); err != nil {
			t.Fatalf("screen: %v", err)
		}
		for _, l := range sc.Lines {
			if strings.Contains(l, "KEYS_OK_42") && !strings.Contains(l, "$((") {
				found = true
			}
		}
		if !found {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if !found {
		t.Fatal("enter never submitted the command — KEYS_OK_42 output line missing")
	}

	// Unknown token: loud, self-correcting error.
	resp := mc.callRaw(t, 999, "tab/keys", map[string]any{
		"tab_id": tabID, "keys": []string{"bogus"},
	})
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "enter") {
		t.Fatalf("unknown token should error with vocabulary, got %+v", resp)
	}
}

// TestMCPSendKeysBracketedPaste: when the app in the tab turned on
// bracketed paste, tab/keys sends the text inside ESC[200~ ... ESC[201~
// with the keys after the end marker, in one write. Claude Code reads
// an unmarked burst as a paste, so without the markers a trailing
// enter became a newline instead of a submit. The tab runs a small
// reader that enables DECSET 2004 and hex-dumps the raw bytes it gets.
func TestMCPSendKeysBracketedPaste(t *testing.T) {
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
	wc.Hello("paste-setup")
	go wc.Run()
	wc.Attach("", true)
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

	// 6 + 2 + 6 + 1 = 15 bytes expected: start marker, "hi", end
	// marker, enter. Raw mode so the reader sees the CR as sent. The
	// reader prints PASTE_READY once it is listening ('RE''ADY' keeps
	// the echoed command line from matching); the shell's own prompt
	// may also enable 2004, so the mode alone doesn't mean it's ready.
	mc.call(t, 2, "tab/keys", map[string]any{
		"tab_id": tabID,
		"text":   `stty raw -echo; printf '\033[?2004hPASTE_RE''ADY\r\n'; dd bs=1 count=15 2>/dev/null | od -An -tx1 | tr -d ' \n'; printf '\033[?2004l'; stty sane; echo`,
		"keys":   []string{"enter"},
	})

	screenHas := func(id int, want string) bool {
		res := mc.call(t, id, "tab/screen", map[string]any{"tab_id": tabID})
		var sc struct {
			Lines []string `json:"lines"`
		}
		if err := json.Unmarshal(res, &sc); err != nil {
			t.Fatalf("screen: %v", err)
		}
		for _, l := range sc.Lines {
			if strings.Contains(l, want) {
				return true
			}
		}
		return false
	}

	term := d.SessionByName("default").Tab(tabID).Term
	deadline := time.Now().Add(8 * time.Second)
	id := 10
	for {
		id++
		if term.BracketedPasteMode() && screenHas(id, "PASTE_READY") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reader never started in bracketed-paste mode")
		}
		time.Sleep(50 * time.Millisecond)
	}

	mc.call(t, 3, "tab/keys", map[string]any{
		"tab_id": tabID,
		"text":   "hi",
		"keys":   []string{"enter"},
	})

	const want = "1b5b3230307e68691b5b3230317e0d"
	found := false
	for time.Now().Before(deadline) && !found {
		id++
		found = screenHas(id, want)
		if !found {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if !found {
		res := mc.call(t, 9999, "tab/screen", map[string]any{"tab_id": tabID})
		t.Fatalf("reader never got %s (marked text, then CR after the end marker); screen: %s", want, res)
	}
}
