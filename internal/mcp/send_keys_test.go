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
)

// TestMCPSendKeysSubmits is the "how do I press Enter" regression:
// tab/keys with {text, keys:["enter"]} must actually run the command
// — no JSON escape guessing, no incantations. Also checks an
// unknown token errors loudly with the vocabulary.
func TestMCPSendKeysSubmits(t *testing.T) {
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
