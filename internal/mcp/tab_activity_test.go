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
)

// TestMCPTabActivity checks that tabs/list surfaces the per-tab
// activity clock end-to-end: after real input+output, both absolute
// timestamps are populated and the ages are small; a distinct tab
// with no recent activity reads a larger output age (the stale case).
func TestMCPTabActivity(t *testing.T) {
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
	if _, err := wc.Hello("setup"); err != nil {
		t.Fatalf("hello: %v", err)
	}
	go wc.Run()
	if err := wc.Attach("", true); err != nil {
		t.Fatalf("attach: %v", err)
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

	// Let the shell settle, then age it a touch so "recent" is a real
	// (small) number, not exactly zero.
	time.Sleep(400 * time.Millisecond)

	// Drive genuine input + output.
	if err := wc.SendInput(tabID, []byte("echo hello\r")); err != nil {
		t.Fatalf("send input: %v", err)
	}
	time.Sleep(250 * time.Millisecond) // shell echoes + prints + reprompts

	mc := dialMCP(t, mcpSock)
	defer mc.Close()

	type row struct {
		ID              uint32 `json:"id"`
		LastOutput      string `json:"last_output"`
		LastInput       string `json:"last_input"`
		LastOutputAgeMs int64  `json:"last_output_age_ms"`
		LastInputAgeMs  int64  `json:"last_input_age_ms"`
	}
	var tabs []row
	if err := json.Unmarshal(mc.call(t, 1, "tabs/list", nil), &tabs); err != nil {
		t.Fatalf("tabs/list: %v", err)
	}
	if len(tabs) != 1 {
		t.Fatalf("want 1 tab, got %d", len(tabs))
	}
	r := tabs[0]
	if r.LastOutput == "" || r.LastInput == "" {
		t.Fatalf("timestamps not populated: %+v", r)
	}
	if _, err := time.Parse(time.RFC3339, r.LastOutput); err != nil {
		t.Fatalf("last_output not RFC3339: %q", r.LastOutput)
	}
	// Just typed + printed → both ages should be recent (< 5s) and
	// non-negative.
	if r.LastOutputAgeMs < 0 || r.LastOutputAgeMs > 5000 {
		t.Fatalf("last_output_age_ms = %d, want a small recent value", r.LastOutputAgeMs)
	}
	if r.LastInputAgeMs < 0 || r.LastInputAgeMs > 5000 {
		t.Fatalf("last_input_age_ms = %d, want a small recent value", r.LastInputAgeMs)
	}

	// Now let it sit idle and confirm the output age GROWS (the
	// staleness signal the whole feature exists for).
	firstAge := r.LastOutputAgeMs
	time.Sleep(600 * time.Millisecond)
	if err := json.Unmarshal(mc.call(t, 2, "tabs/list", nil), &tabs); err != nil {
		t.Fatalf("tabs/list 2: %v", err)
	}
	if tabs[0].LastOutputAgeMs <= firstAge {
		t.Fatalf("output age did not grow while idle: %d then %d", firstAge, tabs[0].LastOutputAgeMs)
	}
}
